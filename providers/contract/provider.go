package contract

import (
	"context"
	"fmt"
	"time"
)

// Provider operates on dedicated compute and existing persistent storage.
// Control owns admission, draining, native history and the decision to switch.
// Callers persist the request and operation reference before advancing a release.
// Prepare may start writers; Control must first establish that any previous
// holder has stopped or been fenced. Capability absence never implies safety.
type Provider interface {
	Capabilities() Capabilities
	ResolveEnvironment(context.Context, EnvironmentRequest) (Environment, error)
	// Plan performs reads only. Persist its result before submitting Prepare.
	Plan(context.Context, PrepareRequest) (ComputeReference, error)
	Prepare(context.Context, ComputeReference) (Operation, error)
	Inspect(context.Context, ComputeReference) (Observation, error)
	InspectOperation(context.Context, OperationReference) (Operation, error)
	StopCompute(context.Context, ComputeRequest) (Operation, error)
	ReleaseCompute(context.Context, ComputeRequest) (Operation, error)
}

type Capabilities struct {
	// ReservedComputeIDs means an ID has durable uniqueness, including after
	// deletion. It does not mean repeated HTTP requests replay an operation.
	ReservedComputeIDs bool `json:"reservedComputeIds"`
	OperationLookup    bool `json:"operationLookup"`
	PersistentVolumes  bool `json:"persistentVolumes"`
	WriterReleaseProof bool `json:"writerReleaseProof"`
	Suspend            bool `json:"suspend"`
	Resume             bool `json:"resume"`
}

// EnvironmentRequest fixes the image and native runtime configuration version.
type EnvironmentRequest struct {
	Image      string `json:"image"`
	RevisionID string `json:"revisionId"`
	Platform   string `json:"platform,omitempty"`
	// Zero leaves the resource limit unset.
	CPU       float64     `json:"cpu,omitempty"`
	MemoryMiB int64       `json:"memoryMiB,omitempty"`
	Launch    *LaunchSpec `json:"launch,omitempty"`
	Pool      string      `json:"pool,omitempty"`
}

// LaunchSpec is immutable, credential-free process configuration supplied by application assembly.
type LaunchSpec struct {
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	SecretRefs  []string          `json:"secretRefs,omitempty"`
	ConfigMap   string            `json:"configMap,omitempty"`
	ControlPort int32             `json:"controlPort,omitempty"`
}

// Environment is safe to persist in a Release; it contains no access tokens.
type Environment struct {
	Provider   string      `json:"provider"`
	Image      string      `json:"image"`
	RevisionID string      `json:"revisionId"`
	Platform   string      `json:"platform,omitempty"`
	CPU        float64     `json:"cpu,omitempty"`
	MemoryMiB  int64       `json:"memoryMiB,omitempty"`
	Port       int32       `json:"port"`
	Launch     *LaunchSpec `json:"launch,omitempty"`
	Pool       string      `json:"pool,omitempty"`
}

// OperationContext identifies the host's persisted lifecycle intent.
// Scope is an opaque, canonical tenant/project identity supplied by the host.
// It participates in resource identity; authentication belongs to API assembly.
type OperationContext struct {
	Scope        string `json:"scope"`
	DeploymentID string `json:"deploymentId"`
	InstanceID   string `json:"instanceId"`
	OperationID  string `json:"operationId"`
	RequestKey   string `json:"requestKey"`
}

// WorkspaceBinding references existing independently owned storage. The
// caller provisions/imports storage explicitly; compute never deletes it.
type WorkspaceBinding struct {
	ID               string `json:"id"`
	StorageReference string `json:"storageReference"`
	MountPath        string `json:"mountPath"`
}

type PrepareRequest struct {
	Context     OperationContext `json:"context"`
	Environment Environment      `json:"environment"`
	Workspace   WorkspaceBinding `json:"workspace"`
}

// ComputeReference is a durable reconstruction of the original create intent.
// UID fences later mutations against replacement resources with the same name.
type ComputeReference struct {
	ID         string         `json:"id"`
	UID        string         `json:"uid,omitempty"`
	PodName    string         `json:"podName,omitempty"`
	Namespace  string         `json:"namespace"`
	StorageUID string         `json:"storageUid"`
	Endpoint   string         `json:"endpoint"`
	Request    PrepareRequest `json:"request"`
}

type ComputeRequest struct {
	Target     ComputeReference `json:"target"`
	RequestKey string           `json:"requestKey"`
	// ExternalID is the original accepted operation, when known. Supplying it
	// makes recovery query-only; a missing operation is an unknown outcome.
	ExternalID string `json:"externalId,omitempty"`
}

type OperationKind string

const (
	PrepareCompute OperationKind = "prepare"
	StopCompute    OperationKind = "stop"
	ReleaseCompute OperationKind = "release"
)

type OperationStatus string

const (
	Pending   OperationStatus = "Pending"
	Running   OperationStatus = "Running"
	Succeeded OperationStatus = "Succeeded"
	Failed    OperationStatus = "Failed"
	Unknown   OperationStatus = "Unknown"
	Blocked   OperationStatus = "Blocked"
)

type OperationReference struct {
	ID     string           `json:"id,omitempty"`
	Kind   OperationKind    `json:"kind"`
	Target ComputeReference `json:"target"`
}

type Operation struct {
	Reference   OperationReference `json:"reference"`
	Status      OperationStatus    `json:"status"`
	Stage       string             `json:"stage,omitempty"`
	Observation *Observation       `json:"observation,omitempty"`
	Failure     *Error             `json:"failure,omitempty"`
}

type EvidenceState string

const (
	EvidenceUnknown  EvidenceState = "Unknown"
	EvidenceHeld     EvidenceState = "Held"
	EvidenceReleased EvidenceState = "Released"
)

// WriterEvidence separates compute completion from a storage release proof.
// Only Released authorizes Control to consider the storage takeover gate.
type WriterEvidence struct {
	State          EvidenceState `json:"state"`
	ComputeStopped bool          `json:"computeStopped"`
	OperationID    string        `json:"operationId,omitempty"`
	ObservedAt     time.Time     `json:"observedAt"`
	Reason         string        `json:"reason"`
}

// Access describes the instance's execution endpoint, never credentials. Runtime auth
// is supplied separately by the application using its credential references.
type Access struct {
	Endpoint string `json:"endpoint"`
}

// Observation is the provider's last known projection, not Agentc readiness.
// ObservedAt is the source observation time when available, not a fresh liveness
// guarantee manufactured from an API read.
type Observation struct {
	Reference           ComputeReference `json:"reference"`
	Phase               string           `json:"phase"`
	Available           bool             `json:"available"`
	ResourceVersion     string           `json:"resourceVersion"`
	ImageID             string           `json:"imageId,omitempty"`
	ObservedAt          time.Time        `json:"observedAt"`
	ExternalOperationID string           `json:"externalOperationId,omitempty"`
	WorkspaceRetained   bool             `json:"workspaceRetained"`
	StoragePhase        string           `json:"storagePhase"`
	Writer              WriterEvidence   `json:"writer"`
	Access              Access           `json:"access"`
	Failure             *Error           `json:"failure,omitempty"`
}

type ErrorCategory string

const (
	Invalid               ErrorCategory = "Invalid"
	Incompatible          ErrorCategory = "Incompatible"
	Forbidden             ErrorCategory = "Forbidden"
	NotFound              ErrorCategory = "NotFound"
	Conflict              ErrorCategory = "Conflict"
	NotReady              ErrorCategory = "NotReady"
	Unsupported           ErrorCategory = "Unsupported"
	DependencyUnavailable ErrorCategory = "DependencyUnavailable"
	OutcomeUnknown        ErrorCategory = "OutcomeUnknown"
)

// Error keeps a safe public message and original downstream code/cause. Cause
// is deliberately excluded from JSON; errors.Is/As can still inspect it.
type Error struct {
	Category    ErrorCategory `json:"category"`
	Source      string        `json:"source"`
	Code        string        `json:"code"`
	Message     string        `json:"message"`
	OperationID string        `json:"operationId,omitempty"`
	ResourceID  string        `json:"resourceId,omitempty"`
	HTTPStatus  int           `json:"httpStatus,omitempty"`
	Cause       error         `json:"-"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Source, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }
