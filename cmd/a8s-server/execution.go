package main

import (
	"context"
	"strconv"

	agent "github.com/jingyugao/a8s/agentc/contract"
	av1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control"
	"github.com/jingyugao/a8s/execution"
)

type executionRouting struct {
	service *control.Service
	agents  *agentAdapter
	scope   *v1.Scope
}

var _ execution.TargetReader = (*executionRouting)(nil)
var _ execution.Resolver = (*executionRouting)(nil)
var _ execution.Authorizer = (*executionRouting)(nil)

func (e *executionRouting) Authorize(ctx context.Context, resource execution.Resource, action string) error {
	if resource.Scope != api.ScopeKey(e.scope) {
		return agent.NewError("Forbidden", "caller is not authorized for this scope")
	}
	if _, err := e.service.GetWorkspace(ctx, e.scope, resource.DeploymentID, resource.WorkspaceID); err != nil {
		return executionError(err)
	}
	return nil
}

func executionError(err error) error {
	if err == nil {
		return nil
	}
	detail := api.Detail(err)
	category := "DependencyUnavailable"
	switch detail.Category {
	case v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND:
		category = "NotFound"
	case v1.ErrorCategory_ERROR_CATEGORY_FORBIDDEN:
		category = "Forbidden"
	case v1.ErrorCategory_ERROR_CATEGORY_CONFLICT:
		category = "Conflict"
	case v1.ErrorCategory_ERROR_CATEGORY_INVALID:
		category = "Invalid"
	}
	return &agent.Error{Code: category, Source: "control", NativeCode: detail.Code, Message: detail.Message, Cause: err}
}

func (e *executionRouting) ReadExecutionTarget(ctx context.Context, resource execution.Resource) (execution.Target, error) {
	scope, err := api.ParseScope(resource.Scope)
	if err != nil {
		return execution.Target{}, executionError(err)
	}
	// One load ensures ownership and the release/epoch binding describe the same
	// committed view. Execution cannot choose a target from current health alone.
	state, err := e.service.Store.Load(ctx, scope, resource.DeploymentID)
	if err != nil {
		return execution.Target{}, executionError(err)
	}
	if state.Workspace.Id != resource.WorkspaceID {
		return execution.Target{}, agent.NewError("NotFound", "Workspace does not belong to this Deployment")
	}
	if !state.Target.AdmissionOpen {
		return execution.Target{}, agent.NewError("AdmissionClosed", "execution admission is closed; inspect the Deployment lifecycle operation")
	}
	instance := state.Instances[state.Target.InstanceId]
	if instance == nil {
		return execution.Target{}, agent.NewError("NotReady", "Control has no committed instance")
	}
	return execution.Target{Binding: &av1.Binding{Scope: resource.Scope, DeploymentId: resource.DeploymentID, WorkspaceId: resource.WorkspaceID, InstanceId: instance.Id, ReleaseId: instance.ReleaseId, Epoch: instance.Epoch}, AdmissionOpen: state.Target.AdmissionOpen, ViewVersion: strconv.FormatUint(state.Target.ViewVersion, 10)}, nil
}

func (e *executionRouting) Resolve(ctx context.Context, bound *av1.Binding) (agent.Executor, error) {
	if e.agents == nil {
		return nil, agent.NewError("Unsupported", "Agentc endpoint is not configured")
	}
	if err := agent.ValidateBinding(bound); err != nil {
		return nil, err
	}
	scope, err := api.ParseScope(bound.Scope)
	if err != nil {
		return nil, executionError(err)
	}
	state, err := e.service.Store.Load(ctx, scope, bound.DeploymentId)
	if err != nil {
		return nil, executionError(err)
	}
	instance := state.Instances[bound.InstanceId]
	if state.Workspace.Id != bound.WorkspaceId || instance == nil || instance.ReleaseId != bound.ReleaseId || instance.Epoch != bound.Epoch {
		return nil, agent.NewError("Conflict", "recorded execution target does not match the original binding")
	}
	return e.agents.remote(bound.InstanceId), nil
}
