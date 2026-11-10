package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control"
	"github.com/jingyugao/a8s/control/controllers"
	"github.com/jingyugao/a8s/control/releases"
	"github.com/jingyugao/a8s/control/resources"
	provider "github.com/jingyugao/a8s/providers/contract"
	native "github.com/jingyugao/a8s/providers/kubernetes"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type environmentAdapter struct {
	provider  provider.Provider
	mountPath string
}

var _ control.Environment = (*environmentAdapter)(nil)

func (e *environmentAdapter) SupportsWriterRelease() bool {
	return e.provider.Capabilities().WriterReleaseProof
}

// Create is never replayed on absence. Stop/cleanup use exact Pod UID
// preconditions and cannot affect a replacement resource with the same name.
func (e *environmentAdapter) Idempotent(step string) bool {
	return step == controllers.StopCompute || step == controllers.ReleaseCompute
}

func (e *environmentAdapter) Plan(ctx context.Context, action control.Action) (string, error) {
	sandbox := action.Release.Runtime.Sandbox
	if sandbox.Provider != "kubernetes" {
		return "", api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "server only configures the native Kubernetes provider")
	}
	if action.Workspace.StorageReference == "" {
		return "", api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "native Kubernetes requires an existing Workspace PVC")
	}
	request := provider.EnvironmentRequest{Image: releases.FixedReference(sandbox.Image), RevisionID: sandbox.EnvironmentRevision}
	if raw := os.Getenv("A8S_SANDBOX_LAUNCH"); raw != "" {
		launch := &provider.LaunchSpec{}
		if err := json.Unmarshal([]byte(raw), launch); err != nil {
			return "", api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "invalid sandbox launch configuration")
		}
		if launch.Environment == nil {
			launch.Environment = map[string]string{}
		}
		launch.Environment["A8S_PACKAGE_DIGEST"] = action.Release.Package.Digest
		launch.Environment["A8S_NATIVE_STATE"] = e.mountPath + "/.a8s/" + action.Instance.Id
		request.Launch = launch
	}
	if sandbox.Architecture != "" {
		request.Platform = "linux/" + sandbox.Architecture
	}
	for name, value := range sandbox.Resources {
		var err error
		switch name {
		case "cpu":
			request.CPU, err = strconv.ParseFloat(value, 64)
		case "memory_mib":
			request.MemoryMiB, err = strconv.ParseInt(value, 10, 64)
		default:
			return "", api.NewError(v1.ErrorCategory_ERROR_CATEGORY_UNSUPPORTED, "unsupported sandbox resource constraint")
		}
		if err != nil {
			return "", api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "invalid sandbox resource constraint")
		}
	}
	environment, err := e.provider.ResolveEnvironment(ctx, request)
	if err != nil {
		return "", dependencyError(err)
	}
	opID, _, _ := strings.Cut(action.RequestKey, "/")
	ref, err := e.provider.Plan(ctx, provider.PrepareRequest{
		Context:     provider.OperationContext{Scope: api.ScopeKey(action.Scope), DeploymentID: action.DeploymentID, InstanceID: action.Instance.Id, OperationID: opID, RequestKey: action.RequestKey},
		Environment: environment,
		Workspace:   provider.WorkspaceBinding{ID: action.Workspace.Id, StorageReference: action.Workspace.StorageReference, MountPath: e.mountPath},
	})
	if err != nil {
		return "", dependencyError(err)
	}
	encoded, err := json.Marshal(ref)
	return string(encoded), err
}

func computeReference(action control.Action) (provider.ComputeReference, error) {
	var ref provider.ComputeReference
	if err := json.Unmarshal([]byte(action.Instance.ComputeReference), &ref); err != nil {
		return ref, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "provider reference is missing or invalid")
	}
	if ref.Request.Context.Scope != api.ScopeKey(action.Scope) || ref.Request.Context.DeploymentID != action.DeploymentID || ref.Request.Context.InstanceID != action.Instance.Id || ref.Request.Workspace.ID != action.Workspace.Id {
		return ref, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_CONFLICT, "provider reference does not match the requested resource")
	}
	return ref, nil
}

func (e *environmentAdapter) Prepare(ctx context.Context, action control.Action) (*resources.Observation, error) {
	ref, err := computeReference(action)
	if err != nil {
		return nil, err
	}
	op, err := e.provider.Prepare(ctx, ref)
	return providerObservation(action, op, err)
}

func (e *environmentAdapter) Lookup(ctx context.Context, action control.Action) (*resources.Observation, error) {
	ref, err := computeReference(action)
	if err != nil {
		return nil, err
	}
	kind := provider.PrepareCompute
	if action.Step == controllers.StopCompute {
		kind = provider.StopCompute
	}
	if action.Step == controllers.ReleaseCompute {
		kind = provider.ReleaseCompute
	}
	op, err := e.provider.InspectOperation(ctx, provider.OperationReference{ID: action.ExternalID, Kind: kind, Target: ref})
	result, err := providerObservation(action, op, err)
	if err == nil && kind != provider.PrepareCompute && op.Status == provider.Pending {
		result.Found = false // safe continuation through UID-fenced mutation
	}
	return result, err
}

func (e *environmentAdapter) StopCompute(ctx context.Context, action control.Action) (*resources.Observation, error) {
	ref, err := computeReference(action)
	if err != nil {
		return nil, err
	}
	op, err := e.provider.StopCompute(ctx, provider.ComputeRequest{Target: ref, RequestKey: action.RequestKey, ExternalID: action.ExternalID})
	return providerObservation(action, op, err)
}

func (e *environmentAdapter) ReleaseCompute(ctx context.Context, action control.Action) (*resources.Observation, error) {
	ref, err := computeReference(action)
	if err != nil {
		return nil, err
	}
	op, err := e.provider.ReleaseCompute(ctx, provider.ComputeRequest{Target: ref, RequestKey: action.RequestKey, ExternalID: action.ExternalID})
	return providerObservation(action, op, err)
}

func providerObservation(action control.Action, op provider.Operation, callErr error) (*resources.Observation, error) {
	result := &resources.Observation{Found: op.Reference.Target.ID != "", ExternalID: op.Reference.ID, InstanceID: action.Instance.Id, WorkspaceID: action.Workspace.Id, Done: op.Status == provider.Succeeded}
	ref := op.Reference.Target
	if observation := op.Observation; observation != nil {
		ref = observation.Reference
		result.ProcessStopped = observation.Writer.ComputeStopped
		result.WriterReleased = observation.Writer.State == provider.EvidenceReleased
		result.WorkspaceRetained = observation.WorkspaceRetained
		result.StorageReference = ref.Request.Workspace.StorageReference
		// Provider checked the immutable Pod spec and its image digest.
		_, result.SandboxDigest, _ = strings.Cut(ref.Request.Environment.Image, "@")
	}
	encoded, _ := json.Marshal(ref)
	result.ComputeReference = string(encoded)
	if callErr != nil {
		return result, dependencyError(callErr)
	}
	if op.Failure != nil {
		return result, dependencyError(op.Failure)
	}
	if op.Status == provider.Failed || op.Status == provider.Blocked || op.Status == provider.Unknown {
		return result, api.NewError(v1.ErrorCategory_ERROR_CATEGORY_OUTCOME_UNKNOWN, "provider operation is not confirmed; inspect its original operation")
	}
	return result, nil
}

func newEnvironment(namespace, mountPath string) (*environmentAdapter, error) {
	var config *rest.Config
	var err error
	if os.Getenv("KUBECONFIG") != "" {
		loader := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{CurrentContext: os.Getenv("A8S_KUBERNETES_CONTEXT")}).ClientConfig()
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, errors.New("cannot load Kubernetes configuration; use in-cluster credentials or explicit KUBECONFIG")
	}
	config.Timeout = 15 * time.Second
	client, err := typed.NewForConfig(config)
	if err != nil {
		return nil, errors.New("cannot initialize Kubernetes client")
	}
	port := int64(8095)
	if value := os.Getenv("A8S_EXEC_SERVER_PORT"); value != "" {
		port, err = strconv.ParseInt(value, 10, 32)
		if err != nil {
			return nil, errors.New("invalid A8S_EXEC_SERVER_PORT")
		}
	}
	p, err := native.New(native.Config{Client: client, Namespace: namespace, Port: int32(port), Timeout: 15 * time.Second})
	if err != nil {
		return nil, err
	}
	return &environmentAdapter{provider: p, mountPath: mountPath}, nil
}

func dependencyError(err error) error {
	if err == nil {
		return nil
	}
	var failure *provider.Error
	if errors.As(err, &failure) {
		category := categoryByName(string(failure.Category))
		return &api.Error{Detail: &v1.ErrorDetail{Category: category, Message: failure.Message, Source: failure.Source, Code: failure.Code, OperationId: failure.OperationID}, Cause: err}
	}
	return agentError(err)
}

func categoryByName(name string) v1.ErrorCategory {
	for value, label := range v1.ErrorCategory_name {
		if strings.EqualFold(strings.ReplaceAll(strings.TrimPrefix(label, "ERROR_CATEGORY_"), "_", ""), name) {
			return v1.ErrorCategory(value)
		}
	}
	return v1.ErrorCategory_ERROR_CATEGORY_DEPENDENCY_UNAVAILABLE
}
