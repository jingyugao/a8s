package kubernetes

import (
	"context"
	"reflect"
	"strings"

	provider "github.com/jingyugao/a8s/providers/contract"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func desiredService(ref provider.ComputeReference) *core.Service {
	yes := true
	service := &core.Service{ObjectMeta: meta.ObjectMeta{Name: ref.ID, Namespace: ref.Namespace,
		Labels: map[string]string{managedBy: "a8s"}, Annotations: map[string]string{intentKey: fingerprint(ref)},
		OwnerReferences: []meta.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: podName(ref), UID: types.UID(ref.UID), Controller: &yes}}},
		Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, Selector: map[string]string{"a8s.io/instance": ref.ID},
			Ports: []core.ServicePort{{Name: "executor", Protocol: core.ProtocolTCP, Port: ref.Request.Environment.Port, TargetPort: intstr.FromInt32(ref.Request.Environment.Port)}}}}
	if launch := ref.Request.Environment.Launch; launch != nil && launch.ControlPort != 0 {
		service.Spec.Ports = append(service.Spec.Ports, core.ServicePort{Name: "agentc", Protocol: core.ProtocolTCP, Port: launch.ControlPort, TargetPort: intstr.FromInt32(launch.ControlPort)})
	}
	return service
}

func checkService(ref provider.ComputeReference, service *core.Service) error {
	want := desiredService(ref)
	owner := meta.GetControllerOf(service)
	if service.Name != ref.ID || service.Namespace != ref.Namespace || service.Labels[managedBy] != "a8s" || service.Annotations[intentKey] != fingerprint(ref) ||
		owner == nil || owner.UID != types.UID(ref.UID) || owner.Kind != "Pod" || owner.Name != podName(ref) || owner.APIVersion != "v1" ||
		service.Spec.Type != core.ServiceTypeClusterIP || !reflect.DeepEqual(service.Spec.Selector, want.Spec.Selector) || !reflect.DeepEqual(service.Spec.Ports, want.Spec.Ports) {
		return fault(provider.Conflict, "ServiceIdentityMismatch", "Service ownership, selector or endpoint differs from the fixed Instance")
	}
	return nil
}

func (p *Provider) ensureService(ctx context.Context, ref provider.ComputeReference) error {
	services := p.config.Client.Services(ref.Namespace)
	service, err := services.Get(ctx, ref.ID, meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		service, err = services.Create(ctx, desiredService(ref), meta.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			service, err = services.Get(ctx, ref.ID, meta.GetOptions{})
		}
		if err != nil {
			return wrap(err, true, "prepare:"+ref.UID, ref.ID)
		}
	} else if err != nil {
		return wrap(err, false, "prepare:"+ref.UID, ref.ID)
	}
	if err := checkService(ref, service); err != nil {
		return err
	}
	if service.DeletionTimestamp != nil {
		return fault(provider.NotReady, "ServiceDeleting", "Instance Service is still being removed")
	}
	return nil
}

// A terminal phase alone can be assigned by the control plane on node loss.
// Require real container exit records, plus a currently Ready node. This covers
// ordinary managed termination; it does not implement node/storage fencing.
func (p *Provider) writerStopped(ctx context.Context, pod *core.Pod) (bool, error) {
	if pod.DeletionTimestamp != nil && pod.Spec.NodeName == "" && len(pod.Status.ContainerStatuses) == 0 {
		return true, nil // an unscheduled, deleting Pod cannot start containers
	}
	if pod.Status.Phase != core.PodFailed && pod.Status.Phase != core.PodSucceeded || pod.Status.Reason == "NodeLost" ||
		len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return false, nil
	}
	for _, status := range pod.Status.ContainerStatuses {
		found := false
		for _, container := range pod.Spec.Containers {
			found = found || container.Name == status.Name
		}
		exit := status.State.Terminated
		if !found || exit == nil || exit.FinishedAt.IsZero() || exit.Reason == "ContainerStatusUnknown" || exit.ContainerID == "" && status.ContainerID == "" {
			return false, nil
		}
	}
	if pod.Spec.NodeName == "" {
		return false, nil
	}
	node, err := p.config.Client.Nodes().Get(ctx, pod.Spec.NodeName, meta.GetOptions{})
	if err != nil {
		return false, wrap(err, false, "", pod.Spec.NodeName)
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == core.NodeReady {
			return condition.Status == core.ConditionTrue && node.DeletionTimestamp == nil, nil
		}
	}
	return false, nil
}

func (p *Provider) noOtherWriter(ctx context.Context, ref provider.ComputeReference) error {
	pods, err := p.config.Client.Pods(ref.Namespace).List(ctx, meta.ListOptions{})
	if err != nil {
		return wrap(err, false, "", ref.ID)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name == podName(ref) {
			if ref.PodName != "" && string(pod.UID) == ref.UID && p.unclaimed(ctx, pod) == nil {
				continue
			}
			if err := checkPod(ref, pod); err != nil {
				return err
			}
			continue
		}
		if pod.Labels[poolLabel] != "" && pod.Annotations[claimKey] == "" {
			if err := p.unclaimed(ctx, pod); err == nil {
				continue
			}
		}
		for _, volume := range pod.Spec.Volumes {
			claim := volume.PersistentVolumeClaim
			if claim == nil || claim.ClaimName != ref.Request.Workspace.StorageReference || claim.ReadOnly {
				continue
			}
			stopped, err := p.writerStopped(ctx, pod)
			if err != nil {
				return err
			}
			if !stopped {
				return fault(provider.NotReady, "WorkspaceWriterPresent", "another Pod may still write this Workspace PVC; takeover is blocked")
			}
		}
	}
	return nil
}

func (p *Provider) observe(ctx context.Context, ref provider.ComputeReference, pod *core.Pod) (provider.Observation, error) {
	if err := checkPod(ref, pod); err != nil {
		return provider.Observation{}, err
	}
	ref.UID = string(pod.UID)
	pvc, err := p.workspace(ctx, ref)
	if err != nil {
		return provider.Observation{}, err
	}
	observation := provider.Observation{Reference: ref, Phase: strings.ToLower(string(pod.Status.Phase)), ResourceVersion: pod.ResourceVersion,
		WorkspaceRetained: pvc.DeletionTimestamp == nil && pvc.Status.Phase != core.ClaimLost,
		StoragePhase:      string(pvc.Status.Phase), Access: provider.Access{Endpoint: ref.Endpoint},
		Writer: provider.WriterEvidence{State: provider.EvidenceUnknown, Reason: "waiting for container termination on a Ready node; node loss and API absence are not stop evidence"}}
	if pod.DeletionTimestamp != nil {
		observation.Phase = "stopping"
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == core.PodReady {
			observation.Available = condition.Status == core.ConditionTrue && pod.DeletionTimestamp == nil && pvc.Status.Phase == core.ClaimBound && observation.WorkspaceRetained
			observation.ObservedAt = condition.LastTransitionTime.Time
		}
	}
	if len(pod.Status.ContainerStatuses) == 1 {
		status := pod.Status.ContainerStatuses[0]
		observation.ImageID = status.ImageID
		observation.Available = observation.Available && status.Ready && status.State.Running != nil
		if status.State.Running != nil {
			observation.Writer.State = provider.EvidenceHeld
		}
	} else {
		observation.Available = false
	}
	stopped, err := p.writerStopped(ctx, pod)
	if err != nil {
		return observation, err
	}
	if stopped {
		observation.Phase = "stopped"
		observation.Writer.State, observation.Writer.ComputeStopped = provider.EvidenceReleased, true
		observation.Writer.OperationID = "stop:" + ref.UID
		observation.Writer.Reason = "this managed Pod's containers exited on a Ready node; Workspace PVC is retained"
		for _, status := range pod.Status.ContainerStatuses {
			if exit := status.State.Terminated; exit != nil && exit.FinishedAt.Time.After(observation.Writer.ObservedAt) {
				observation.Writer.ObservedAt = exit.FinishedAt.Time
			}
		}
	}
	if observation.Available {
		service, err := p.config.Client.Services(ref.Namespace).Get(ctx, ref.ID, meta.GetOptions{})
		if apierrors.IsNotFound(err) {
			observation.Available = false
		} else if err != nil {
			return observation, wrap(err, false, "", ref.ID)
		} else if err := checkService(ref, service); err != nil {
			return observation, err
		} else {
			observation.Available = service.DeletionTimestamp == nil
		}
	}
	return observation, nil
}
