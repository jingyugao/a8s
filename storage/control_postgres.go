package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control/resources"
	"google.golang.org/protobuf/proto"
)

type ControlPostgres struct{ pool *pgxpool.Pool }

var _ resources.Store = (*ControlPostgres)(nil)

func NewControlPostgres(pool *pgxpool.Pool) (*ControlPostgres, error) {
	if pool == nil {
		return nil, errors.New("Control PostgreSQL pool is required")
	}
	return &ControlPostgres{pool: pool}, nil
}

// CheckSchema is read-only; migrations are an explicit administrator action.
func (s *ControlPostgres) CheckSchema(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT d.version, d.pending, d.workspace_id, d.storage_reference, d.touched_at, d.payload, c.kind, c.payload
FROM a8s_control_deployments d CROSS JOIN a8s_control_components c LIMIT 0`)
	if err != nil {
		return databaseError(err)
	}
	rows.Close()
	return nil
}

func databaseError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND, "resource not found in this scope")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "resource, Workspace or storage identity is already owned")
	}
	e := api.NewError(v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE, "Control database unavailable")
	e.Detail.Source = "postgres"
	if pgErr != nil {
		e.Detail.Code = pgErr.Code
	}
	e.Cause = err
	return e
}

func (s *ControlPostgres) Create(ctx context.Context, state *resources.State) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	d := state.Deployment
	_, err = s.pool.Exec(ctx, `INSERT INTO a8s_control_deployments
(tenant_id, project_id, id, workspace_id, storage_reference, version, pending, payload)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, d.Scope.TenantId, d.Scope.ProjectId, d.Id, state.Workspace.Id, state.Workspace.StorageReference, d.ResourceVersion, resources.HasWork(state), payload)
	if err != nil {
		return databaseError(err)
	}
	return nil
}

func (s *ControlPostgres) Load(ctx context.Context, scope *v1.Scope, id string) (*resources.State, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM a8s_control_deployments WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, scope.GetTenantId(), scope.GetProjectId(), id).Scan(&payload)
	if err != nil {
		return nil, databaseError(err)
	}
	var state resources.State
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, fmt.Errorf("decode Control deployment: %w", err)
	}
	return &state, nil
}

func (s *ControlPostgres) Save(ctx context.Context, state *resources.State, expected uint64) error {
	if state.Deployment.ResourceVersion != expected+1 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "invalid next resource_version")
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	d := state.Deployment
	command, err := s.pool.Exec(ctx, `UPDATE a8s_control_deployments SET
version=$1, pending=$2, payload=$3, storage_reference=$4, touched_at=clock_timestamp()
WHERE tenant_id=$5 AND project_id=$6 AND id=$7 AND version=$8 AND workspace_id=$9`, d.ResourceVersion, resources.HasWork(state), payload, state.Workspace.StorageReference, d.Scope.TenantId, d.Scope.ProjectId, d.Id, expected, state.Workspace.Id)
	if err != nil {
		return databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "deployment resource_version changed; reload")
	}
	return nil
}

func (s *ControlPostgres) List(ctx context.Context, scope *v1.Scope, after string, limit int) ([]*resources.State, error) {
	rows, err := s.pool.Query(ctx, `SELECT payload FROM a8s_control_deployments WHERE tenant_id=$1 AND project_id=$2 AND id>$3 ORDER BY id LIMIT $4`, scope.GetTenantId(), scope.GetProjectId(), after, limit)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []*resources.State
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, databaseError(err)
		}
		var state resources.State
		if err := json.Unmarshal(payload, &state); err != nil {
			return nil, err
		}
		out = append(out, &state)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	return out, nil
}

func (s *ControlPostgres) Pending(ctx context.Context, scope *v1.Scope, limit int) ([]resources.Key, error) {
	query := `SELECT tenant_id, project_id, id FROM a8s_control_deployments WHERE pending ORDER BY touched_at, tenant_id, project_id, id LIMIT $1`
	args := []any{limit}
	if scope != nil {
		query = `SELECT tenant_id, project_id, id FROM a8s_control_deployments WHERE pending AND tenant_id=$2 AND project_id=$3 ORDER BY touched_at, tenant_id, project_id, id LIMIT $1`
		args = append(args, scope.TenantId, scope.ProjectId)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var out []resources.Key
	for rows.Next() {
		key := resources.Key{Scope: &v1.Scope{}}
		if err := rows.Scan(&key.Scope.TenantId, &key.Scope.ProjectId, &key.ID); err != nil {
			return nil, databaseError(err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	return out, nil
}

func (s *ControlPostgres) putComponent(ctx context.Context, scope *v1.Scope, kind, id string, message proto.Message) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	command, err := s.pool.Exec(ctx, `INSERT INTO a8s_control_components (tenant_id,project_id,kind,id,payload) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, scope.TenantId, scope.ProjectId, kind, id, payload)
	if err != nil {
		return databaseError(err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var current []byte
	if err := s.pool.QueryRow(ctx, `SELECT payload FROM a8s_control_components WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND id=$4`, scope.TenantId, scope.ProjectId, kind, id).Scan(&current); err != nil {
		return databaseError(err)
	}
	previous := message.ProtoReflect().New().Interface()
	if err := json.Unmarshal(current, previous); err != nil {
		return err
	}
	if !proto.Equal(previous, message) {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "immutable component id was registered with different content")
	}
	return nil
}

func (s *ControlPostgres) getComponent(ctx context.Context, scope *v1.Scope, kind, id string, message proto.Message) error {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM a8s_control_components WHERE tenant_id=$1 AND project_id=$2 AND kind=$3 AND id=$4`, scope.GetTenantId(), scope.GetProjectId(), kind, id).Scan(&payload)
	if err != nil {
		return databaseError(err)
	}
	return json.Unmarshal(payload, message)
}

func (s *ControlPostgres) PutAgentc(ctx context.Context, a *v1.AgentcRegistration) error {
	return s.putComponent(ctx, a.Scope, "agentc", a.Id, a)
}
func (s *ControlPostgres) GetAgentc(ctx context.Context, scope *v1.Scope, id string) (*v1.AgentcRegistration, error) {
	a := &v1.AgentcRegistration{}
	err := s.getComponent(ctx, scope, "agentc", id, a)
	return a, err
}
func (s *ControlPostgres) PutRuntime(ctx context.Context, r *v1.RuntimeRevision) error {
	return s.putComponent(ctx, r.Scope, "runtime", r.Id, r)
}
func (s *ControlPostgres) GetRuntime(ctx context.Context, scope *v1.Scope, id string) (*v1.RuntimeRevision, error) {
	r := &v1.RuntimeRevision{}
	err := s.getComponent(ctx, scope, "runtime", id, r)
	return r, err
}
