package kubernetes

import (
	"context"
	"encoding/json"
	"reflect"

	provider "github.com/jingyugao/a8s/providers/contract"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func claimName(ref provider.ComputeReference) string { return "claim-" + ref.ID }

// allocation queries the original durable reservation. A missing reservation or
// Pod never causes inspection to select another target.
func (p *Provider) allocation(ctx context.Context, ref provider.ComputeReference) (provider.ComputeReference, error) {
	if ref.Request.Environment.Pool == "" {
		return ref, nil
	}
	cm, err := p.config.Client.ConfigMaps(ref.Namespace).Get(ctx, claimName(ref), meta.GetOptions{})
	if err != nil {
		return ref, unknown(err, "", claimName(ref))
	}
	if cm.Data["intent"] != fingerprint(ref) || cm.Labels[managedBy] != "a8s" {
		return ref, fault(provider.Conflict, "ClaimIntentMismatch", "reservation belongs to another immutable instance intent")
	}
	name, uid := cm.Data["podName"], cm.Data["uid"]
	if name != "" {
		if uid == "" || ref.PodName != "" && ref.PodName != name || ref.UID != "" && ref.UID != uid {
			return ref, fault(provider.Conflict, "ClaimTargetMismatch", "reservation differs from the original Pod name or UID")
		}
		ref.PodName, ref.UID = name, uid
	}
	return ref, nil
}

func (p *Provider) allocate(ctx context.Context, ref provider.ComputeReference) (provider.ComputeReference, error) {
	cms := p.config.Client.ConfigMaps(ref.Namespace)
	_, err := cms.Get(ctx, claimName(ref), meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		pool := &WarmPool{Provider: p, Name: ref.Request.Environment.Pool, Scope: ref.Request.Context.Scope}
		_, spec, state, storageUID, err := pool.load(ctx)
		if err != nil {
			return ref, err
		}
		expected := normalizePool(PoolSpec{Scope: ref.Request.Context.Scope, DeploymentID: ref.Request.Context.DeploymentID, Environment: ref.Request.Environment, Workspace: ref.Request.Workspace})
		if storageUID != ref.StorageUID || !reflect.DeepEqual(expected, normalizePool(spec)) {
			return ref, fault(provider.Conflict, "PoolRevisionMismatch", "pool template, workspace or storage differs from the fixed instance")
		}
		data := map[string]string{"intent": fingerprint(ref), "podName": "", "uid": ""}
		if spec.MinReady > 0 && state.Revision == poolRevision(spec, storageUID) {
			for _, slot := range state.Slots {
				if slot.UID == "" {
					continue
				}
				pod, err := p.config.Client.Pods(ref.Namespace).Get(ctx, slot.Name, meta.GetOptions{})
				if apierrors.IsNotFound(err) {
					continue
				}
				if err != nil {
					return ref, err
				}
				if string(pod.UID) != slot.UID || !podReady(pod) || pod.Annotations[claimKey] != "" {
					continue
				}
				if err := p.unclaimed(ctx, pod); err != nil {
					return ref, err
				}
				data["podName"], data["uid"] = slot.Name, slot.UID
				break
			}
		}
		// Even a cold fallback is fixed before side effects; later retries cannot
		// jump to a newly available warm Pod after an ambiguous create.
		_, err = cms.Create(ctx, &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: claimName(ref), Namespace: ref.Namespace, Labels: map[string]string{managedBy: "a8s", poolLabel: ref.Request.Environment.Pool}}, Data: data}, meta.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return ref, unknown(err, "", claimName(ref))
		}
	} else if err != nil {
		return ref, err
	}
	ref, err = p.allocation(ctx, ref)
	if err != nil {
		return ref, err
	}
	if ref.PodName != "" {
		return ref, p.claimAllocated(ctx, ref)
	}
	return ref, nil
}

func (p *Provider) claimAllocated(ctx context.Context, ref provider.ComputeReference) error {
	pods := p.config.Client.Pods(ref.Namespace)
	pod, err := pods.Get(ctx, ref.PodName, meta.GetOptions{})
	if err != nil {
		return unknown(err, "", ref.PodName)
	}
	if string(pod.UID) != ref.UID {
		return fault(provider.Conflict, "ClaimUIDChanged", "reserved warm Pod was replaced")
	}
	if pod.Annotations[claimKey] == ref.ID {
		return checkPod(ref, pod)
	}
	if err := p.unclaimed(ctx, pod); err != nil {
		return err
	}
	if pod.Annotations[poolStorageKey] != ref.StorageUID || pod.Labels[poolLabel] != ref.Request.Environment.Pool || !podReady(pod) {
		return fault(provider.NotReady, "WarmPodNotReady", "reserved Pod is not ready for this pool and PVC identity")
	}
	if err := p.noOtherWriter(ctx, ref); err != nil {
		return err
	}
	annotations := pod.Annotations
	annotations[claimKey], annotations[intentKey] = ref.ID, fingerprint(ref)
	labels := pod.Labels
	labels["a8s.io/instance"] = ref.ID
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": ref.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pod.ResourceVersion},
		{"op": "replace", "path": "/metadata/annotations", "value": annotations},
		{"op": "replace", "path": "/metadata/labels", "value": labels},
		{"op": "add", "path": "/metadata/finalizers", "value": []string{finalizer}},
	})
	pod, err = pods.Patch(ctx, ref.PodName, types.JSONPatchType, patch, meta.PatchOptions{})
	if err != nil {
		if latest, queryErr := pods.Get(ctx, ref.PodName, meta.GetOptions{}); queryErr == nil && string(latest.UID) == ref.UID && latest.Annotations[claimKey] == ref.ID {
			return checkPod(ref, latest)
		}
		return wrap(err, true, "", ref.PodName)
	}
	return checkPod(ref, pod)
}
