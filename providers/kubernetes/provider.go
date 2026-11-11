// Package kubernetes manages dedicated Pods and Services using native Kubernetes.
// Workspace PVCs outlive compute. There is no checkpoint or automatic failover.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	provider "github.com/jingyugao/a8s/providers/contract"
	digest "github.com/opencontainers/go-digest"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	typed "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	finalizer = "a8s.io/retain-stop-evidence"
	intentKey = "a8s.io/intent"
	managedBy = "app.kubernetes.io/managed-by"
)

// Config contains operator policy; credentials and Kubernetes client setup
// remain in application assembly. Images must start their own exec-server.
type Config struct {
	Client    typed.CoreV1Interface
	Namespace string
	Port      int32 // zero selects the exec-server port 8095
	Timeout   time.Duration
}

type Provider struct{ config Config }

func New(config Config) (*Provider, error) {
	if config.Port == 0 {
		config.Port = 8095
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	if config.Client == nil || len(validation.IsDNS1123Label(config.Namespace)) != 0 || config.Port < 1 || config.Port > 65535 || config.Timeout < 0 {
		return nil, fault(provider.Invalid, "InvalidConfig", "Kubernetes client, namespace, valid port and positive timeout are required")
	}
	return &Provider{config: config}, nil
}

func (*Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{OperationLookup: true, PersistentVolumes: true, WriterReleaseProof: true}
}

func (p *Provider) ResolveEnvironment(_ context.Context, request provider.EnvironmentRequest) (provider.Environment, error) {
	name, hash, ok := strings.Cut(request.Image, "@")
	if !ok || name == "" || strings.ContainsAny(name, " \t\r\n?#") || digest.Digest(hash).Validate() != nil ||
		request.RevisionID == "" || request.CPU < 0 || math.IsNaN(request.CPU) || math.IsInf(request.CPU, 0) || request.MemoryMiB < 0 || request.MemoryMiB > math.MaxInt64/(1<<20) {
		return provider.Environment{}, fault(provider.Invalid, "InvalidEnvironment", "fixed image digest, runtime revision and valid resource limits are required")
	}
	if request.Platform != "" && request.Platform != "linux/amd64" && request.Platform != "linux/arm64" {
		return provider.Environment{}, fault(provider.Unsupported, "UnsupportedPlatform", "native provider supports linux/amd64 or linux/arm64")
	}
	if request.Pool != "" && len(validation.IsDNS1123Subdomain(request.Pool)) != 0 {
		return provider.Environment{}, fault(provider.Invalid, "InvalidPool", "valid pool configuration name is required")
	}
	return provider.Environment{Provider: "kubernetes", Image: request.Image, RevisionID: request.RevisionID,
		Platform: request.Platform, CPU: request.CPU, MemoryMiB: request.MemoryMiB, Port: p.config.Port, Launch: request.Launch, Pool: request.Pool}, nil
}

func referenceID(request provider.PrepareRequest) string {
	c := request.Context
	if request.Environment.Launch != nil {
		return strings.ToLower(c.InstanceID)
	}
	data, _ := json.Marshal([]string{c.Scope, c.DeploymentID, c.InstanceID, c.OperationID, c.RequestKey})
	hash := sha256.Sum256(data)
	return "a8s-" + hex.EncodeToString(hash[:24])
}

func fingerprint(ref provider.ComputeReference) string {
	data, _ := json.Marshal(struct {
		Request            provider.PrepareRequest
		Storage, Namespace string
	}{ref.Request, ref.StorageUID, ref.Namespace})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func endpoint(ref provider.ComputeReference) string {
	return fmt.Sprintf("ws://%s.%s.svc:%d/", ref.ID, ref.Namespace, ref.Request.Environment.Port)
}

func (p *Provider) validate(ref provider.ComputeReference) error {
	c, e, w := ref.Request.Context, ref.Request.Environment, ref.Request.Workspace
	if c.Scope == "" || c.DeploymentID == "" || c.InstanceID == "" || c.OperationID == "" || c.RequestKey == "" || w.ID == "" ||
		len(validation.IsDNS1123Subdomain(w.StorageReference)) != 0 || !path.IsAbs(w.MountPath) || path.Clean(w.MountPath) != w.MountPath || w.MountPath == "/" ||
		strings.IndexFunc(w.MountPath, func(c rune) bool { return c < 32 || c == 127 }) >= 0 {
		return fault(provider.Invalid, "InvalidIntent", "persisted resource identity, existing PVC and clean absolute mount path are required")
	}
	resolved, err := p.ResolveEnvironment(context.Background(), provider.EnvironmentRequest{Image: e.Image, RevisionID: e.RevisionID, Platform: e.Platform, CPU: e.CPU, MemoryMiB: e.MemoryMiB, Launch: e.Launch, Pool: e.Pool})
	if err != nil {
		return err
	}
	// Port is part of the persisted environment; configuration changes must not
	// silently change existing instances' endpoints.
	resolved.Port = e.Port
	if !reflect.DeepEqual(resolved, e) || e.Port < 1 || e.Port > 65535 || len(validation.IsDNS1123Label(ref.ID)) != 0 || ref.Namespace != p.config.Namespace || ref.ID != referenceID(ref.Request) || ref.StorageUID == "" || ref.Endpoint != endpoint(ref) {
		return fault(provider.Conflict, "ReferenceMismatch", "compute reference differs from its immutable native Kubernetes intent")
	}
	if ref.PodName != "" && (e.Pool == "" || ref.UID == "" || len(validation.IsDNS1123Label(ref.PodName)) != 0) {
		return fault(provider.Invalid, "InvalidPooledReference", "pooled Pod name, UID and pool identity must be persisted together")
	}
	return nil
}

func podName(ref provider.ComputeReference) string {
	if ref.PodName != "" {
		return ref.PodName
	}
	return ref.ID
}

func (p *Provider) Plan(ctx context.Context, request provider.PrepareRequest) (provider.ComputeReference, error) {
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	ref := provider.ComputeReference{ID: referenceID(request), Namespace: p.config.Namespace, Request: request}
	ref.Endpoint = endpoint(ref)
	// Validate structure before using caller input in a resource lookup.
	ref.StorageUID = "planning"
	if err := p.validate(ref); err != nil {
		return provider.ComputeReference{}, err
	}
	pvc, err := p.config.Client.PersistentVolumeClaims(ref.Namespace).Get(ctx, request.Workspace.StorageReference, meta.GetOptions{})
	if err != nil {
		return ref, wrap(err, false, "", request.Workspace.StorageReference)
	}
	if pvc.UID == "" || pvc.DeletionTimestamp != nil || pvc.Status.Phase == core.ClaimLost {
		return ref, fault(provider.NotReady, "WorkspaceUnavailable", "Workspace PVC is missing its identity, being deleted or lost")
	}
	ref.StorageUID = string(pvc.UID)
	return ref, nil
}

func (p *Provider) workspace(ctx context.Context, ref provider.ComputeReference) (*core.PersistentVolumeClaim, error) {
	pvc, err := p.config.Client.PersistentVolumeClaims(ref.Namespace).Get(ctx, ref.Request.Workspace.StorageReference, meta.GetOptions{})
	if err != nil {
		return nil, wrap(err, false, "", ref.Request.Workspace.StorageReference)
	}
	if string(pvc.UID) != ref.StorageUID {
		return nil, fault(provider.Conflict, "WorkspaceIdentityChanged", "Workspace PVC was replaced with a different UID")
	}
	return pvc, nil
}

func desiredPod(ref provider.ComputeReference) *core.Pod {
	e, w := ref.Request.Environment, ref.Request.Workspace
	no := false
	grace := int64(30)
	limits := core.ResourceList{}
	if e.CPU != 0 {
		limits[core.ResourceCPU] = resource.MustParse(strconv.FormatFloat(e.CPU, 'f', -1, 64))
	}
	if e.MemoryMiB != 0 {
		limits[core.ResourceMemory] = *resource.NewQuantity(e.MemoryMiB<<20, resource.BinarySI)
	}
	selector := map[string]string{"kubernetes.io/os": "linux"}
	if _, arch, ok := strings.Cut(e.Platform, "/"); ok {
		selector["kubernetes.io/arch"] = arch
	}
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: podName(ref), Namespace: ref.Namespace,
		Labels: map[string]string{managedBy: "a8s", "a8s.io/instance": ref.ID}, Annotations: map[string]string{intentKey: fingerprint(ref)}, Finalizers: []string{finalizer}},
		Spec: core.PodSpec{RestartPolicy: core.RestartPolicyNever, AutomountServiceAccountToken: &no, TerminationGracePeriodSeconds: &grace, NodeSelector: selector,
			Volumes: []core.Volume{{Name: "workspace", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: w.StorageReference}}}},
			Containers: []core.Container{{Name: "executor", Image: e.Image, ImagePullPolicy: core.PullIfNotPresent,
				Resources: core.ResourceRequirements{Limits: limits}, Ports: []core.ContainerPort{{Name: "executor", ContainerPort: e.Port}},
				VolumeMounts:   []core.VolumeMount{{Name: "workspace", MountPath: w.MountPath}},
				ReadinessProbe: &core.Probe{ProbeHandler: core.ProbeHandler{TCPSocket: &core.TCPSocketAction{Port: intstr.FromInt32(e.Port)}}, PeriodSeconds: 2}}}}}
	if launch := e.Launch; launch != nil {
		if ref.PodName != "" {
			copy := *launch
			copy.Environment = warmEnvironment(launch.Environment)
			copy.Environment["A8S_GUEST_SLOT"] = ref.PodName
			launch = &copy
		}
		container := &pod.Spec.Containers[0]
		if len(launch.Command) > 0 && launch.Command[0] == "/usr/local/bin/a8s-guest" {
			root := int64(0)
			container.SecurityContext = &core.SecurityContext{RunAsUser: &root, RunAsGroup: &root, AllowPrivilegeEscalation: &no,
				Capabilities:   &core.Capabilities{Drop: []core.Capability{"ALL"}, Add: []core.Capability{"SETUID", "SETGID", "SETPCAP", "KILL", "CHOWN", "FOWNER", "DAC_OVERRIDE"}},
				SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}}
			container.StartupProbe = &core.Probe{ProbeHandler: core.ProbeHandler{HTTPGet: &core.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(launch.ControlPort)}}, PeriodSeconds: 1, FailureThreshold: 180}
			container.ReadinessProbe = &core.Probe{ProbeHandler: container.StartupProbe.ProbeHandler, PeriodSeconds: 1}
		}
		container.Command = launch.Command
		keys := make([]string, 0, len(launch.Environment))
		for key := range launch.Environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			container.Env = append(container.Env, core.EnvVar{Name: key, Value: launch.Environment[key]})
		}
		for _, name := range launch.SecretRefs {
			container.EnvFrom = append(container.EnvFrom, core.EnvFromSource{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: name}}})
		}
		if launch.ConfigMap != "" {
			pod.Spec.Volumes = append(pod.Spec.Volumes, core.Volume{Name: "host-config", VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: launch.ConfigMap}}}})
			container.VolumeMounts = append(container.VolumeMounts, core.VolumeMount{Name: "host-config", MountPath: "/etc/codex", ReadOnly: true})
		}
		if launch.ControlPort != 0 {
			container.Ports = append(container.Ports, core.ContainerPort{Name: "agentc", ContainerPort: launch.ControlPort})
			if container.ReadinessProbe.TCPSocket != nil {
				container.ReadinessProbe.TCPSocket.Port = intstr.FromInt32(launch.ControlPort)
			}
		}
	}
	return pod
}

func checkPod(ref provider.ComputeReference, pod *core.Pod) error {
	want := desiredPod(ref)
	if ref.PodName != "" && (pod.Annotations[claimKey] != ref.ID || pod.Labels[poolLabel] != ref.Request.Environment.Pool || pod.Annotations[poolStorageKey] != ref.StorageUID) {
		return fault(provider.Conflict, "PoolClaimMissing", "pooled reference has no matching claim")
	}
	if pod.UID == "" || pod.Name != podName(ref) || pod.Namespace != ref.Namespace || ref.UID != "" && string(pod.UID) != ref.UID ||
		pod.Labels[managedBy] != "a8s" || pod.Labels["a8s.io/instance"] != ref.ID || pod.Annotations[intentKey] != fingerprint(ref) || pod.Spec.RestartPolicy != core.RestartPolicyNever ||
		len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return fault(provider.Conflict, "PodIdentityMismatch", "Pod UID, ownership or immutable intent differs from the requested Instance")
	}
	c, expected := pod.Spec.Containers[0], want.Spec.Containers[0]
	if c.Name != expected.Name || c.Image != expected.Image || !reflect.DeepEqual(c.Command, expected.Command) || !reflect.DeepEqual(c.Args, expected.Args) || !reflect.DeepEqual(c.Env, expected.Env) || !reflect.DeepEqual(c.EnvFrom, expected.EnvFrom) ||
		!reflect.DeepEqual(c.SecurityContext, expected.SecurityContext) || !reflect.DeepEqual(c.VolumeMounts, expected.VolumeMounts) || !equalResources(c.Resources.Limits, expected.Resources.Limits) ||
		!reflect.DeepEqual(pod.Spec.NodeSelector, want.Spec.NodeSelector) || !equalVolumes(pod.Spec.Volumes, want.Spec.Volumes) {
		return fault(provider.Conflict, "PodSpecMismatch", "Pod image, resources, entrypoint or Workspace mount differs from the fixed intent")
	}
	return nil
}

func equalVolumes(actual, expected []core.Volume) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range actual {
		a, b := actual[i], expected[i]
		// Kubernetes defaults a ConfigMap's mode on admission.
		if a.ConfigMap != nil {
			value := *a.ConfigMap
			value.DefaultMode = nil
			a.ConfigMap = &value
		}
		if b.ConfigMap != nil {
			value := *b.ConfigMap
			value.DefaultMode = nil
			b.ConfigMap = &value
		}
		if !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

func equalResources(a, b core.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		other, ok := b[name]
		if !ok || value.Cmp(other) != 0 {
			return false
		}
	}
	return true
}

func (p *Provider) Prepare(ctx context.Context, ref provider.ComputeReference) (provider.Operation, error) {
	op := operation(ref, provider.PrepareCompute)
	if err := p.validate(ref); err != nil {
		return op, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	if ref.Request.Environment.Pool != "" {
		var err error
		ref, err = p.allocate(ctx, ref)
		if err != nil {
			return op, wrap(err, false, "", ref.ID)
		}
		op = operation(ref, provider.PrepareCompute)
	}
	pvc, err := p.workspace(ctx, ref)
	if err != nil {
		return op, err
	}
	if pvc.DeletionTimestamp != nil || pvc.Status.Phase == core.ClaimLost {
		return op, fault(provider.NotReady, "WorkspaceUnavailable", "Workspace PVC is being deleted or lost")
	}
	pod, err := p.config.Client.Pods(ref.Namespace).Get(ctx, podName(ref), meta.GetOptions{})
	if apierrors.IsNotFound(err) && ref.UID == "" {
		if err := p.noOtherWriter(ctx, ref); err != nil {
			return op, err
		}
		pod, err = p.config.Client.Pods(ref.Namespace).Create(ctx, desiredPod(ref), meta.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			pod, err = p.config.Client.Pods(ref.Namespace).Get(ctx, ref.ID, meta.GetOptions{})
		}
		if err != nil {
			return op, wrap(err, true, "", ref.ID)
		}
	} else if err != nil {
		return op, unknown(err, "", ref.ID)
	}
	return p.prepared(ctx, ref, pod)
}

func (p *Provider) prepared(ctx context.Context, ref provider.ComputeReference, pod *core.Pod) (provider.Operation, error) {
	op := operation(ref, provider.PrepareCompute)
	if err := checkPod(ref, pod); err != nil {
		return op, err
	}
	ref.UID = string(pod.UID)
	op = operation(ref, provider.PrepareCompute)
	// Repair only routing for an already accepted, owned Pod after a crash.
	// An inspection never recreates missing execution compute.
	if pod.DeletionTimestamp == nil {
		if err := p.ensureService(ctx, ref); err != nil {
			return op, err
		}
	}
	observation, err := p.observe(ctx, ref, pod)
	if err != nil {
		return op, err
	}
	op.Observation = &observation
	if observation.Available {
		op.Status = provider.Succeeded
	} else if pod.Status.Phase == core.PodFailed || pod.Status.Phase == core.PodSucceeded {
		op.Status = provider.Failed
		op.Failure = fault(provider.NotReady, "ExecutorExited", "execution Pod exited before environment readiness")
	} else {
		op.Status = provider.Running
	}
	return op, nil
}

func (p *Provider) Inspect(ctx context.Context, ref provider.ComputeReference) (provider.Observation, error) {
	if err := p.validate(ref); err != nil {
		return provider.Observation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	ref, err := p.allocation(ctx, ref)
	if err != nil {
		return provider.Observation{}, err
	}
	pod, err := p.config.Client.Pods(ref.Namespace).Get(ctx, podName(ref), meta.GetOptions{})
	if err != nil {
		return provider.Observation{}, unknown(err, "", ref.ID)
	}
	return p.observe(ctx, ref, pod)
}

var _ provider.Provider = (*Provider)(nil)
