package control

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"google.golang.org/protobuf/proto"
)

func (s *Service) GetDeployment(ctx context.Context, scope *v1.Scope, id string) (*v1.Deployment, error) {
	if err := validateKey(scope, id); err != nil {
		return nil, err
	}
	state, err := s.Store.Load(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	return state.Deployment, nil
}

func (s *Service) ListDeployments(ctx context.Context, req *v1.ListDeploymentsRequest) (*v1.ListDeploymentsResponse, error) {
	if err := validateKey(req.GetScope(), "list"); err != nil {
		return nil, err
	}
	limit := int(req.PageSize)
	if limit == 0 {
		limit = 50
	}
	if limit > 200 {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "page_size must be at most 200")
	}
	after := ""
	if req.PageToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(req.PageToken)
		prefix := api.ScopeKey(req.Scope) + ":"
		if err != nil || !strings.HasPrefix(string(decoded), prefix) {
			return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "page_token does not belong to this scope")
		}
		after = strings.TrimPrefix(string(decoded), prefix)
	}
	states, err := s.Store.List(ctx, req.Scope, after, limit+1)
	if err != nil {
		return nil, err
	}
	out := &v1.ListDeploymentsResponse{}
	if len(states) > limit {
		out.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(api.ScopeKey(req.Scope) + ":" + states[limit-1].Deployment.Id))
		states = states[:limit]
	}
	for _, state := range states {
		out.Deployments = append(out.Deployments, state.Deployment)
	}
	return out, nil
}

func (s *Service) GetOperation(ctx context.Context, scope *v1.Scope, deploymentID, id string) (*v1.Operation, error) {
	if err := validateKey(scope, deploymentID); err != nil {
		return nil, err
	}
	state, err := s.Store.Load(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	op := state.Operations[id]
	if op == nil {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND, "operation not found")
	}
	return op, nil
}

func (s *Service) GetRelease(ctx context.Context, scope *v1.Scope, deploymentID, id string) (*v1.Release, error) {
	if err := validateKey(scope, deploymentID); err != nil {
		return nil, err
	}
	state, err := s.Store.Load(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	release := state.Releases[id]
	if release == nil {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND, "release not found")
	}
	return release, nil
}

func (s *Service) GetWorkspace(ctx context.Context, scope *v1.Scope, deploymentID, id string) (*v1.Workspace, error) {
	if err := validateKey(scope, deploymentID); err != nil {
		return nil, err
	}
	state, err := s.Store.Load(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	if state.Workspace.Id != id {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND, "workspace not found for this deployment")
	}
	return state.Workspace, nil
}

// ReadExecutionTarget exposes the closed routing view as well as the current
// target so Execution can return AdmissionClosed with the lifecycle operation.
// The host must authorize scope and resource before invoking this internal port.
func (s *Service) ReadExecutionTarget(ctx context.Context, scope *v1.Scope, deploymentID, workspaceID string) (*v1.ExecutionTarget, error) {
	if err := validateKey(scope, deploymentID); err != nil {
		return nil, err
	}
	state, err := s.Store.Load(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	if workspaceID != "" && workspaceID != state.Workspace.Id {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND, "workspace does not belong to this deployment")
	}
	view := proto.Clone(state.Target).(*v1.ExecutionTarget)
	if !view.AdmissionOpen && view.Condition == nil {
		view.Condition = &v1.ErrorDetail{Category: v1.ErrorCategory_ERROR_CATEGORY_ADMISSION_CLOSED, Message: "execution admission is closed", OperationId: state.Deployment.OperationId}
	}
	return view, nil
}
