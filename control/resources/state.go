package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"google.golang.org/protobuf/proto"
)

// State is the atomic persistence boundary for one Deployment and Workspace.
// No remote calls are made while saving it. Snapshots and finished operations
// remain available for rollback and request-key replay.
type State struct {
	Deployment *v1.Deployment           `json:"deployment"`
	Workspace  *v1.Workspace            `json:"workspace"`
	Target     *v1.ExecutionTarget      `json:"target"`
	Releases   map[string]*v1.Release   `json:"releases"`
	Operations map[string]*v1.Operation `json:"operations"`
	Instances  map[string]*v1.Instance  `json:"instances"`
	Requests   map[string]RequestRecord `json:"requests"`
	Intents    map[string]*Intent       `json:"intents"`
}

type RequestRecord struct {
	Fingerprint string `json:"fingerprint"`
	OperationID string `json:"operation_id"`
}

// Intent is recorded before dispatch. Results are persisted before advancing.
type Intent struct {
	OperationID string       `json:"operation_id"`
	Step        string       `json:"step"`
	RequestKey  string       `json:"request_key"`
	InstanceID  string       `json:"instance_id"`
	Epoch       uint64       `json:"epoch"`
	Result      *Observation `json:"result,omitempty"`
}

// Observation is a Control projection, not a second provider/Agentc protocol.
// Adapters translate their own public contracts into this evidence.
type Observation struct {
	Capabilities      []string `json:"capabilities,omitempty"`
	Found             bool     `json:"found"`
	Done              bool     `json:"done"`
	ExternalID        string   `json:"external_id"`
	ComputeReference  string   `json:"compute_reference"`
	StorageReference  string   `json:"storage_reference"`
	Binding           string   `json:"binding"`
	InstanceID        string   `json:"instance_id"`
	WorkspaceID       string   `json:"workspace_id"`
	Epoch             uint64   `json:"epoch"`
	PackageDigest     string   `json:"package_digest"`
	AgentcDigest      string   `json:"agentc_digest"`
	SandboxDigest     string   `json:"sandbox_digest"`
	Ready             bool     `json:"ready"`
	AdmissionClosed   bool     `json:"admission_closed"`
	AdmissionOpen     bool     `json:"admission_open"`
	ActiveTurns       uint64   `json:"active_turns"`
	QueuedInputs      uint64   `json:"queued_inputs"`
	PendingApprovals  uint64   `json:"pending_approvals"`
	ToolWriters       uint64   `json:"tool_writers"`
	Drained           bool     `json:"drained"`
	ProcessStopped    bool     `json:"process_stopped"`
	WriterReleased    bool     `json:"writer_released"`
	HistoryRetained   bool     `json:"history_retained"`
	WorkspaceRetained bool     `json:"workspace_retained"`
}

type Key struct {
	Scope *v1.Scope
	ID    string
}

// Store implementations must return independent values, perform atomic CAS,
// enforce scope/id and scope/workspace uniqueness, and never delete user data.
type Store interface {
	Create(context.Context, *State) error
	Load(context.Context, *v1.Scope, string) (*State, error)
	Save(context.Context, *State, uint64) error
	List(context.Context, *v1.Scope, string, int) ([]*State, error)
	Pending(context.Context, *v1.Scope, int) ([]Key, error)
	PutAgentc(context.Context, *v1.AgentcRegistration) error
	GetAgentc(context.Context, *v1.Scope, string) (*v1.AgentcRegistration, error)
	PutRuntime(context.Context, *v1.RuntimeRevision) error
	GetRuntime(context.Context, *v1.Scope, string) (*v1.RuntimeRevision, error)
}

func ValidateScope(scope *v1.Scope) error {
	if scope.GetTenantId() == "" || scope.GetProjectId() == "" {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "tenant_id and project_id are required")
	}
	if err := ValidateID(scope.TenantId); err != nil {
		return err
	}
	return ValidateID(scope.ProjectId)
}

func ValidateID(id string) error {
	if len(id) == 0 || len(id) > 128 || strings.IndexFunc(id, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.')
	}) >= 0 {
		return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_INVALID, "id must contain 1..128 ASCII letters, digits, '.', '_' or '-'")
	}
	return nil
}

func Fingerprint(message proto.Message) string {
	b, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HasWork(state *State) bool {
	op := state.Operations[state.Deployment.OperationId]
	return op != nil && op.Status != v1.OperationStatus_OPERATION_STATUS_SUCCEEDED && op.Status != v1.OperationStatus_OPERATION_STATUS_FAILED
}
