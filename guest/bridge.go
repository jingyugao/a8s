//go:build linux

package guest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"path/filepath"
	"sync"

	agent "github.com/jingyugao/a8s/agentc/contract"
	av1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/state"
	"github.com/jingyugao/a8s/control/controllers"
	"google.golang.org/protobuf/proto"
)

type bridge struct {
	manager *Manager
	idle    *controllers.IdleController
	prepare sync.Mutex
}

func (b *bridge) validate(binding *av1.Binding) (Snapshot, error) {
	_, value := b.manager.Status()
	if err := agent.ValidateBinding(binding); err != nil {
		return value, err
	}
	if !proto.Equal(binding, value.Binding) {
		return value, agent.NewError("Conflict", "stale workspace or instance binding")
	}
	return value, nil
}
func (b *bridge) ready(value Snapshot) *av1.Ready {
	return &av1.Ready{Binding: value.Binding, Ready: value.Phase == "Running" || value.Phase == "Suspended", PackageDigest: b.manager.packageDigest, HostDigest: b.manager.hostDigest, AdmissionOpen: value.AdmissionOpen, Capabilities: map[string]string{"idle_reclaim": "supported", "live_events": "supported", "native_flush_proof": "unverified"}}
}
func (b *bridge) Prepare(ctx context.Context, req *av1.PrepareRequest) (*av1.PrepareResponse, error) {
	b.prepare.Lock()
	defer b.prepare.Unlock()
	if req == nil {
		return nil, agent.NewError("Invalid", "prepare request is required")
	}
	if err := agent.ValidateBinding(req.Binding); err != nil {
		return nil, err
	}
	m := b.manager
	ready, current := m.Status()
	if !ready {
		return nil, agent.NewError("NotReady", "Agent content is not warm")
	}
	if req.PackageDigest != m.packageDigest || req.HostDigest != m.hostDigest || req.Binding.DeploymentId != m.cfg.AgentName || filepath.Clean(req.WorkspaceDirectory) != m.cfg.Root {
		return nil, agent.NewError("Conflict", "prepared deployment differs from fixed Agent content or workspace root")
	}
	if current.Binding != nil && proto.Equal(current.Binding, req.Binding) {
		if current.Phase != "Running" && current.Phase != "Suspended" {
			return nil, agent.NewError("NotReady", "original instance transition is unconfirmed")
		}
		return &av1.PrepareResponse{Ready: b.ready(current)}, nil
	}
	hash := sha256.Sum256([]byte(req.Binding.Scope + "\x00" + req.Binding.WorkspaceId))
	workspace := "ws-" + hex.EncodeToString(hash[:16])
	uid := uint32(10000) + binary.BigEndian.Uint32(hash[16:20])%1000000000
	value, err := m.Start(ctx, StartRequest{WorkspaceID: workspace, UID: uid, Generation: req.Binding.Epoch, RequestKey: req.Binding.InstanceId, Binding: req.Binding, Managed: true})
	if err != nil {
		return nil, err
	}
	return &av1.PrepareResponse{Ready: b.ready(value)}, nil
}
func (b *bridge) Inspect(ctx context.Context, req *av1.InspectRequest) (*av1.InspectResponse, error) {
	value, err := b.validate(req.GetBinding())
	if err != nil {
		return nil, err
	}
	if value.Phase == "Running" {
		response, err := b.manager.client.Inspect(ctx, req)
		if err != nil {
			return nil, err
		}
		return response, nil
	}
	return &av1.InspectResponse{Ready: b.ready(value)}, nil
}
func (b *bridge) SetAdmission(ctx context.Context, req *av1.SetAdmissionRequest) (*av1.SetAdmissionResponse, error) {
	m := b.manager
	m.op.Lock()
	defer m.op.Unlock()
	value, err := b.validate(req.GetBinding())
	if err != nil {
		return nil, err
	}
	if req.Open && value.AdmissionClosed {
		return nil, agent.NewError("AdmissionClosed", "instance admission was permanently closed")
	}
	if value.AdmissionOpen == req.Open && (req.Open || value.AdmissionClosed) {
		return &av1.SetAdmissionResponse{Ready: b.ready(value)}, nil
	}
	if value.Phase == "Running" {
		if _, err := m.client.SetAdmission(ctx, req); err != nil {
			return nil, err
		}
	} else if value.Phase != "Suspended" {
		return nil, agent.NewError("NotReady", "workspace is transitioning")
	}
	value.AdmissionOpen = req.Open
	if !req.Open {
		value.AdmissionClosed = true
	}
	if err := m.save(value); err != nil {
		return nil, err
	}
	return &av1.SetAdmissionResponse{Ready: b.ready(value)}, nil
}
func (b *bridge) Drain(ctx context.Context, req *av1.DrainRequest) (*av1.DrainResponse, error) {
	value, err := b.validate(req.GetBinding())
	if err != nil {
		return nil, err
	}
	if value.Phase == "Suspended" {
		return &av1.DrainResponse{AdmissionClosed: !value.AdmissionOpen, ToolsObserved: true, Drained: !value.AdmissionOpen, NativeStatePersisted: value.HistoryFlushed}, nil
	}
	return b.manager.client.Drain(ctx, req)
}
func (b *bridge) Stop(ctx context.Context, req *av1.StopRequest) (*av1.StopResponse, error) {
	value, err := b.validate(req.GetBinding())
	if err != nil {
		return nil, err
	}
	if value.AdmissionOpen {
		return nil, agent.NewError("AdmissionClosed", "close admission before stopping")
	}
	if err := b.manager.Park(ctx); err != nil {
		return nil, err
	}
	return &av1.StopResponse{ProcessStopped: true, NativeStatePersisted: true, Scope: "instance"}, nil
}

func (b *bridge) execute(ctx context.Context, binding *av1.Binding, mutation bool, call func(context.Context) error) error {
	value, err := b.validate(binding)
	if err != nil {
		return err
	}
	if mutation && !value.AdmissionOpen {
		return agent.NewError("AdmissionClosed", "instance admission is closed")
	}
	done, err := b.idle.Begin(ctx, mutation)
	if err != nil {
		return err
	}
	defer done()
	return b.manager.Execute(ctx, value.Request.WorkspaceID, value.Request.Generation, call)
}

var _ agent.Agentc = (*bridge)(nil)

func (b *bridge) OpenThread(ctx context.Context, req *av1.OpenThreadRequest) (response *av1.OpenThreadResponse, err error) {
	err = b.execute(ctx, req.GetBinding(), true, func(ctx context.Context) error {
		var callErr error
		response, callErr = b.manager.client.OpenThread(ctx, req)
		return callErr
	})
	return
}

func (b *bridge) SubmitInput(ctx context.Context, req *av1.SubmitInputRequest) (response *av1.SubmitInputResponse, err error) {
	err = b.execute(ctx, req.GetBinding(), true, func(ctx context.Context) error {
		var callErr error
		response, callErr = b.manager.client.SubmitInput(ctx, req)
		return callErr
	})
	return
}

func (b *bridge) ReadThread(ctx context.Context, req *av1.ReadThreadRequest) (response *av1.ReadThreadResponse, err error) {
	value, err := b.validate(req.GetBinding())
	if err != nil {
		return nil, err
	}
	// A deliberate history read may wake the original instance once.
	if value.Phase == "Suspended" {
		done, err := b.idle.Begin(ctx, true)
		if err != nil {
			return nil, err
		}
		defer done()
	}
	err = b.execute(ctx, req.GetBinding(), false, func(ctx context.Context) error {
		var callErr error
		response, callErr = b.manager.client.ReadThread(ctx, req)
		return callErr
	})
	return
}

func (b *bridge) GetInputResult(ctx context.Context, req *av1.GetInputResultRequest) (response *av1.GetInputResultResponse, err error) {
	value, err := b.validate(req.GetBinding())
	if err != nil {
		return nil, err
	}
	if cached, err := state.LoadResult(value.RuntimeDirectory, req); err == nil {
		return cached, nil
	}
	// An uncached historical read needs native history once. Completed result
	// polls then use the derived cache, including while the runtime is sleeping.
	if value.Phase == "Suspended" {
		done, err := b.idle.Begin(ctx, true)
		if err != nil {
			return nil, err
		}
		defer done()
	}

	err = b.execute(ctx, req.GetBinding(), false, func(ctx context.Context) error {
		var callErr error
		response, callErr = b.manager.client.GetInputResult(ctx, req)
		return callErr
	})
	return
}

func (b *bridge) Cancel(ctx context.Context, req *av1.CancelRequest) (response *av1.CancelResponse, err error) {
	err = b.execute(ctx, req.GetBinding(), true, func(ctx context.Context) error {
		var callErr error
		response, callErr = b.manager.client.Cancel(ctx, req)
		return callErr
	})
	return
}

func (b *bridge) RespondInteraction(ctx context.Context, req *av1.RespondInteractionRequest) (response *av1.RespondInteractionResponse, err error) {
	err = b.execute(ctx, req.GetBinding(), true, func(ctx context.Context) error {
		var callErr error
		response, callErr = b.manager.client.RespondInteraction(ctx, req)
		return callErr
	})
	return
}

func (b *bridge) Subscribe(ctx context.Context, req *av1.SubscribeRequest, send func(*av1.SubscribeResponse) error) error {
	return b.execute(ctx, req.GetBinding(), false, func(ctx context.Context) error { return b.manager.client.Subscribe(ctx, req, send) })
}
