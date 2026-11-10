package kubernetes

import (
	"context"
	"encoding/json"
	"strings"

	provider "github.com/jingyugao/a8s/providers/contract"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func operation(ref provider.ComputeReference, kind provider.OperationKind) provider.Operation {
	op := provider.Operation{Reference: provider.OperationReference{Kind: kind, Target: ref}, Status: provider.Unknown}
	if ref.UID != "" {
		op.Reference.ID = string(kind) + ":" + ref.UID
	}
	return op
}

func (p *Provider) InspectOperation(ctx context.Context, ref provider.OperationReference) (provider.Operation, error) {
	op := operation(ref.Target, ref.Kind)
	if ref.Kind != provider.PrepareCompute && ref.Kind != provider.StopCompute && ref.Kind != provider.ReleaseCompute {
		return op, fault(provider.Unsupported, "UnsupportedOperation", "native provider does not support the requested operation")
	}
	if ref.ID != "" {
		prefix := string(ref.Kind) + ":"
		uid := strings.TrimPrefix(ref.ID, prefix)
		if !strings.HasPrefix(ref.ID, prefix) || uid == "" || ref.Target.UID != "" && uid != ref.Target.UID {
			return op, fault(provider.Conflict, "OperationIdentityMismatch", "operation identity does not match the original Pod UID")
		}
		ref.Target.UID = uid
	}
	if err := p.validate(ref.Target); err != nil {
		return op, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	pod, err := p.config.Client.Pods(ref.Target.Namespace).Get(ctx, ref.Target.ID, meta.GetOptions{})
	if apierrors.IsNotFound(err) && ref.Kind == provider.ReleaseCompute && ref.Target.UID != "" {
		return p.released(ctx, ref.Target)
	}
	if err != nil {
		return op, unknown(err, ref.ID, ref.Target.ID)
	}
	if err := checkPod(ref.Target, pod); err != nil {
		return op, err
	}
	ref.Target.UID = string(pod.UID)
	if ref.Kind == provider.PrepareCompute {
		return p.prepared(ctx, ref.Target, pod)
	}
	op = operation(ref.Target, ref.Kind)
	observation, err := p.observe(ctx, ref.Target, pod)
	op.Observation = &observation
	if err != nil {
		return op, err
	}
	if ref.Kind == provider.StopCompute && observation.Writer.ComputeStopped {
		op.Status = provider.Succeeded
	} else if ref.Kind == provider.ReleaseCompute || pod.DeletionTimestamp == nil {
		// These steps can safely be repeated using UID preconditions. The
		// assembly adapter reports Pending as not yet dispatched/completed.
		op.Status = provider.Pending
	} else {
		op.Status, op.Stage = provider.Running, "WaitingForContainerExit"
	}
	return op, nil
}

func (p *Provider) StopCompute(ctx context.Context, request provider.ComputeRequest) (provider.Operation, error) {
	ref := provider.OperationReference{ID: request.ExternalID, Kind: provider.StopCompute, Target: request.Target}
	op, err := p.InspectOperation(ctx, ref)
	if err != nil || op.Status != provider.Pending {
		return op, err
	}
	if request.Target.UID == "" || request.RequestKey == "" {
		return op, fault(provider.Invalid, "StopIdentityRequired", "persisted Pod UID and request key are required for stop")
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	uid, version := types.UID(request.Target.UID), op.Observation.ResourceVersion
	err = p.config.Client.Pods(request.Target.Namespace).Delete(ctx, request.Target.ID, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid, ResourceVersion: &version}})
	if err != nil {
		return op, wrap(err, true, op.Reference.ID, request.Target.ID)
	}
	op.Status, op.Stage = provider.Running, "WaitingForContainerExit"
	return op, nil
}

func (p *Provider) ReleaseCompute(ctx context.Context, request provider.ComputeRequest) (provider.Operation, error) {
	op, err := p.InspectOperation(ctx, provider.OperationReference{ID: request.ExternalID, Kind: provider.ReleaseCompute, Target: request.Target})
	if err != nil || op.Status != provider.Pending {
		return op, err
	}
	if request.RequestKey == "" || request.Target.UID == "" {
		return op, fault(provider.Invalid, "ReleaseIdentityRequired", "persisted Pod UID and request key are required for cleanup")
	}
	if !op.Observation.Writer.ComputeStopped {
		return op, fault(provider.NotReady, "WriterNotStopped", "compute cleanup requires confirmed container exit; active or unreachable writers are retained")
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	if err := p.deleteService(ctx, request.Target); err != nil {
		return op, err
	}
	pods := p.config.Client.Pods(request.Target.Namespace)
	pod, err := pods.Get(ctx, request.Target.ID, meta.GetOptions{})
	if err != nil {
		return op, unknown(err, op.Reference.ID, request.Target.ID)
	}
	if err := checkPod(request.Target, pod); err != nil {
		return op, err
	}
	if pod.DeletionTimestamp == nil {
		uid := pod.UID
		err = pods.Delete(ctx, pod.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid, ResourceVersion: &pod.ResourceVersion}})
	} else {
		remaining := []string{}
		for _, entry := range pod.Finalizers {
			if entry != finalizer {
				remaining = append(remaining, entry)
			}
		}
		if len(remaining) == len(pod.Finalizers) {
			op.Status = provider.Running
			return op, nil // already released; wait for Kubernetes/other finalizers
		}
		patch, _ := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": request.Target.UID},
			{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
			{"op": "replace", "path": "/metadata/finalizers", "value": remaining},
		})
		_, err = pods.Patch(ctx, pod.Name, types.JSONPatchType, patch, meta.PatchOptions{})
	}
	if err != nil {
		return op, wrap(err, true, op.Reference.ID, request.Target.ID)
	}
	op.Status = provider.Running
	return op, nil
}

func (p *Provider) deleteService(ctx context.Context, ref provider.ComputeReference) error {
	services := p.config.Client.Services(ref.Namespace)
	service, err := services.Get(ctx, ref.ID, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return wrap(err, false, "release:"+ref.UID, ref.ID)
	}
	if err := checkService(ref, service); err != nil {
		return err
	}
	err = services.Delete(ctx, ref.ID, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &service.UID, ResourceVersion: &service.ResourceVersion}})
	if err != nil && !apierrors.IsNotFound(err) {
		return wrap(err, true, "release:"+ref.UID, ref.ID)
	}
	_, err = services.Get(ctx, ref.ID, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return wrap(err, false, "release:"+ref.UID, ref.ID)
	}
	return fault(provider.NotReady, "ServiceDeleting", "old Instance Service is still being removed")
}

func (p *Provider) released(ctx context.Context, ref provider.ComputeReference) (provider.Operation, error) {
	op := operation(ref, provider.ReleaseCompute)
	pvc, err := p.workspace(ctx, ref)
	if err != nil {
		return op, err
	}
	if err := p.deleteService(ctx, ref); err != nil {
		return op, err
	}
	op.Status = provider.Succeeded
	op.Observation = &provider.Observation{Reference: ref, Phase: "absent", WorkspaceRetained: pvc.DeletionTimestamp == nil && pvc.Status.Phase != core.ClaimLost,
		StoragePhase: string(pvc.Status.Phase), Writer: provider.WriterEvidence{State: provider.EvidenceUnknown, Reason: "compute metadata cleaned; prior stop evidence is owned by Control"}}
	return op, nil
}
