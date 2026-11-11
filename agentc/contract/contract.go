package contract

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
)

// Lifecycle is called only by Control. Preparing a binding does not open admission.
type Lifecycle interface {
	Prepare(context.Context, *v1.PrepareRequest) (*v1.PrepareResponse, error)
	Inspect(context.Context, *v1.InspectRequest) (*v1.InspectResponse, error)
	SetAdmission(context.Context, *v1.SetAdmissionRequest) (*v1.SetAdmissionResponse, error)
	Drain(context.Context, *v1.DrainRequest) (*v1.DrainResponse, error)
	Stop(context.Context, *v1.StopRequest) (*v1.StopResponse, error)
}

type Executor interface {
	OpenThread(context.Context, *v1.OpenThreadRequest) (*v1.OpenThreadResponse, error)
	ReadThread(context.Context, *v1.ReadThreadRequest) (*v1.ReadThreadResponse, error)
	SubmitInput(context.Context, *v1.SubmitInputRequest) (*v1.SubmitInputResponse, error)
	GetInputResult(context.Context, *v1.GetInputResultRequest) (*v1.GetInputResultResponse, error)
	Cancel(context.Context, *v1.CancelRequest) (*v1.CancelResponse, error)
	RespondInteraction(context.Context, *v1.RespondInteractionRequest) (*v1.RespondInteractionResponse, error)
	// Subscribe is live only. Slow consumers must receive an error, never silent gaps.
	Subscribe(context.Context, *v1.SubscribeRequest, func(*v1.SubscribeResponse) error) error
}

type Agentc interface {
	Lifecycle
	Executor
}

// Error exposes only a safe message. Cause retains diagnostic detail for trusted callers.
type Error struct {
	Code, Source, NativeCode, RequestID, Message string
	Cause                                        error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func (e *Error) Unwrap() error { return e.Cause }
func NewError(code, message string) error {
	return &Error{Code: code, Source: "agentc", Message: message}
}
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "DependencyUnavailable"
}
func ValidateBinding(b *v1.Binding) error {
	if b == nil || b.Scope == "" || b.DeploymentId == "" || b.WorkspaceId == "" || b.InstanceId == "" || b.ReleaseId == "" || b.Epoch == 0 {
		return NewError("Invalid", "complete instance binding and nonzero epoch are required")
	}
	return nil
}
