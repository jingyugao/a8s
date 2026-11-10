package sdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/api/gen/a8s/v1/a8sv1connect"
	"github.com/jingyugao/a8s/execution/gen/a8s/execution/v1/executionv1connect"
)

type Options struct {
	HTTPClient connect.HTTPClient
	// Headers inject caller-owned identity without importing product config.
	Headers        http.Header
	RequestTimeout time.Duration
	PollInterval   time.Duration
}

type Client struct {
	Control      a8sv1connect.ControlServiceClient
	Execution    executionv1connect.ExecutionServiceClient
	pollInterval time.Duration
}

// New creates generated Connect clients with bounded unary calls and no send
// retries. Mutation request keys always come from the caller's protobuf request.
func New(baseURL string, options Options) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("endpoint must be an http(s) URL without credentials, query, or fragment")
	}
	if options.RequestTimeout < 0 || options.PollInterval < 0 {
		return nil, errors.New("timeouts and poll interval must not be negative")
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 30 * time.Second
	}
	if options.PollInterval == 0 {
		options.PollInterval = time.Second
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{}
	}
	options.Headers = options.Headers.Clone()
	unary := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, options.RequestTimeout)
			defer cancel()
			for key, values := range options.Headers {
				req.Header()[key] = append([]string(nil), values...)
			}
			response, err := next(ctx, req)
			if err == nil {
				return response, nil
			}
			failure := &RequestError{Cause: err}
			if keyed, ok := req.Any().(interface{ GetRequestKey() string }); ok {
				failure.RequestKey = keyed.GetRequestKey()
			}
			if identified, ok := req.Any().(interface{ GetId() string }); ok {
				failure.ResourceID = identified.GetId()
			}
			if identified, ok := req.Any().(interface{ GetRequestId() string }); ok {
				failure.RequestID = identified.GetRequestId()
			}
			switch message := req.Any().(type) {
			case *v1.RegisterAgentcRequest:
				failure.ResourceID = message.GetAgentc().GetId()
			case *v1.CreateRuntimeRevisionRequest:
				failure.ResourceID = message.GetRuntimeRevision().GetId()
			}
			var connectError *connect.Error
			if errors.As(err, &connectError) {
				failure.Code = connectError.Code().String()
				failure.Category = connectError.Meta().Get("A8s-Error-Code")
				failure.Source = connectError.Meta().Get("A8s-Error-Source")
				failure.NativeCode = connectError.Meta().Get("A8s-Native-Code")
				if id := connectError.Meta().Get("A8s-Request-Id"); id != "" {
					failure.RequestID = id
				}
				for _, detail := range connectError.Details() {
					value, decodeErr := detail.Value()
					if decodeErr == nil {
						if public, ok := value.(*v1.ErrorDetail); ok {
							failure.Detail = public
							failure.OperationID = public.OperationId
							if failure.Category == "" {
								failure.Category = public.Category.String()
							}
							if failure.Source == "" {
								failure.Source = public.Source
							}
							if failure.NativeCode == "" {
								failure.NativeCode = public.Code
							}
							break
						}
					}
				}
			}
			failure.OutcomeUnknown = failure.Detail.GetCategory() == v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN || failure.Category == "OutcomeUnknown"
			if mutatingProcedure(req.Spec().Procedure) && (connect.CodeOf(err) == connect.CodeDeadlineExceeded || connect.CodeOf(err) == connect.CodeCanceled || connect.CodeOf(err) == connect.CodeUnavailable || connect.CodeOf(err) == connect.CodeUnknown) {
				failure.OutcomeUnknown = true
			}
			return response, failure
		}
	})
	interceptor := clientInterceptor{unary: unary, headers: options.Headers}
	return &Client{
		Control:      a8sv1connect.NewControlServiceClient(options.HTTPClient, strings.TrimRight(baseURL, "/"), connect.WithInterceptors(interceptor)),
		Execution:    executionv1connect.NewExecutionServiceClient(options.HTTPClient, strings.TrimRight(baseURL, "/"), connect.WithInterceptors(interceptor)),
		pollInterval: options.PollInterval,
	}, nil
}

func mutatingProcedure(procedure string) bool {
	name := procedure[strings.LastIndex(procedure, "/")+1:]
	switch name {
	case "RegisterAgentc", "CreateRuntimeRevision", "CreateDeployment", "UpdateDeployment", "SetDesiredState", "Rollback", "OpenThread", "SubmitInput", "Cancel", "RespondInteraction":
		return true
	default:
		return false
	}
}

type clientInterceptor struct {
	unary   connect.UnaryInterceptorFunc
	headers http.Header
}

func (i clientInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return i.unary.WrapUnary(next)
}
func (i clientInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		for key, values := range i.headers {
			conn.RequestHeader()[key] = append([]string(nil), values...)
		}
		return conn
	}
}
func (i clientInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// RequestError retains the request key and any server operation identity. Cause
// remains unwrap-able as a Connect or context error; no request body is logged.
type RequestError struct {
	RequestKey     string          `json:"requestKey,omitempty"`
	RequestID      string          `json:"requestId,omitempty"`
	ResourceID     string          `json:"resourceId,omitempty"`
	OperationID    string          `json:"operationId,omitempty"`
	Code           string          `json:"code,omitempty"`
	Category       string          `json:"category,omitempty"`
	Source         string          `json:"source,omitempty"`
	NativeCode     string          `json:"nativeCode,omitempty"`
	Detail         *v1.ErrorDetail `json:"detail,omitempty"`
	OutcomeUnknown bool            `json:"outcomeUnknown,omitempty"`
	Cause          error           `json:"-"`
}

func (e *RequestError) Error() string {
	if e.Detail != nil {
		return fmt.Sprintf("%s: %s", e.Detail.Category, e.Detail.Message)
	}
	return e.Cause.Error()
}
func (e *RequestError) Unwrap() error { return e.Cause }

type WaitError struct {
	Operation *v1.Operation
	Request   *v1.GetOperationRequest
	Cause     error
}

func (e *WaitError) Error() string {
	return fmt.Sprintf("stopped waiting for operation %s; server outcome is unchanged: %v", e.Request.GetId(), e.Cause)
}
func (e *WaitError) Unwrap() error { return e.Cause }

type OperationError struct{ Operation *v1.Operation }

func (e *OperationError) Error() string {
	return fmt.Sprintf("operation %s: %s (%s): %s", e.Operation.GetId(), e.Operation.GetStatus(), e.Operation.GetPhase(), e.Operation.GetError().GetMessage())
}

// WaitOperation returns the most recently observed operation even when a wait
// deadline, transport failure, Blocked, or Unknown result ends the wait. It never
// cancels the server operation or resubmits its mutation.
func (c *Client) WaitOperation(ctx context.Context, request *v1.GetOperationRequest, timeout time.Duration) (*v1.Operation, error) {
	if request == nil || request.Id == "" {
		return nil, errors.New("operation ID is required")
	}
	if timeout < 0 {
		return nil, errors.New("wait timeout must not be negative")
	}
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var latest *v1.Operation
	for {
		response, err := c.Control.GetOperation(ctx, connect.NewRequest(request))
		if err != nil {
			return latest, &WaitError{Operation: latest, Request: request, Cause: err}
		}
		latest = response.Msg.Operation
		if latest == nil {
			return latest, &WaitError{Operation: latest, Request: request, Cause: errors.New("server returned no operation")}
		}
		switch latest.Status {
		case v1.OperationStatus_OPERATION_STATUS_SUCCEEDED:
			return latest, nil
		case v1.OperationStatus_OPERATION_STATUS_FAILED, v1.OperationStatus_OPERATION_STATUS_BLOCKED, v1.OperationStatus_OPERATION_STATUS_UNKNOWN:
			return latest, &OperationError{Operation: latest}
		case v1.OperationStatus_OPERATION_STATUS_PENDING, v1.OperationStatus_OPERATION_STATUS_RUNNING:
		default:
			return latest, &WaitError{Operation: latest, Request: request, Cause: errors.New("unrecognized operation status")}
		}
		timer := time.NewTimer(c.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return latest, &WaitError{Operation: latest, Request: request, Cause: ctx.Err()}
		case <-timer.C:
		}
	}
}
