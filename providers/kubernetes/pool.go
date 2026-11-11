package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	provider "github.com/jingyugao/a8s/providers/contract"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	poolLabel      = "a8s.io/warm-pool"
	claimKey       = "a8s.io/pool-claim"
	poolSpecKey    = "a8s.io/warm-spec"
	poolVersionKey = "a8s.io/warm-revision"
	poolStorageKey = "a8s.io/warm-storage-uid"
)

// PoolSpec is operator policy. Capacity is mutable; the execution template is
// fixed by content, runtime, configuration, resources and PVC identity.
type PoolSpec struct {
	MinReady     int                       `json:"minReady"`
	Scope        string                    `json:"scope"`
	DeploymentID string                    `json:"deploymentId"`
	Environment  provider.Environment      `json:"environment"`
	Workspace    provider.WorkspaceBinding `json:"workspace"`
}

type poolSlot struct{ Name, UID string }
type poolState struct {
	Revision string
	Sequence uint64
	Slots    []poolSlot
}

type PoolStatus struct {
	MinReady int    `json:"minReady"`
	Ready    int    `json:"ready"`
	Warming  int    `json:"warming"`
	Claimed  int    `json:"claimed"`
	Revision string `json:"revision"`
	Error    string `json:"error,omitempty"`
}

// WarmPool keeps creation tickets in its ConfigMap before creating Pods. CAS
// updates serialize capacity changes across server replicas without local locks.
type WarmPool struct {
	Provider    *Provider
	Name, Scope string
}

func warmEnvironment(input map[string]string) map[string]string {
	out := make(map[string]string, len(input)+1)
	for key, value := range input {
		if key == "A8S_GUEST_SLOT" || key == "A8S_NATIVE_STATE" {
			continue
		}
		if key == "A8S_IDLE_RECLAIM" {
			var policy map[string]any
			if json.Unmarshal([]byte(value), &policy) == nil {
				if policy["enabled"] == false {
					delete(policy, "enabled")
				}
				encoded, _ := json.Marshal(policy)
				value = string(encoded)
			}
		}
		out[key] = value
	}
	out["A8S_GUEST_MANAGED_ONLY"] = "true"
	return out
}

func normalizePool(spec PoolSpec) PoolSpec {
	spec.MinReady = 0
	if spec.Environment.Launch != nil {
		launch := *spec.Environment.Launch
		launch.Environment = warmEnvironment(launch.Environment)
		spec.Environment.Launch = &launch
	}
	return spec
}

func poolRevision(spec PoolSpec, storageUID string) string {
	data, _ := json.Marshal(struct {
		Spec       PoolSpec
		StorageUID string
	}{normalizePool(spec), storageUID})
	return fmt.Sprintf("%x", sha256.Sum256(data))[:24]
}

func (w *WarmPool) load(ctx context.Context) (*core.ConfigMap, PoolSpec, poolState, string, error) {
	cm, err := w.Provider.config.Client.ConfigMaps(w.Provider.config.Namespace).Get(ctx, w.Name, meta.GetOptions{})
	if err != nil {
		return nil, PoolSpec{}, poolState{}, "", err
	}
	var spec PoolSpec
	var state poolState
	if err := json.Unmarshal([]byte(cm.Data["spec"]), &spec); err != nil {
		return nil, spec, state, "", err
	}
	if raw := cm.Data["state"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return nil, spec, state, "", err
		}
	}
	if spec.Scope != w.Scope || spec.MinReady < 0 || spec.MinReady > 100 || spec.Environment.Pool != w.Name || spec.DeploymentID == "" ||
		spec.Environment.Launch == nil || len(spec.Environment.Launch.Command) == 0 || spec.Environment.Launch.Command[0] != "/usr/local/bin/a8s-guest" {
		return nil, spec, state, "", fmt.Errorf("invalid warm pool scope, template or minReady (0..100)")
	}
	request := provider.PrepareRequest{Context: provider.OperationContext{Scope: spec.Scope, DeploymentID: spec.DeploymentID, InstanceID: "warm-validation", OperationID: "warm", RequestKey: "warm"}, Environment: spec.Environment, Workspace: spec.Workspace}
	ref, err := w.Provider.Plan(ctx, request)
	if err != nil {
		return nil, spec, state, "", err
	}
	return cm, spec, state, ref.StorageUID, nil
}

func warmReference(spec PoolSpec, storageUID, name, uid, namespace string) provider.ComputeReference {
	return provider.ComputeReference{ID: name, PodName: name, UID: uid, Namespace: namespace, StorageUID: storageUID,
		Request: provider.PrepareRequest{Context: provider.OperationContext{Scope: spec.Scope, DeploymentID: spec.DeploymentID, InstanceID: name, OperationID: "warm", RequestKey: "warm"}, Environment: spec.Environment, Workspace: spec.Workspace}}
}

func warmPod(spec PoolSpec, storageUID, name, namespace string) *core.Pod {
	spec = normalizePool(spec)
	ref := warmReference(spec, storageUID, name, "unassigned", namespace)
	pod := desiredPod(ref)
	delete(pod.Labels, "a8s.io/instance")
	pod.Labels[poolLabel] = spec.Environment.Pool
	data, _ := json.Marshal(spec)
	pod.Annotations = map[string]string{poolSpecKey: string(data), poolVersionKey: poolRevision(spec, storageUID), poolStorageKey: storageUID}
	pod.Finalizers = nil
	return pod
}

func (p *Provider) unclaimed(ctx context.Context, pod *core.Pod) error {
	if pod.UID == "" || pod.Labels[managedBy] != "a8s" || pod.Labels[poolLabel] == "" || pod.Labels["a8s.io/instance"] != "" || pod.Annotations[claimKey] != "" || len(pod.OwnerReferences) != 0 || len(pod.Finalizers) != 0 {
		return fault(provider.Conflict, "PoolPodClaimed", "Pod is claimed or not an owned free pool slot")
	}
	var spec PoolSpec
	if err := json.Unmarshal([]byte(pod.Annotations[poolSpecKey]), &spec); err != nil {
		return err
	}
	uid := pod.Annotations[poolStorageKey]
	if spec.Environment.Pool != pod.Labels[poolLabel] || uid == "" || pod.Annotations[poolVersionKey] != poolRevision(spec, uid) {
		return fault(provider.Conflict, "PoolTemplateMismatch", "free Pod template identity changed")
	}
	copy := pod.DeepCopy()
	ref := warmReference(spec, uid, pod.Name, string(pod.UID), pod.Namespace)
	copy.Labels["a8s.io/instance"] = pod.Name
	copy.Annotations[claimKey] = pod.Name
	copy.Annotations[intentKey] = fingerprint(ref)
	return checkPod(ref, copy)
}

func podReady(pod *core.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != core.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodReady {
			return condition.Status == core.ConditionTrue
		}
	}
	return false
}

// deleteFree is fenced against a concurrent claim: either its resourceVersion
// precondition wins or the claim patch wins. It never deletes a claimed Pod.
func (w *WarmPool) deleteFree(ctx context.Context, pod *core.Pod) error {
	if err := w.Provider.unclaimed(ctx, pod); err != nil {
		return err
	}
	return w.Provider.config.Client.Pods(pod.Namespace).Delete(ctx, pod.Name, meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}})
}

func (w *WarmPool) Reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, w.Provider.config.Timeout)
	defer cancel()
	cm, spec, state, storageUID, err := w.load(ctx)
	if err != nil {
		return err
	}
	revision := poolRevision(spec, storageUID)
	var slots []poolSlot
	pods := w.Provider.config.Client.Pods(w.Provider.config.Namespace)
	for _, slot := range state.Slots {
		pod, err := pods.Get(ctx, slot.Name, meta.GetOptions{})
		if apierrors.IsNotFound(err) {
			if slot.UID == "" && state.Revision == revision && len(slots) < spec.MinReady {
				slots = append(slots, slot)
			}
			continue
		}
		if err != nil {
			return err
		}
		if slot.UID != "" && slot.UID != string(pod.UID) {
			return fmt.Errorf("pool slot Pod UID changed: %s", slot.Name)
		}
		if pod.Annotations[claimKey] != "" {
			continue
		}
		if err := w.Provider.unclaimed(ctx, pod); err != nil {
			return err
		}
		if state.Revision != revision || len(slots) >= spec.MinReady || pod.DeletionTimestamp != nil || pod.Status.Phase == core.PodFailed || pod.Status.Phase == core.PodSucceeded {
			if pod.DeletionTimestamp == nil {
				if err := w.deleteFree(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
			continue
		}
		slot.UID = string(pod.UID)
		slots = append(slots, slot)
	}
	state.Slots, state.Revision = slots, revision
	for len(state.Slots) < spec.MinReady {
		if state.Sequence == ^uint64(0) {
			return fmt.Errorf("pool slot sequence exhausted")
		}
		state.Sequence++
		name := "warm-" + revision[:16] + "-" + strconv.FormatUint(state.Sequence, 16)
		state.Slots = append(state.Slots, poolSlot{Name: name})
	}
	encoded, _ := json.Marshal(state)
	if cm.Data["state"] != string(encoded) {
		copy := cm.DeepCopy()
		copy.Data["state"] = string(encoded)
		if _, err := w.Provider.config.Client.ConfigMaps(cm.Namespace).Update(ctx, copy, meta.UpdateOptions{}); err != nil {
			return err
		}
	}
	// Tickets are durable now. A timeout can only retry this same Pod name.
	for _, slot := range state.Slots {
		if slot.UID != "" {
			continue
		}
		_, err := pods.Create(ctx, warmPod(spec, storageUID, slot.Name, cm.Namespace), meta.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	// Clean late creations from a stale reconciler. Claimed Pods are retained.
	list, err := pods.List(ctx, meta.ListOptions{LabelSelector: poolLabel + "=" + w.Name})
	if err != nil {
		return err
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if pod.Annotations[claimKey] == "" && pod.DeletionTimestamp == nil {
			latest, err := w.Provider.config.Client.ConfigMaps(cm.Namespace).Get(ctx, cm.Name, meta.GetOptions{})
			if err != nil {
				return err
			}
			var current poolState
			if err := json.Unmarshal([]byte(latest.Data["state"]), &current); err != nil {
				return err
			}
			keep := false
			for _, slot := range current.Slots {
				if slot.Name == pod.Name {
					keep = true
					break
				}
			}
			if keep {
				continue
			}
			if err := w.deleteFree(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (w *WarmPool) Status(ctx context.Context) (PoolStatus, error) {
	cm, spec, state, uid, err := w.load(ctx)
	if err != nil {
		return PoolStatus{}, err
	}
	_ = cm
	status := PoolStatus{MinReady: spec.MinReady, Revision: poolRevision(spec, uid)}
	list, err := w.Provider.config.Client.Pods(w.Provider.config.Namespace).List(ctx, meta.ListOptions{LabelSelector: poolLabel + "=" + w.Name})
	if err != nil {
		return status, err
	}
	tickets := map[string]string{}
	for _, slot := range state.Slots {
		tickets[slot.Name] = slot.UID
		if slot.UID == "" {
			status.Warming++
		}
	}
	for i := range list.Items {
		pod := &list.Items[i]
		if pod.Annotations[claimKey] != "" {
			status.Claimed++
			continue
		}
		if tickets[pod.Name] != string(pod.UID) || pod.Annotations[poolVersionKey] != status.Revision || pod.DeletionTimestamp != nil {
			continue
		}
		if err := w.Provider.unclaimed(ctx, pod); err != nil {
			status.Error = err.Error()
			continue
		}
		if podReady(pod) {
			status.Ready++
		} else {
			status.Warming++
		}
	}
	return status, nil
}

func (w *WarmPool) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := w.Reconcile(ctx); err != nil && !apierrors.IsConflict(err) && ctx.Err() == nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func validPoolName(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }
