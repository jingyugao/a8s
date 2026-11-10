package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"github.com/jingyugao/a8s/agentc/contract"
	agentv1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/contract/transport"
	v1 "github.com/jingyugao/a8s/execution/gen/a8s/execution/v1"
	"github.com/jingyugao/a8s/execution/gen/a8s/execution/v1/executionv1connect"
)

type Handler struct{ Gateway *Gateway }

func NewHandler(g *Gateway, opts ...connect.HandlerOption) (string, http.Handler) {
	return executionv1connect.NewExecutionServiceHandler(&Handler{g}, opts...)
}
func resource(r *v1.Resource) Resource {
	if r == nil {
		return Resource{}
	}
	return Resource{Scope: r.Scope, DeploymentID: r.DeploymentId, WorkspaceID: r.WorkspaceId, ThreadID: r.ThreadId}
}
func record(r Record) *v1.Record {
	result := &v1.Record{RequestId: r.ID, State: r.State, ThreadId: r.Resource.ThreadID}
	var native agentv1.SubmitInputResponse
	if json.Unmarshal(r.Result, &native) == nil && native.Result != nil {
		result.TurnId = native.Result.TurnId
	}
	return result
}
func publicError(err error) error {
	if errors.Is(err, ErrNotFound) {
		err = contract.NewError("NotFound", "execution record does not exist in this resource scope")
	}
	if errors.Is(err, ErrConflict) {
		err = contract.NewError("Conflict", "execution record changed concurrently")
	}
	return transport.PublicError(err)
}
func (h *Handler) OpenThread(ctx context.Context, q *connect.Request[v1.OpenThreadRequest]) (*connect.Response[v1.OpenThreadResponse], error) {
	r, err := h.Gateway.OpenThread(ctx, resource(q.Msg.Resource), q.Msg.RequestKey)
	if err != nil {
		return nil, publicError(err)
	}
	return connect.NewResponse(&v1.OpenThreadResponse{Record: record(r)}), nil
}
func (h *Handler) SubmitInput(ctx context.Context, q *connect.Request[v1.SubmitInputRequest]) (*connect.Response[v1.SubmitInputResponse], error) {
	r, err := h.Gateway.SubmitInput(ctx, SubmitRequest{Resource: resource(q.Msg.Resource), RequestKey: q.Msg.RequestKey, Text: q.Msg.Text, AuthorID: q.Msg.AuthorId})
	if err != nil {
		return nil, publicError(err)
	}
	return connect.NewResponse(&v1.SubmitInputResponse{Record: record(r)}), nil
}
func (h *Handler) GetInputResult(ctx context.Context, q *connect.Request[v1.GetInputResultRequest]) (*connect.Response[v1.GetInputResultResponse], error) {
	r, err := h.Gateway.GetInputResult(ctx, resource(q.Msg.Resource), q.Msg.RequestId)
	if err != nil {
		return nil, publicError(err)
	}
	return connect.NewResponse(&v1.GetInputResultResponse{Result: r}), nil
}
func (h *Handler) Cancel(ctx context.Context, q *connect.Request[v1.CancelRequest]) (*connect.Response[v1.CancelResponse], error) {
	r, err := h.Gateway.Cancel(ctx, CommandRequest{Resource: resource(q.Msg.Resource), RequestKey: q.Msg.RequestKey, TurnID: q.Msg.TurnId}, q.Msg.InputRequestId)
	if err != nil {
		return nil, publicError(err)
	}
	return connect.NewResponse(&v1.CancelResponse{Record: record(r)}), nil
}
func (h *Handler) RespondInteraction(ctx context.Context, q *connect.Request[v1.RespondInteractionRequest]) (*connect.Response[v1.RespondInteractionResponse], error) {
	r, err := h.Gateway.RespondInteraction(ctx, CommandRequest{Resource: resource(q.Msg.Resource), RequestKey: q.Msg.RequestKey, TurnID: q.Msg.TurnId, InteractionID: q.Msg.InteractionId, Decision: q.Msg.Decision}, q.Msg.InputRequestId)
	if err != nil {
		return nil, publicError(err)
	}
	return connect.NewResponse(&v1.RespondInteractionResponse{Record: record(r)}), nil
}
func (h *Handler) Subscribe(ctx context.Context, q *connect.Request[v1.SubscribeRequest], stream *connect.ServerStream[v1.SubscribeResponse]) error {
	return publicError(h.Gateway.Subscribe(ctx, resource(q.Msg.Resource), q.Msg.InputRequestId, func(e *agentv1.SubscribeResponse) error { return stream.Send(&v1.SubscribeResponse{Event: e}) }))
}
func (h *Handler) Lookup(ctx context.Context, q *connect.Request[v1.LookupRequest]) (*connect.Response[v1.LookupResponse], error) {
	r, err := h.Gateway.Lookup(ctx, resource(q.Msg.Resource), q.Msg.Kind, q.Msg.RequestKey)
	if err != nil {
		return nil, publicError(err)
	}
	return connect.NewResponse(&v1.LookupResponse{Record: record(r)}), nil
}
