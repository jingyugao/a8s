package control

import (
	"connectrpc.com/connect"
	"context"
	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/api/gen/a8s/v1/a8sv1connect"
	"github.com/jingyugao/a8s/control/resources"
	"net/http"
	"time"
)

// Authorizer resolves the authenticated principal from context and checks both
// resource scope and the requested action. A missing authorizer denies access.
type Authorizer func(context.Context, *v1.Scope, string, string) error

type Handler struct {
	Service   *Service
	Authorize Authorizer
}

var _ a8sv1connect.ControlServiceHandler = (*Handler)(nil)

func NewHandler(service *Service, authorize Authorizer) (string, http.Handler, error) {
	if service == nil || service.Store == nil || authorize == nil {
		return "", nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "Control service, store and authorizer are required")
	}
	deadline := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			return next(ctx, req)
		}
	})
	path, handler := a8sv1connect.NewControlServiceHandler(&Handler{Service: service, Authorize: authorize}, connect.WithReadMaxBytes(1<<20), connect.WithInterceptors(deadline))
	return path, handler, nil
}

func (h *Handler) check(ctx context.Context, scope *v1.Scope, action, id string) error {
	if err := resources.ValidateScope(scope); err != nil {
		return rpcError(err)
	}
	if h.Authorize == nil {
		return rpcError(api.NewError(v1.ErrorCategory_ERROR_CATEGORY_FORBIDDEN, "caller identity is required"))
	}
	return rpcError(h.Authorize(ctx, scope, action, id))
}

func rpcError(err error) error {
	if err == nil {
		return nil
	}
	detail := api.Detail(err)
	code := connect.CodeUnavailable
	switch detail.Category {
	case v1.ErrorCategory_ERROR_CATEGORY_INVALID, v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE:
		code = connect.CodeInvalidArgument
	case v1.ErrorCategory_ERROR_CATEGORY_FORBIDDEN:
		code = connect.CodePermissionDenied
	case v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND:
		code = connect.CodeNotFound
	case v1.ErrorCategory_ERROR_CATEGORY_CONFLICT:
		code = connect.CodeAborted
	case v1.ErrorCategory_ERROR_CATEGORY_NOT_READY, v1.ErrorCategory_ERROR_CATEGORY_ADMISSION_CLOSED:
		code = connect.CodeFailedPrecondition
	case v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED:
		code = connect.CodeUnimplemented
	case v1.ErrorCategory_ERROR_CATEGORY_CURSOR_EXPIRED:
		code = connect.CodeOutOfRange
	}
	failure := connect.NewError(code, api.NewError(detail.Category, detail.Message))
	if metadata, addErr := connect.NewErrorDetail(detail); addErr == nil {
		failure.AddDetail(metadata)
	}
	return failure
}

func (h *Handler) RegisterAgentc(ctx context.Context, req *connect.Request[v1.RegisterAgentcRequest]) (*connect.Response[v1.RegisterAgentcResponse], error) {
	if err := h.check(ctx, req.Msg.GetAgentc().GetScope(), "RegisterAgentc", req.Msg.GetAgentc().GetId()); err != nil {
		return nil, err
	}
	value, err := h.Service.RegisterAgentc(ctx, req.Msg.Agentc)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.RegisterAgentcResponse{Agentc: value}), nil
}

func (h *Handler) GetAgentc(ctx context.Context, req *connect.Request[v1.GetAgentcRequest]) (*connect.Response[v1.GetAgentcResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "GetAgentc", req.Msg.Id); err != nil {
		return nil, err
	}
	value, err := h.Service.Store.GetAgentc(ctx, req.Msg.Scope, req.Msg.Id)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.GetAgentcResponse{Agentc: value}), nil
}

func (h *Handler) CreateRuntimeRevision(ctx context.Context, req *connect.Request[v1.CreateRuntimeRevisionRequest]) (*connect.Response[v1.CreateRuntimeRevisionResponse], error) {
	if err := h.check(ctx, req.Msg.GetRuntimeRevision().GetScope(), "CreateRuntimeRevision", req.Msg.GetRuntimeRevision().GetId()); err != nil {
		return nil, err
	}
	value, err := h.Service.CreateRuntimeRevision(ctx, req.Msg.RuntimeRevision)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.CreateRuntimeRevisionResponse{RuntimeRevision: value}), nil
}

func (h *Handler) GetRuntimeRevision(ctx context.Context, req *connect.Request[v1.GetRuntimeRevisionRequest]) (*connect.Response[v1.GetRuntimeRevisionResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "GetRuntimeRevision", req.Msg.Id); err != nil {
		return nil, err
	}
	value, err := h.Service.Store.GetRuntime(ctx, req.Msg.Scope, req.Msg.Id)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.GetRuntimeRevisionResponse{RuntimeRevision: value}), nil
}

func (h *Handler) GetDeployment(ctx context.Context, req *connect.Request[v1.GetDeploymentRequest]) (*connect.Response[v1.GetDeploymentResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "GetDeployment", req.Msg.Id); err != nil {
		return nil, err
	}
	value, err := h.Service.GetDeployment(ctx, req.Msg.Scope, req.Msg.Id)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.GetDeploymentResponse{Deployment: value}), nil
}

func (h *Handler) GetWorkspace(ctx context.Context, req *connect.Request[v1.GetWorkspaceRequest]) (*connect.Response[v1.GetWorkspaceResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "GetWorkspace", req.Msg.DeploymentId); err != nil {
		return nil, err
	}
	value, err := h.Service.GetWorkspace(ctx, req.Msg.Scope, req.Msg.DeploymentId, req.Msg.Id)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.GetWorkspaceResponse{Workspace: value}), nil
}

func (h *Handler) GetRelease(ctx context.Context, req *connect.Request[v1.GetReleaseRequest]) (*connect.Response[v1.GetReleaseResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "GetRelease", req.Msg.DeploymentId); err != nil {
		return nil, err
	}
	value, err := h.Service.GetRelease(ctx, req.Msg.Scope, req.Msg.DeploymentId, req.Msg.Id)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.GetReleaseResponse{Release: value}), nil
}

func (h *Handler) GetOperation(ctx context.Context, req *connect.Request[v1.GetOperationRequest]) (*connect.Response[v1.GetOperationResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "GetOperation", req.Msg.DeploymentId); err != nil {
		return nil, err
	}
	value, err := h.Service.GetOperation(ctx, req.Msg.Scope, req.Msg.DeploymentId, req.Msg.Id)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.GetOperationResponse{Operation: value}), nil
}

func (h *Handler) ReadExecutionTarget(ctx context.Context, req *connect.Request[v1.ReadExecutionTargetRequest]) (*connect.Response[v1.ReadExecutionTargetResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "ReadExecutionTarget", req.Msg.DeploymentId); err != nil {
		return nil, err
	}
	value, err := h.Service.ReadExecutionTarget(ctx, req.Msg.Scope, req.Msg.DeploymentId, req.Msg.WorkspaceId)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.ReadExecutionTargetResponse{Target: value}), nil
}

func (h *Handler) CreateDeployment(ctx context.Context, req *connect.Request[v1.CreateDeploymentRequest]) (*connect.Response[v1.CreateDeploymentResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "CreateDeployment", req.Msg.Id); err != nil {
		return nil, err
	}
	deployment, operation, err := h.Service.CreateDeployment(ctx, req.Msg)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.CreateDeploymentResponse{Deployment: deployment, Operation: operation}), nil
}

func (h *Handler) UpdateDeployment(ctx context.Context, req *connect.Request[v1.UpdateDeploymentRequest]) (*connect.Response[v1.UpdateDeploymentResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "UpdateDeployment", req.Msg.Id); err != nil {
		return nil, err
	}
	deployment, operation, err := h.Service.UpdateDeployment(ctx, req.Msg)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.UpdateDeploymentResponse{Deployment: deployment, Operation: operation}), nil
}

func (h *Handler) SetDesiredState(ctx context.Context, req *connect.Request[v1.SetDesiredStateRequest]) (*connect.Response[v1.SetDesiredStateResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "SetDesiredState", req.Msg.Id); err != nil {
		return nil, err
	}
	deployment, operation, err := h.Service.SetDesiredState(ctx, req.Msg)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.SetDesiredStateResponse{Deployment: deployment, Operation: operation}), nil
}

func (h *Handler) Rollback(ctx context.Context, req *connect.Request[v1.RollbackRequest]) (*connect.Response[v1.RollbackResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "Rollback", req.Msg.Id); err != nil {
		return nil, err
	}
	deployment, operation, err := h.Service.Rollback(ctx, req.Msg)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(&v1.RollbackResponse{Deployment: deployment, Operation: operation}), nil
}

func (h *Handler) ListDeployments(ctx context.Context, req *connect.Request[v1.ListDeploymentsRequest]) (*connect.Response[v1.ListDeploymentsResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "ListDeployments", ""); err != nil {
		return nil, err
	}
	value, err := h.Service.ListDeployments(ctx, req.Msg)
	if err != nil {
		return nil, rpcError(err)
	}
	return connect.NewResponse(value), nil
}

func (h *Handler) RequestWake(ctx context.Context, req *connect.Request[v1.RequestWakeRequest]) (*connect.Response[v1.RequestWakeResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "RequestWake", req.Msg.DeploymentId); err != nil {
		return nil, err
	}
	return nil, rpcError(api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "RequestWake is not supported in the first release"))
}

func (h *Handler) DeleteWorkspace(ctx context.Context, req *connect.Request[v1.DeleteWorkspaceRequest]) (*connect.Response[v1.DeleteWorkspaceResponse], error) {
	if err := h.check(ctx, req.Msg.Scope, "DeleteWorkspace", req.Msg.DeploymentId); err != nil {
		return nil, err
	}
	return nil, rpcError(api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "DeleteWorkspace is not supported in the first release"))
}
