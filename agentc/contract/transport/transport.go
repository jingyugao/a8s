// Package transport adapts the Agentc contract to Connect without a harness dependency.
package transport

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1/agentcv1connect"
)

// PublicError excludes the raw cause. Structured metadata preserves classification
// and original request association even when an RPC deadline has expired.
func PublicError(err error) error {
	if err == nil {
		return nil
	}
	code := connect.CodeUnavailable
	switch contract.Code(err) {
	case "Invalid":
		code = connect.CodeInvalidArgument
	case "Forbidden":
		code = connect.CodePermissionDenied
	case "NotFound":
		code = connect.CodeNotFound
	case "Conflict":
		code = connect.CodeAborted
	case "AdmissionClosed", "NotReady", "Incompatible":
		code = connect.CodeFailedPrecondition
	case "Unsupported":
		code = connect.CodeUnimplemented
	case "OutcomeUnknown":
		code = connect.CodeUnknown
	}
	var classified *contract.Error
	if !errors.As(err, &classified) {
		return connect.NewError(code, errors.New("dependency unavailable"))
	}
	public := connect.NewError(code, errors.New(classified.Message))
	public.Meta().Set("A8s-Error-Code", classified.Code)
	public.Meta().Set("A8s-Error-Source", classified.Source)
	if classified.NativeCode != "" {
		public.Meta().Set("A8s-Native-Code", classified.NativeCode)
	}
	if classified.RequestID != "" {
		public.Meta().Set("A8s-Request-Id", classified.RequestID)
	}
	return public
}

type server struct {
	agent   contract.Agentc
	timeout time.Duration
}

func NewHandler(agent contract.Agentc, timeout time.Duration, opts ...connect.HandlerOption) (string, http.Handler) {
	return agentcv1connect.NewAgentcServiceHandler(&server{agent, timeout}, opts...)
}
func unary[Q, R any](ctx context.Context, timeout time.Duration, q *connect.Request[Q], call func(context.Context, *Q) (*R, error)) (*connect.Response[R], error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := call(ctx, q.Msg)
	if err != nil {
		return nil, PublicError(err)
	}
	return connect.NewResponse(r), nil
}
func (s *server) Prepare(c context.Context, q *connect.Request[v1.PrepareRequest]) (*connect.Response[v1.PrepareResponse], error) {
	return unary(c, s.timeout, q, s.agent.Prepare)
}
func (s *server) Inspect(c context.Context, q *connect.Request[v1.InspectRequest]) (*connect.Response[v1.InspectResponse], error) {
	return unary(c, s.timeout, q, s.agent.Inspect)
}
func (s *server) SetAdmission(c context.Context, q *connect.Request[v1.SetAdmissionRequest]) (*connect.Response[v1.SetAdmissionResponse], error) {
	return unary(c, s.timeout, q, s.agent.SetAdmission)
}
func (s *server) Drain(c context.Context, q *connect.Request[v1.DrainRequest]) (*connect.Response[v1.DrainResponse], error) {
	return unary(c, s.timeout, q, s.agent.Drain)
}
func (s *server) Stop(c context.Context, q *connect.Request[v1.StopRequest]) (*connect.Response[v1.StopResponse], error) {
	return unary(c, s.timeout, q, s.agent.Stop)
}
func (s *server) OpenThread(c context.Context, q *connect.Request[v1.OpenThreadRequest]) (*connect.Response[v1.OpenThreadResponse], error) {
	return unary(c, s.timeout, q, s.agent.OpenThread)
}
func (s *server) ReadThread(c context.Context, q *connect.Request[v1.ReadThreadRequest]) (*connect.Response[v1.ReadThreadResponse], error) {
	return unary(c, s.timeout, q, s.agent.ReadThread)
}
func (s *server) SubmitInput(c context.Context, q *connect.Request[v1.SubmitInputRequest]) (*connect.Response[v1.SubmitInputResponse], error) {
	return unary(c, s.timeout, q, s.agent.SubmitInput)
}
func (s *server) GetInputResult(c context.Context, q *connect.Request[v1.GetInputResultRequest]) (*connect.Response[v1.GetInputResultResponse], error) {
	return unary(c, s.timeout, q, s.agent.GetInputResult)
}
func (s *server) Cancel(c context.Context, q *connect.Request[v1.CancelRequest]) (*connect.Response[v1.CancelResponse], error) {
	return unary(c, s.timeout, q, s.agent.Cancel)
}
func (s *server) RespondInteraction(c context.Context, q *connect.Request[v1.RespondInteractionRequest]) (*connect.Response[v1.RespondInteractionResponse], error) {
	return unary(c, s.timeout, q, s.agent.RespondInteraction)
}
func (s *server) Subscribe(c context.Context, q *connect.Request[v1.SubscribeRequest], stream *connect.ServerStream[v1.SubscribeResponse]) error {
	c, cancel := context.WithTimeout(c, 5*time.Minute)
	defer cancel()
	return PublicError(s.agent.Subscribe(c, q.Msg, stream.Send))
}

type Client struct {
	rpc     agentcv1connect.AgentcServiceClient
	timeout time.Duration
}

func NewClient(httpClient connect.HTTPClient, baseURL string, timeout time.Duration, opts ...connect.ClientOption) *Client {
	return &Client{agentcv1connect.NewAgentcServiceClient(httpClient, baseURL, opts...), timeout}
}
func remote[Q, R any](ctx context.Context, timeout time.Duration, q *Q, call func(context.Context, *connect.Request[Q]) (*connect.Response[R], error)) (*R, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := call(ctx, connect.NewRequest(q))
	if err != nil {
		return nil, remoteError(err)
	}
	return r.Msg, nil
}
func remoteError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Meta().Get("A8s-Error-Code") != "" {
		return &contract.Error{Code: ce.Meta().Get("A8s-Error-Code"), Source: ce.Meta().Get("A8s-Error-Source"), NativeCode: ce.Meta().Get("A8s-Native-Code"), RequestID: ce.Meta().Get("A8s-Request-Id"), Message: ce.Message(), Cause: err}
	}
	return &contract.Error{Code: "OutcomeUnknown", Source: "agentc.transport", Message: "remote result is unknown; query the original request", Cause: err}
}
func (c *Client) Prepare(x context.Context, q *v1.PrepareRequest) (*v1.PrepareResponse, error) {
	return remote(x, c.timeout, q, c.rpc.Prepare)
}
func (c *Client) Inspect(x context.Context, q *v1.InspectRequest) (*v1.InspectResponse, error) {
	return remote(x, c.timeout, q, c.rpc.Inspect)
}
func (c *Client) SetAdmission(x context.Context, q *v1.SetAdmissionRequest) (*v1.SetAdmissionResponse, error) {
	return remote(x, c.timeout, q, c.rpc.SetAdmission)
}
func (c *Client) Drain(x context.Context, q *v1.DrainRequest) (*v1.DrainResponse, error) {
	return remote(x, c.timeout, q, c.rpc.Drain)
}
func (c *Client) Stop(x context.Context, q *v1.StopRequest) (*v1.StopResponse, error) {
	return remote(x, c.timeout, q, c.rpc.Stop)
}
func (c *Client) OpenThread(x context.Context, q *v1.OpenThreadRequest) (*v1.OpenThreadResponse, error) {
	return remote(x, c.timeout, q, c.rpc.OpenThread)
}
func (c *Client) ReadThread(x context.Context, q *v1.ReadThreadRequest) (*v1.ReadThreadResponse, error) {
	return remote(x, c.timeout, q, c.rpc.ReadThread)
}
func (c *Client) SubmitInput(x context.Context, q *v1.SubmitInputRequest) (*v1.SubmitInputResponse, error) {
	return remote(x, c.timeout, q, c.rpc.SubmitInput)
}
func (c *Client) GetInputResult(x context.Context, q *v1.GetInputResultRequest) (*v1.GetInputResultResponse, error) {
	return remote(x, c.timeout, q, c.rpc.GetInputResult)
}
func (c *Client) Cancel(x context.Context, q *v1.CancelRequest) (*v1.CancelResponse, error) {
	return remote(x, c.timeout, q, c.rpc.Cancel)
}
func (c *Client) RespondInteraction(x context.Context, q *v1.RespondInteractionRequest) (*v1.RespondInteractionResponse, error) {
	return remote(x, c.timeout, q, c.rpc.RespondInteraction)
}
func (c *Client) Subscribe(x context.Context, q *v1.SubscribeRequest, send func(*v1.SubscribeResponse) error) error {
	stream, err := c.rpc.Subscribe(x, connect.NewRequest(q))
	if err != nil {
		return remoteError(err)
	}
	defer stream.Close()
	for stream.Receive() {
		if err := send(stream.Msg()); err != nil {
			return err
		}
	}
	return remoteErrorOrNil(stream.Err())
}
func remoteErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return remoteError(err)
}

var _ contract.Agentc = (*Client)(nil)
