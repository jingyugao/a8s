package control

import (
	"context"
	"crypto/rand"
	"strings"
	"time"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control/releases"
	"github.com/jingyugao/a8s/control/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	Store        resources.Store
	Dependencies Dependencies
}

func clone[T proto.Message](value T) T { return proto.Clone(value).(T) }

func validateKey(scope *v1.Scope, id string) error {
	if err := resources.ValidateScope(scope); err != nil {
		return err
	}
	return resources.ValidateID(id)
}

func validateSpec(spec *v1.DeploymentSpec) error {
	if _, _, err := IdlePolicy(spec.GetIdleReclaim()); err != nil {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, err.Error())
	}
	if spec.GetPackageReference() == "" || spec.GetRuntimeRevisionId() == "" {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "package_reference and runtime_revision_id are required")
	}
	for _, credential := range spec.Credentials {
		if credential.GetName() == "" {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "credential reference name is required")
		}
	}
	return nil
}

func (s *Service) RegisterAgentc(ctx context.Context, registration *v1.AgentcRegistration) (*v1.AgentcRegistration, error) {
	if err := validateKey(registration.GetScope(), registration.GetId()); err != nil {
		return nil, err
	}
	if err := releases.ValidateArtifact(registration.Artifact); err != nil {
		return nil, err
	}
	if registration.HostDigest != "" {
		if err := releases.ValidateArtifact(&v1.Artifact{Reference: "agentc-host", Digest: registration.HostDigest}); err != nil {
			return nil, err
		}
	}
	if registration.ProtocolVersion == "" {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "protocol_version is required")
	}
	if registration.PersistentHistory && registration.NativeStateVersion == "" {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "persistent history requires a declared native_state_version")
	}
	a := clone(registration)
	a.ResourceVersion = 1
	if err := s.Store.PutAgentc(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) CreateRuntimeRevision(ctx context.Context, revision *v1.RuntimeRevision) (*v1.RuntimeRevision, error) {
	if err := validateKey(revision.GetScope(), revision.GetId()); err != nil {
		return nil, err
	}
	if _, err := s.Store.GetAgentc(ctx, revision.Scope, revision.AgentcId); err != nil {
		return nil, err
	}
	if revision.GetSandbox().GetProvider() == "" || revision.GetSandbox().GetEnvironmentRevision() == "" {
		return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "sandbox provider and fixed environment_revision are required")
	}
	if err := releases.ValidateArtifact(revision.Sandbox.Image); err != nil {
		return nil, err
	}
	r := clone(revision)
	r.ResourceVersion = 1
	if err := s.Store.PutRuntime(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func newOperation(state *resources.State, kind, requestKey, fingerprint string) *v1.Operation {
	op := &v1.Operation{
		Scope: clone(state.Deployment.Scope), Id: "op-" + rand.Text(), DeploymentId: state.Deployment.Id,
		Kind: kind, RequestKey: requestKey, Generation: state.Deployment.Generation,
		ReleaseId: "rel-" + rand.Text(), Status: v1.OperationStatus_OPERATION_STATUS_PENDING,
		Phase: v1.OperationPhase_OPERATION_PHASE_RESOLVING, CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(),
	}
	state.Operations[op.Id] = op
	state.Requests[kind+":"+requestKey] = resources.RequestRecord{Fingerprint: fingerprint, OperationID: op.Id}
	state.Deployment.OperationId = op.Id
	state.Deployment.Condition = nil
	return op
}

func requestValid(key string) error {
	if strings.TrimSpace(key) == "" || len(key) > 256 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "request_key must contain 1..256 bytes")
	}
	return nil
}

func acceptanceError(err error, op *v1.Operation) error {
	if !api.IsCategory(err, v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE) {
		return err
	}
	detail := clone(api.Detail(err))
	detail.Category = v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN
	detail.OperationId = op.Id
	detail.Message = "lifecycle acceptance could not be confirmed; retry the original request key"
	return &api.Error{Detail: detail, Cause: err}
}

func replay(state *resources.State, kind, key, fingerprint string) (*v1.Operation, error) {
	if record, ok := state.Requests[kind+":"+key]; ok {
		if record.Fingerprint != fingerprint {
			return nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "request_key was used with different content")
		}
		return state.Operations[record.OperationID], nil
	}
	return nil, nil
}

func (s *Service) CreateDeployment(ctx context.Context, req *v1.CreateDeploymentRequest) (*v1.Deployment, *v1.Operation, error) {
	if err := validateKey(req.GetScope(), req.GetId()); err != nil {
		return nil, nil, err
	}
	if err := requestValid(req.RequestKey); err != nil {
		return nil, nil, err
	}
	if err := validateSpec(req.Spec); err != nil {
		return nil, nil, err
	}
	fingerprint := resources.Fingerprint(req)
	if existing, err := s.Store.Load(ctx, req.Scope, req.Id); err == nil {
		op, replayErr := replay(existing, "create", req.RequestKey, fingerprint)
		if op != nil || replayErr != nil {
			return existing.Deployment, op, replayErr
		}
		return nil, nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "deployment id already exists")
	} else if !api.IsCategory(err, v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND) {
		return nil, nil, err
	}
	if _, err := s.Store.GetRuntime(ctx, req.Scope, req.Spec.RuntimeRevisionId); err != nil {
		return nil, nil, err
	}
	workspaceID := req.WorkspaceId
	if workspaceID == "" {
		workspaceID = "ws-" + rand.Text()
	}
	if err := resources.ValidateID(workspaceID); err != nil {
		return nil, nil, err
	}
	state := &resources.State{
		Deployment: &v1.Deployment{Scope: clone(req.Scope), Id: req.Id, WorkspaceId: workspaceID, Spec: clone(req.Spec), DesiredState: v1.DesiredState_DESIRED_STATE_ONLINE, ResourceVersion: 1, Generation: 1},
		Workspace:  &v1.Workspace{Scope: clone(req.Scope), Id: workspaceID, DeploymentId: req.Id, StorageReference: req.StorageReference, Retained: true},
		Target:     &v1.ExecutionTarget{Scope: clone(req.Scope), DeploymentId: req.Id, WorkspaceId: workspaceID, ViewVersion: 1},
		Releases:   map[string]*v1.Release{}, Operations: map[string]*v1.Operation{}, Instances: map[string]*v1.Instance{}, Requests: map[string]resources.RequestRecord{}, Intents: map[string]*resources.Intent{},
	}
	op := newOperation(state, "create", req.RequestKey, fingerprint)
	state.Target.OperationId = op.Id
	if err := s.Store.Create(ctx, state); err != nil {
		if api.IsCategory(err, v1.ErrorCategory_ERROR_CATEGORY_CONFLICT) {
			if current, loadErr := s.Store.Load(ctx, req.Scope, req.Id); loadErr == nil {
				if original, replayErr := replay(current, "create", req.RequestKey, fingerprint); original != nil || replayErr != nil {
					return current.Deployment, original, replayErr
				}
			}
		}
		return nil, nil, acceptanceError(err, op)
	}
	return state.Deployment, op, nil
}

func checkVersion(state *resources.State, expected uint64) error {
	if expected == 0 || state.Deployment.ResourceVersion != expected {
		err := api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "deployment resource_version changed")
		err.Detail.CurrentVersion = state.Deployment.ResourceVersion
		return err
	}
	if resources.HasWork(state) {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "a lifecycle operation is still active; reconcile it before changing desired state")
	}
	return nil
}

func (s *Service) mutate(ctx context.Context, scope *v1.Scope, id, kind, key string, expected uint64, req proto.Message, apply func(*resources.State, *v1.Operation) error) (*v1.Deployment, *v1.Operation, error) {
	if err := validateKey(scope, id); err != nil {
		return nil, nil, err
	}
	if err := requestValid(key); err != nil {
		return nil, nil, err
	}
	state, err := s.Store.Load(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	fingerprint := resources.Fingerprint(req)
	if op, err := replay(state, kind, key, fingerprint); op != nil || err != nil {
		return state.Deployment, op, err
	}
	if err := checkVersion(state, expected); err != nil {
		return nil, nil, err
	}
	state.Deployment.Generation++
	op := newOperation(state, kind, key, fingerprint)
	if err := apply(state, op); err != nil {
		return nil, nil, err
	}
	if op.Status != v1.OperationStatus_OPERATION_STATUS_SUCCEEDED && state.Deployment.ActiveInstanceId != "" {
		previous := state.Releases[state.Deployment.ActiveReleaseId]
		if previous == nil || !previous.Agentc.GetPersistentHistory() {
			return nil, nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "replacement/offline requires persistent native history")
		}
		if s.Dependencies.Environment != nil && !s.Dependencies.Environment.SupportsWriterRelease() {
			return nil, nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "provider cannot prove writer release; replacement/offline requires external isolation review")
		}
	}
	state.Deployment.ResourceVersion = expected + 1
	state.Target.ViewVersion = expected + 1
	state.Target.OperationId = op.Id
	if err := s.Store.Save(ctx, state, expected); err != nil {
		// A concurrent retry of the identical request must still find its operation.
		if api.IsCategory(err, v1.ErrorCategory_ERROR_CATEGORY_CONFLICT) {
			if latest, loadErr := s.Store.Load(ctx, scope, id); loadErr == nil {
				if original, replayErr := replay(latest, kind, key, fingerprint); original != nil || replayErr != nil {
					return latest.Deployment, original, replayErr
				}
			}
		}
		return nil, nil, acceptanceError(err, op)
	}
	return state.Deployment, op, nil
}

func (s *Service) UpdateDeployment(ctx context.Context, req *v1.UpdateDeploymentRequest) (*v1.Deployment, *v1.Operation, error) {
	if err := validateSpec(req.GetSpec()); err != nil {
		return nil, nil, err
	}
	return s.mutate(ctx, req.Scope, req.Id, "update", req.RequestKey, req.ExpectedResourceVersion, req, func(state *resources.State, op *v1.Operation) error {
		if _, err := s.Store.GetRuntime(ctx, req.Scope, req.Spec.RuntimeRevisionId); err != nil {
			return err
		}
		state.Deployment.Spec = clone(req.Spec)
		return nil
	})
}

func (s *Service) SetDesiredState(ctx context.Context, req *v1.SetDesiredStateRequest) (*v1.Deployment, *v1.Operation, error) {
	if req.GetDesiredState() != v1.DesiredState_DESIRED_STATE_ONLINE && req.GetDesiredState() != v1.DesiredState_DESIRED_STATE_OFFLINE {
		return nil, nil, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "only online and offline are supported")
	}
	return s.mutate(ctx, req.Scope, req.Id, "state", req.RequestKey, req.ExpectedResourceVersion, req, func(state *resources.State, op *v1.Operation) error {
		previous := state.Releases[state.Deployment.ActiveReleaseId]
		if req.DesiredState == state.Deployment.DesiredState && ((req.DesiredState == v1.DesiredState_DESIRED_STATE_OFFLINE && state.Deployment.ActiveInstanceId == "") || (state.Target.AdmissionOpen && previous != nil && proto.Equal(previous.Spec, state.Deployment.Spec))) {
			op.ReleaseId = state.Deployment.ActiveReleaseId
			op.Phase = v1.OperationPhase_OPERATION_PHASE_COMPLETED
			op.Status = v1.OperationStatus_OPERATION_STATUS_SUCCEEDED
			op.Published = true
			state.Deployment.ObservedGeneration = op.Generation
			return nil
		}
		state.Deployment.DesiredState = req.DesiredState
		if req.DesiredState == v1.DesiredState_DESIRED_STATE_OFFLINE {
			return nil
		}
		if previous := state.Releases[state.Deployment.ActiveReleaseId]; previous != nil && proto.Equal(previous.Spec, state.Deployment.Spec) {
			r := clone(previous)
			r.Id = op.ReleaseId
			r.Generation = op.Generation
			r.CreatedAt = timestamppb.Now()
			state.Releases[r.Id] = r
		}
		return nil
	})
}

func (s *Service) Rollback(ctx context.Context, req *v1.RollbackRequest) (*v1.Deployment, *v1.Operation, error) {
	return s.mutate(ctx, req.Scope, req.Id, "rollback", req.RequestKey, req.ExpectedResourceVersion, req, func(state *resources.State, op *v1.Operation) error {
		old := state.Releases[req.ReleaseId]
		if old == nil {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_NOT_FOUND, "release not found")
		}
		r := clone(old)
		r.Id = op.ReleaseId
		r.Generation = op.Generation
		r.CreatedAt = timestamppb.New(time.Now())
		r.RollbackSourceId = old.Id
		state.Releases[r.Id] = r
		state.Deployment.Spec = clone(r.Spec)
		return nil
	})
}
