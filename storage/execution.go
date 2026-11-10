package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jingyugao/a8s/agentc/contract"
	"github.com/jingyugao/a8s/execution"
)

// ExecutionSQL uses PostgreSQL through the caller-owned database/sql connection.
// It never creates a database, runs migrations automatically, or stores input text.
type ExecutionSQL struct{ DB *sql.DB }

var _ execution.Store = (*ExecutionSQL)(nil)

const executionColumns = "request_id, request_key, kind, fingerprint, resource, target, state, result, version, created_at, updated_at"

type scanner interface{ Scan(...any) error }

func scanExecution(row scanner) (execution.Record, error) {
	var r execution.Record
	var resource, target, result []byte
	err := row.Scan(&r.ID, &r.Key, &r.Kind, &r.Fingerprint, &resource, &target, &r.State, &result, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, execution.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(resource, &r.Resource); err != nil {
		return r, err
	}
	if err := json.Unmarshal(target, &r.Target); err != nil {
		return r, err
	}
	r.Result = result
	return r, nil
}
func (s *ExecutionSQL) ByKey(ctx context.Context, r execution.Resource, kind, key string) (execution.Record, error) {
	return scanExecution(s.DB.QueryRowContext(ctx, "SELECT "+executionColumns+" FROM a8s_execution_requests WHERE scope=$1 AND deployment_id=$2 AND workspace_id=$3 AND kind=$4 AND request_key=$5", r.Scope, r.DeploymentID, r.WorkspaceID, kind, key))
}
func (s *ExecutionSQL) ByID(ctx context.Context, r execution.Resource, id string) (execution.Record, error) {
	return scanExecution(s.DB.QueryRowContext(ctx, "SELECT "+executionColumns+" FROM a8s_execution_requests WHERE scope=$1 AND deployment_id=$2 AND workspace_id=$3 AND request_id=$4", r.Scope, r.DeploymentID, r.WorkspaceID, id))
}
func (s *ExecutionSQL) Claim(ctx context.Context, r execution.Record) (execution.Record, bool, error) {
	resource, err := json.Marshal(r.Resource)
	if err != nil {
		return r, false, err
	}
	target, err := json.Marshal(r.Target)
	if err != nil {
		return r, false, err
	}
	row := s.DB.QueryRowContext(ctx, "INSERT INTO a8s_execution_requests (scope,deployment_id,workspace_id,"+executionColumns+") VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULL,1,$11,$12) ON CONFLICT (scope,deployment_id,workspace_id,kind,request_key) DO NOTHING RETURNING "+executionColumns, r.Resource.Scope, r.Resource.DeploymentID, r.Resource.WorkspaceID, r.ID, r.Key, r.Kind, r.Fingerprint, resource, target, r.State, r.CreatedAt, r.UpdatedAt)
	stored, err := scanExecution(row)
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, execution.ErrNotFound) {
		return r, false, err
	}
	stored, err = s.ByKey(ctx, r.Resource, r.Kind, r.Key)
	return stored, false, err
}
func (s *ExecutionSQL) Save(ctx context.Context, r execution.Record, expected int64) (execution.Record, error) {
	resource, err := json.Marshal(r.Resource)
	if err != nil {
		return r, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	target, err := json.Marshal(r.Target)
	if err != nil {
		return r, err
	}
	stored, err := scanExecution(tx.QueryRowContext(ctx, "UPDATE a8s_execution_requests SET resource=$1,state=$2,result=$3,version=version+1,updated_at=$4,target=CASE WHEN state='Rejected' THEN $10::jsonb ELSE target END WHERE request_id=$5 AND scope=$6 AND deployment_id=$7 AND workspace_id=$8 AND version=$9 RETURNING "+executionColumns, resource, r.State, []byte(r.Result), r.UpdatedAt, r.ID, r.Resource.Scope, r.Resource.DeploymentID, r.Resource.WorkspaceID, expected, target))
	if errors.Is(err, execution.ErrNotFound) {
		return r, execution.ErrConflict
	}
	if err != nil {
		return r, err
	}
	if r.Kind == "OpenThread" && r.State == "Accepted" && r.Resource.ThreadID != "" {
		_, err = tx.ExecContext(ctx, "INSERT INTO a8s_execution_threads (scope,deployment_id,workspace_id,thread_id,open_request_id) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (scope,thread_id) DO NOTHING", r.Resource.Scope, r.Resource.DeploymentID, r.Resource.WorkspaceID, r.Resource.ThreadID, r.ID)
		if err != nil {
			return r, err
		}
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT open_request_id FROM a8s_execution_threads WHERE scope=$1 AND thread_id=$2", r.Resource.Scope, r.Resource.ThreadID).Scan(&owner); err != nil {
			return r, err
		}
		if owner != r.ID {
			return r, contract.NewError("Conflict", "native thread already registered to another request")
		}
	}
	if err := tx.Commit(); err != nil {
		return r, fmt.Errorf("commit execution receipt: %w", err)
	}
	return stored, nil
}
func (s *ExecutionSQL) HasThread(ctx context.Context, r execution.Resource) (bool, error) {
	var found bool
	err := s.DB.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM a8s_execution_threads WHERE scope=$1 AND deployment_id=$2 AND workspace_id=$3 AND thread_id=$4)", r.Scope, r.DeploymentID, r.WorkspaceID, r.ThreadID).Scan(&found)
	return found, err
}
