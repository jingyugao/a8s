package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control"
	"github.com/jingyugao/a8s/control/ownership"
	"github.com/jingyugao/a8s/control/releases"
	"github.com/jingyugao/a8s/control/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	Prepare        = "prepare"
	Bind           = "bind"
	OpenAdmission  = "open-admission"
	CloseAdmission = "close-admission"
	Drain          = "drain"
	StopAgentc     = "stop-agentc"
	StopCompute    = "stop-compute"
	ReleaseCompute = "release-compute"
)

type Reconciler struct {
	Store resources.Store
	// Scope optionally limits a single-scope host's background authority.
	Scope        *v1.Scope
	Dependencies control.Dependencies
	CallTimeout  time.Duration
}

var errConcurrent = errors.New("concurrent Control advancement")

func (r *Reconciler) save(ctx context.Context, state *resources.State, op *v1.Operation) error {
	expected := state.Deployment.ResourceVersion
	state.Deployment.ResourceVersion++
	state.Target.ViewVersion = state.Deployment.ResourceVersion
	op.UpdatedAt = timestamppb.Now()
	err := r.Store.Save(ctx, state, expected)
	if api.IsCategory(err, v1.ErrorCategory_ERROR_CATEGORY_CONFLICT) {
		return fmt.Errorf("%w: %w", errConcurrent, err)
	}
	return err
}

func unavailable(message string) error {
	return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, message)
}
func blocked(message string) error {
	return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_READY, message)
}

func (r *Reconciler) recordError(ctx context.Context, state *resources.State, op *v1.Operation, err error) error {
	detail := proto.Clone(api.Detail(err)).(*v1.ErrorDetail)
	if detail.OperationId == "" {
		detail.OperationId = op.Id
	}
	op.Error = detail
	for _, external := range op.ExternalOperations {
		if external.Dispatched && !external.Completed {
			external.Error = detail
		}
	}
	op.Status = v1.OperationStatus_OPERATION_STATUS_BLOCKED
	if detail.Category == v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN || detail.Category == v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE {
		op.Status = v1.OperationStatus_OPERATION_STATUS_UNKNOWN
	}
	// Resolution has no remote side effects. A definite invalid specification can
	// fail while the old Deployment continues serving its already fixed release.
	if op.Phase == v1.OperationPhase_OPERATION_PHASE_RESOLVING && (detail.Category == v1.ErrorCategory_ERROR_CATEGORY_INVALID || detail.Category == v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE || detail.Category == v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND) {
		op.Status = v1.OperationStatus_OPERATION_STATUS_FAILED
	}
	state.Deployment.Condition = detail
	if !state.Target.AdmissionOpen {
		state.Target.Condition = detail
	}
	return r.save(ctx, state, op)
}

func (r *Reconciler) resolve(ctx context.Context, state *resources.State, op *v1.Operation) error {
	d := state.Deployment
	old := state.Instances[d.ActiveInstanceId]
	if old != nil {
		if r.Dependencies.Environment == nil || r.Dependencies.Executor == nil {
			return unavailable("Provider and Agentc lifecycle adapters have not been configured")
		}
		if !r.Dependencies.Environment.SupportsWriterRelease() {
			return unavailable("provider cannot prove writer release; replacement/offline requires external isolation review")
		}
		previous := state.Releases[old.ReleaseId]
		if previous == nil || !previous.Agentc.GetPersistentHistory() {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INCOMPATIBLE, "replacement/offline requires persistent native history")
		}
	}
	if d.DesiredState == v1.DesiredState_DESIRED_STATE_OFFLINE {
		if old == nil {
			ownership.Offline(state, op)
			op.Phase = v1.OperationPhase_OPERATION_PHASE_COMPLETED
			op.Status = v1.OperationStatus_OPERATION_STATUS_SUCCEEDED
			op.ReleaseId = ""
			return nil
		}
		op.Phase = v1.OperationPhase_OPERATION_PHASE_CLOSING_ADMISSION
		op.ReleaseId = old.ReleaseId
		return nil
	}
	if r.Dependencies.Resolver == nil {
		return unavailable("Artifacts resolver has not been configured")
	}
	release := state.Releases[op.ReleaseId]
	if release == nil {
		runtime, err := r.Store.GetRuntime(ctx, d.Scope, d.Spec.RuntimeRevisionId)
		if err != nil {
			return err
		}
		agentc, err := r.Store.GetAgentc(ctx, d.Scope, runtime.AgentcId)
		if err != nil {
			return err
		}
		resolved, err := r.Dependencies.Resolver.ResolvePackage(ctx, d.Spec.PackageReference)
		if err != nil {
			return err
		}
		if resolved == nil {
			return blocked("package resolution returned no description")
		}
		if err := releases.ValidateArtifact(resolved.Artifact); err != nil {
			return err
		}
		if err := releases.CheckCapabilities(resolved.RequiredCapabilities, agentc.Capabilities); err != nil {
			return err
		}
		release = &v1.Release{Scope: proto.Clone(d.Scope).(*v1.Scope), Id: op.ReleaseId, DeploymentId: d.Id, Generation: op.Generation, Spec: proto.Clone(d.Spec).(*v1.DeploymentSpec), Package: proto.Clone(resolved.Artifact).(*v1.Artifact), Agentc: agentc, Runtime: runtime, RequiredCapabilities: append([]string(nil), resolved.RequiredCapabilities...), CreatedAt: timestamppb.Now()}
		// Persist resolution before any later validation or external mutation. A
		// subsequent failure must not resolve a mutable tag to different bytes.
		state.Releases[release.Id] = release
		return nil
	}
	if err := releases.CheckCapabilities(release.RequiredCapabilities, release.Agentc.Capabilities); err != nil {
		return err
	}
	if err := r.Dependencies.Resolver.ValidateRelease(ctx, release, state.Releases[d.ActiveReleaseId]); err != nil {
		return err
	}
	if r.Dependencies.Environment == nil || r.Dependencies.Executor == nil {
		return unavailable("Provider and Agentc lifecycle adapters have not been configured")
	}
	instanceID := "inst-" + op.Id
	state.Instances[instanceID] = &v1.Instance{Id: instanceID, ReleaseId: release.Id, Epoch: state.Target.Epoch + 1}
	plan, err := r.Dependencies.Environment.Plan(ctx, control.Action{Scope: d.Scope, DeploymentID: d.Id, Workspace: state.Workspace, Release: release, Instance: state.Instances[instanceID], RequestKey: op.Id + "/" + Prepare, Step: Prepare})
	if err != nil {
		return err
	}
	state.Instances[instanceID].ComputeReference = plan
	if old != nil {
		op.Phase = v1.OperationPhase_OPERATION_PHASE_CLOSING_ADMISSION
	} else {
		op.Phase = v1.OperationPhase_OPERATION_PHASE_PREPARING
	}
	return nil
}

func providerStep(step string) bool {
	return step == Prepare || step == StopCompute || step == ReleaseCompute
}

func external(op *v1.Operation, step string) *v1.ExternalOperation {
	for _, intent := range op.ExternalOperations {
		if intent.Step == step {
			return intent
		}
	}
	return nil
}

// effect persists intent and dispatch separately. An existing dispatched intent
// is always looked up first. A missing result can only be resubmitted when the
// dependency explicitly guarantees idempotence for the same key.
func (r *Reconciler) effect(ctx context.Context, state *resources.State, op *v1.Operation, step string, instance *v1.Instance) (*resources.Observation, error) {
	key := op.Id + "/" + step
	intent := state.Intents[key]
	if intent == nil {
		state.Intents[key] = &resources.Intent{OperationID: op.Id, Step: step, RequestKey: key, InstanceID: instance.Id, Epoch: instance.Epoch}
		op.ExternalOperations = append(op.ExternalOperations, &v1.ExternalOperation{Step: step, RequestKey: key})
		return nil, r.save(ctx, state, op)
	}
	ext := external(op, step)
	if ext.Completed {
		return intent.Result, nil
	}
	action := control.Action{Scope: state.Deployment.Scope, DeploymentID: state.Deployment.Id, Workspace: state.Workspace, Release: state.Releases[instance.ReleaseId], Instance: instance, RequestKey: intent.RequestKey, ExternalID: ext.OperationId, Step: step}
	var result *resources.Observation
	var err error
	if ext.Dispatched {
		if providerStep(step) {
			if r.Dependencies.Environment == nil {
				return nil, unavailable("Provider lifecycle adapter has not been configured")
			}
			result, err = r.Dependencies.Environment.Lookup(ctx, action)
		} else {
			if r.Dependencies.Executor == nil {
				return nil, unavailable("Agentc lifecycle adapter has not been configured")
			}
			result, err = r.Dependencies.Executor.Lookup(ctx, action)
		}
		if err != nil {
			r.capturePartial(state, intent, ext, result)
			return nil, err
		}
		if result == nil {
			return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN, "dependency lookup returned no evidence")
		}
		if result.Found {
			return r.observe(ctx, state, op, intent, ext, result)
		}
		idempotent := false
		if providerStep(step) {
			idempotent = r.Dependencies.Environment.Idempotent(step)
		} else {
			idempotent = r.Dependencies.Executor.Idempotent(step)
		}
		if !idempotent {
			return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN, "original action cannot be reconciled safely; automatic redispatch is blocked")
		}
	} else {
		// The winning CAS owns the initial call. Other controllers only observe it.
		ext.Dispatched = true
		if err := r.save(ctx, state, op); err != nil {
			return nil, err
		}
	}
	switch step {
	case Prepare, StopCompute, ReleaseCompute:
		if r.Dependencies.Environment == nil {
			return nil, unavailable("Provider lifecycle adapter has not been configured")
		}
		switch step {
		case Prepare:
			result, err = r.Dependencies.Environment.Prepare(ctx, action)
		case StopCompute:
			result, err = r.Dependencies.Environment.StopCompute(ctx, action)
		case ReleaseCompute:
			result, err = r.Dependencies.Environment.ReleaseCompute(ctx, action)
		}
	default:
		if r.Dependencies.Executor == nil {
			return nil, unavailable("Agentc lifecycle adapter has not been configured")
		}
		switch step {
		case Bind:
			result, err = r.Dependencies.Executor.Bind(ctx, action)
		case OpenAdmission:
			result, err = r.Dependencies.Executor.OpenAdmission(ctx, action)
		case CloseAdmission:
			result, err = r.Dependencies.Executor.CloseAdmission(ctx, action)
		case Drain:
			result, err = r.Dependencies.Executor.Drain(ctx, action)
		case StopAgentc:
			result, err = r.Dependencies.Executor.Stop(ctx, action)
		}
	}
	if err != nil {
		r.capturePartial(state, intent, ext, result)
		return nil, err
	}
	if result == nil || !result.Found {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN, "action returned no acceptance evidence")
	}
	return r.observe(ctx, state, op, intent, ext, result)
}

// Some providers return an accepted operation reference together with an error.
// Keep that reference for recovery without treating the action as completed.
func (r *Reconciler) capturePartial(state *resources.State, intent *resources.Intent, ext *v1.ExternalOperation, result *resources.Observation) {
	if result == nil || !result.Found || result.InstanceID != intent.InstanceID || result.WorkspaceID != state.Workspace.Id {
		return
	}
	if ext.OperationId != "" && result.ExternalID != "" && ext.OperationId != result.ExternalID {
		return
	}
	if result.ExternalID != "" {
		ext.OperationId = result.ExternalID
	}
	intent.Result = result
}

func (r *Reconciler) observe(ctx context.Context, state *resources.State, op *v1.Operation, intent *resources.Intent, ext *v1.ExternalOperation, result *resources.Observation) (*resources.Observation, error) {
	if result.InstanceID != intent.InstanceID || result.WorkspaceID != state.Workspace.Id {
		return nil, blocked("dependency observation does not match the requested instance/workspace")
	}
	if ext.OperationId != "" && result.ExternalID != ext.OperationId {
		return nil, blocked("dependency changed the external operation identity")
	}
	ext.OperationId = result.ExternalID
	intent.Result = result
	ext.Error = nil
	if err := r.save(ctx, state, op); err != nil {
		return nil, err
	}
	if !result.Done {
		return nil, nil
	}
	return result, nil
}

func (r *Reconciler) advance(ctx context.Context, state *resources.State, op *v1.Operation) error {
	old := state.Instances[state.Deployment.ActiveInstanceId]
	next := state.Instances["inst-"+op.Id]
	switch op.Phase {
	case v1.OperationPhase_OPERATION_PHASE_RESOLVING:
		return r.resolve(ctx, state, op)
	case v1.OperationPhase_OPERATION_PHASE_CLOSING_ADMISSION:
		if state.Target.AdmissionOpen {
			ownership.Close(state)
			return nil
		}
		result, err := r.effect(ctx, state, op, CloseAdmission, old)
		if err != nil || result == nil {
			return err
		}
		if !result.AdmissionClosed || result.Epoch != old.Epoch {
			return blocked("old Agentc admission closure is unconfirmed")
		}
		external(op, CloseAdmission).Completed = true
		op.Phase = v1.OperationPhase_OPERATION_PHASE_DRAINING
	case v1.OperationPhase_OPERATION_PHASE_DRAINING:
		result, err := r.effect(ctx, state, op, Drain, old)
		if err != nil || result == nil {
			return err
		}
		if !result.Drained || result.ActiveTurns != 0 || result.QueuedInputs != 0 || result.PendingApprovals != 0 || result.ToolWriters != 0 || !result.HistoryRetained {
			return blocked("native turns, queues, approvals, tools or retained history are not safely drained")
		}
		external(op, Drain).Completed = true
		op.Phase = v1.OperationPhase_OPERATION_PHASE_RELEASING_WRITER
	case v1.OperationPhase_OPERATION_PHASE_RELEASING_WRITER:
		stopped, err := r.effect(ctx, state, op, StopAgentc, old)
		if err != nil || stopped == nil {
			return err
		}
		if !stopped.ProcessStopped || !stopped.HistoryRetained {
			return blocked("old Agentc stop/history retention is unconfirmed")
		}
		external(op, StopAgentc).Completed = true
		result, err := r.effect(ctx, state, op, StopCompute, old)
		if err != nil || result == nil {
			return err
		}
		if !result.ProcessStopped || !result.WriterReleased || !result.WorkspaceRetained {
			return blocked("old compute writer release and Workspace retention require provider evidence")
		}
		external(op, StopCompute).Completed = true
		old.WriterReleased = true
		if state.Deployment.DesiredState == v1.DesiredState_DESIRED_STATE_OFFLINE {
			ownership.Offline(state, op)
			op.CleanupPending = true
			op.Phase = v1.OperationPhase_OPERATION_PHASE_CLEANING
		} else {
			op.Phase = v1.OperationPhase_OPERATION_PHASE_PREPARING
		}
	case v1.OperationPhase_OPERATION_PHASE_PREPARING:
		result, err := r.effect(ctx, state, op, Prepare, next)
		if err != nil || result == nil {
			return err
		}
		release := state.Releases[op.ReleaseId]
		if result.ComputeReference == "" || result.StorageReference == "" || result.SandboxDigest != release.Runtime.Sandbox.Image.Digest {
			return blocked("prepared environment does not match the fixed sandbox or durable Workspace")
		}
		if state.Workspace.StorageReference != "" && state.Workspace.StorageReference != result.StorageReference {
			return blocked("provider changed the existing Workspace storage identity")
		}
		next.ComputeReference = result.ComputeReference
		next.SandboxDigest = result.SandboxDigest
		state.Workspace.StorageReference = result.StorageReference
		external(op, Prepare).Completed = true
		op.Phase = v1.OperationPhase_OPERATION_PHASE_STARTING
	case v1.OperationPhase_OPERATION_PHASE_STARTING:
		result, err := r.effect(ctx, state, op, Bind, next)
		if err != nil || result == nil {
			return err
		}
		release := state.Releases[op.ReleaseId]
		expectedHost := release.Agentc.HostDigest
		if expectedHost == "" {
			expectedHost = release.Agentc.Artifact.Digest
		}
		if !result.Ready || !result.AdmissionClosed || result.Epoch != next.Epoch || result.Binding == "" || result.PackageDigest != release.Package.Digest || result.AgentcDigest != expectedHost || result.SandboxDigest != release.Runtime.Sandbox.Image.Digest {
			return blocked("Agentc readiness/binding/epoch/actual digests do not match the fixed release")
		}
		if release.Agentc.PersistentHistory && !result.HistoryRetained {
			return blocked("Agentc did not confirm durable native history")
		}
		if err := releases.CheckCapabilities(release.RequiredCapabilities, result.Capabilities); err != nil {
			return err
		}
		next.AgentcBinding = result.Binding
		next.PackageDigest = result.PackageDigest
		next.AgentcDigest = result.AgentcDigest
		next.Ready = true
		next.Capabilities = append([]string(nil), result.Capabilities...)
		next.ObservedAt = timestamppb.Now()
		external(op, Bind).Completed = true
		op.Phase = v1.OperationPhase_OPERATION_PHASE_COMMITTING
	case v1.OperationPhase_OPERATION_PHASE_COMMITTING:
		result, err := r.effect(ctx, state, op, OpenAdmission, next)
		if err != nil || result == nil {
			return err
		}
		if !result.AdmissionOpen || result.Epoch != next.Epoch || result.Binding != next.AgentcBinding {
			return blocked("new Agentc admission opening is unconfirmed")
		}
		external(op, OpenAdmission).Completed = true
		if err := ownership.Commit(state, op, next, state.Releases[op.ReleaseId]); err != nil {
			return err
		}
		op.CleanupPending = old != nil
		op.Phase = v1.OperationPhase_OPERATION_PHASE_CLEANING
	case v1.OperationPhase_OPERATION_PHASE_CLEANING:
		// The active instance is already the new authority. Find the prior instance
		// through its persisted StopCompute intent, never via current routing.
		intent := state.Intents[op.Id+"/"+StopCompute]
		if intent != nil {
			previous := state.Instances[intent.InstanceID]
			result, err := r.effect(ctx, state, op, ReleaseCompute, previous)
			if err != nil || result == nil {
				return err
			}
			if !result.WorkspaceRetained {
				return blocked("compute cleanup did not confirm Workspace retention")
			}
			external(op, ReleaseCompute).Completed = true
		}
		op.CleanupPending = false
		op.Phase = v1.OperationPhase_OPERATION_PHASE_COMPLETED
		op.Status = v1.OperationStatus_OPERATION_STATUS_SUCCEEDED
	default:
		return fmt.Errorf("unknown lifecycle phase %d", op.Phase)
	}
	return nil
}

// Reconcile performs one bounded advancement. Every call reloads durable state;
// it is safe to construct a fresh Reconciler after any individual external call.
func (r *Reconciler) Reconcile(ctx context.Context, scope *v1.Scope, deploymentID string) error {
	if r.Scope != nil && !proto.Equal(r.Scope, scope) {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_FORBIDDEN, "controller is not authorized for this scope")
	}
	state, err := r.Store.Load(ctx, scope, deploymentID)
	if err != nil {
		return err
	}
	op := state.Operations[state.Deployment.OperationId]
	if op == nil || !resources.HasWork(state) {
		return nil
	}
	timeout := r.CallTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	op.Status = v1.OperationStatus_OPERATION_STATUS_RUNNING
	op.Error = nil
	state.Deployment.Condition = nil
	state.Target.Condition = nil
	err = r.advance(callCtx, state, op)
	if err != nil {
		if errors.Is(err, errConcurrent) {
			return err
		}
		return r.recordError(ctx, state, op, err)
	}
	return r.save(ctx, state, op)
}

// Run uses durable pending records rather than an in-memory queue. Fair ordering
// is implemented by Store.Pending so a blocked deployment cannot starve others.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration, onError func(error)) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		keys, err := r.Store.Pending(ctx, r.Scope, 100)
		if err != nil {
			if onError != nil {
				onError(err)
			}
		} else {
			for _, key := range keys {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err := r.Reconcile(ctx, key.Scope, key.ID); err != nil && !api.IsCategory(err, v1.ErrorCategory_ERROR_CATEGORY_CONFLICT) && onError != nil {
					onError(err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
