package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	agent "github.com/jingyugao/a8s/agentc/contract"
	av1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/contract/transport"
	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control"
	"github.com/jingyugao/a8s/control/controllers"
	"github.com/jingyugao/a8s/control/resources"
	"google.golang.org/protobuf/proto"
)

// A URL template routes to externally provisioned, dedicated Agentc processes.
// The server does not start a shared harness or manage an in-memory process map.
type agentAdapter struct {
	url, token, execURL, workspaceDirectory string
	client                                  *http.Client
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (a *agentAdapter) remote(instanceID string) *transport.Client {
	endpoint := strings.ReplaceAll(a.url, "{instance}", strings.ToLower(instanceID))
	return transport.NewClient(a.client, endpoint, 15*time.Second)

}

func binding(action control.Action) *av1.Binding {
	return &av1.Binding{Scope: api.ScopeKey(action.Scope), DeploymentId: action.DeploymentID, WorkspaceId: action.Workspace.Id, InstanceId: action.Instance.Id, ReleaseId: action.Instance.ReleaseId, Epoch: action.Instance.Epoch}
}

func agentError(err error) error {
	if err == nil {
		return nil
	}
	var failure *agent.Error
	if errors.As(err, &failure) {
		return &api.Error{Detail: &v1.ErrorDetail{Category: categoryByName(failure.Code), Message: failure.Message, Source: failure.Source, Code: failure.NativeCode, OperationId: failure.RequestID}, Cause: err}
	}
	category := v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		category = v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND
	case connect.CodeInvalidArgument:
		category = v1.ErrorCategory_ERROR_CATEGORY_INVALID
	case connect.CodeAborted:
		category = v1.ErrorCategory_ERROR_CATEGORY_CONFLICT
	case connect.CodeUnimplemented:
		category = v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED
	case connect.CodePermissionDenied, connect.CodeUnauthenticated:
		category = v1.ErrorCategory_ERROR_CATEGORY_FORBIDDEN
	case connect.CodeDeadlineExceeded, connect.CodeCanceled:
		category = v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN
	}
	return &api.Error{Detail: &v1.ErrorDetail{Category: category, Message: "Agentc call could not be confirmed; inspect the original binding", Source: "agentc", Code: connect.CodeOf(err).String()}, Cause: err}
}

func readyObservation(action control.Action, ready *av1.Ready) (*resources.Observation, error) {
	if ready == nil || !proto.Equal(ready.Binding, binding(action)) {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "Agentc returned a different instance binding")
	}
	var supported []string
	for capability, status := range ready.Capabilities {
		if status == "supported" {
			supported = append(supported, capability)
		}
	}
	return &resources.Observation{Found: true, Done: ready.Ready, Ready: ready.Ready, InstanceID: ready.Binding.InstanceId, WorkspaceID: ready.Binding.WorkspaceId, Epoch: ready.Binding.Epoch,
		Capabilities:  supported,
		Binding:       strings.ReplaceAll("agentc://"+api.ScopeKey(action.Scope)+"/"+ready.Binding.InstanceId, " ", ""),
		PackageDigest: ready.PackageDigest, AgentcDigest: ready.HostDigest,
		// Sandbox bytes are observed by Provider, not by the external Agentc host.
		SandboxDigest: action.Instance.SandboxDigest, AdmissionOpen: ready.AdmissionOpen, AdmissionClosed: !ready.AdmissionOpen,
		HistoryRetained: ready.Capabilities["native_flush_proof"] == "supported",
	}, nil
}

func (a *agentAdapter) Idempotent(step string) bool {
	return step == controllers.OpenAdmission || step == controllers.CloseAdmission || step == controllers.Drain
}

func (a *agentAdapter) Bind(ctx context.Context, action control.Action) (*resources.Observation, error) {
	if action.Release.Agentc.HostDigest == "" {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "Codex requires host_digest for executable bytes in addition to the OCI artifact digest")
	}
	execURL := strings.ReplaceAll(a.execURL, "{instance}", action.Instance.Id)
	if execURL == "" {
		ref, err := computeReference(action)
		if err != nil {
			return nil, err
		}
		execURL = ref.Endpoint
	}
	response, err := a.remote(action.Instance.Id).Prepare(ctx, &av1.PrepareRequest{Binding: binding(action), PackageDigest: action.Release.Package.Digest, HostDigest: action.Release.Agentc.HostDigest, ExecServerUrl: execURL, WorkspaceDirectory: a.workspaceDirectory})
	if err != nil {
		return nil, agentError(err)
	}
	return readyObservation(action, response.Ready)
}

func (a *agentAdapter) setAdmission(ctx context.Context, action control.Action, open bool) (*resources.Observation, error) {
	response, err := a.remote(action.Instance.Id).SetAdmission(ctx, &av1.SetAdmissionRequest{Binding: binding(action), Open: open})
	if err != nil {
		return nil, agentError(err)
	}
	return readyObservation(action, response.Ready)
}

func (a *agentAdapter) OpenAdmission(ctx context.Context, action control.Action) (*resources.Observation, error) {
	return a.setAdmission(ctx, action, true)
}
func (a *agentAdapter) CloseAdmission(ctx context.Context, action control.Action) (*resources.Observation, error) {
	return a.setAdmission(ctx, action, false)
}

func (a *agentAdapter) Drain(ctx context.Context, action control.Action) (*resources.Observation, error) {
	response, err := a.remote(action.Instance.Id).Drain(ctx, &av1.DrainRequest{Binding: binding(action)})
	if err != nil {
		return nil, agentError(err)
	}
	d := response
	return &resources.Observation{Found: true, Done: true, InstanceID: action.Instance.Id, WorkspaceID: action.Workspace.Id, Epoch: action.Instance.Epoch, AdmissionClosed: d.AdmissionClosed,
		Drained: d.Drained && d.ToolsObserved && d.AdmissionClosed, ActiveTurns: uint64(d.ActiveTurns), QueuedInputs: uint64(d.QueuedInputs), PendingApprovals: uint64(d.WaitingInteractions), ToolWriters: uint64(d.RunningTools), HistoryRetained: d.NativeStatePersisted}, nil
}

func (a *agentAdapter) Stop(ctx context.Context, action control.Action) (*resources.Observation, error) {
	response, err := a.remote(action.Instance.Id).Stop(ctx, &av1.StopRequest{Binding: binding(action)})
	if err != nil {
		return nil, agentError(err)
	}
	if response.Scope != "instance" && response.Scope != "instance process group only; remote workspace release requires Provider evidence" {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_READY, "Agentc stop did not confirm dedicated instance scope")
	}
	return &resources.Observation{Found: true, Done: true, InstanceID: action.Instance.Id, WorkspaceID: action.Workspace.Id, ProcessStopped: response.ProcessStopped, HistoryRetained: response.NativeStatePersisted}, nil
}

func (a *agentAdapter) Lookup(ctx context.Context, action control.Action) (*resources.Observation, error) {
	if action.Step == controllers.Drain {
		return a.Drain(ctx, action)
	}
	if action.Step == controllers.StopAgentc {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN, "Agentc does not yet expose durable stop-result lookup")
	}
	response, err := a.remote(action.Instance.Id).Inspect(ctx, &av1.InspectRequest{Binding: binding(action)})
	if err != nil {
		return nil, agentError(err)
	}
	result, err := readyObservation(action, response.Ready)
	if err != nil {
		return nil, err
	}
	if action.Step == controllers.OpenAdmission && !result.AdmissionOpen || action.Step == controllers.CloseAdmission && !result.AdmissionClosed {
		result.Found = false
	}
	return result, nil
}

func newAgentAdapter(endpoint, token, execURL, directory string) (*agentAdapter, error) {
	if !validEndpoint(strings.ReplaceAll(endpoint, "{instance}", "instance")) || directory == "" {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "Agentc URL and Workspace directory are required")
	}
	return &agentAdapter{url: endpoint, token: token, execURL: execURL, workspaceDirectory: directory, client: &http.Client{Transport: agentTokenTransport{base: http.DefaultTransport, token: token}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// HTTP authorization applies to unary and streaming RPCs alike. Unary deadlines
// are owned by transport.Client; subscriptions have the handler's stream limit.
type agentTokenTransport struct {
	base  http.RoundTripper
	token string
}

func (t agentTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	if t.token != "" {
		copy.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(copy)
}
