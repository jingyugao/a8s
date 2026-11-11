package execution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"google.golang.org/protobuf/proto"
)

// TargetReader must read Control's authoritative ownership view. Execution never changes it.
type TargetReader interface {
	ReadExecutionTarget(context.Context, Resource) (Target, error)
}
type Target struct {
	Binding       *v1.Binding
	AdmissionOpen bool
	ViewVersion   string
}
type Resolver interface {
	Resolve(context.Context, *v1.Binding) (contract.Executor, error)
}

// Authorizer receives the caller identity through context and checks the full resource scope.
type Authorizer interface {
	Authorize(context.Context, Resource, string) error
}
type Resource struct{ Scope, DeploymentID, WorkspaceID, ThreadID string }
type SubmitRequest struct {
	Resource
	RequestKey, Text, AuthorID string
}
type CommandRequest struct {
	Resource
	RequestKey, TurnID, InteractionID, Decision string
}

// Record contains no dispatchable user input. Target remains fixed whenever
// execution may have been received; only a confirmed pre-execution rejection
// permits retargeting on a later same-key submission.
type Record struct {
	ID, Key, Kind, Fingerprint string
	Resource                   Resource
	Target                     *v1.Binding
	State                      string
	Result                     json.RawMessage
	Version                    int64
	CreatedAt, UpdatedAt       time.Time
}

// Store must atomically claim (scope, deployment, workspace, kind, key), and use
// version-conditional Save. Successful OpenThread Save also registers its ThreadID
// atomically. Records must survive gateway restarts and must not expire silently.
// Save may replace Target only when the stored prior state is Rejected.
type Store interface {
	ByKey(context.Context, Resource, string, string) (Record, error)
	ByID(context.Context, Resource, string) (Record, error)
	Claim(context.Context, Record) (Record, bool, error)
	Save(context.Context, Record, int64) (Record, error)
	HasThread(context.Context, Resource) (bool, error)
	Thread(context.Context, Resource) (Record, error)
	ListThreads(context.Context, Resource, string, int) ([]Record, error)
}

var ErrNotFound = errors.New("execution record not found")
var ErrConflict = errors.New("execution record version conflict")

type Gateway struct {
	store   Store
	targets TargetReader
	agents  Resolver
	auth    Authorizer
	timeout time.Duration
}

func New(store Store, targets TargetReader, agents Resolver, auth Authorizer, timeout time.Duration) (*Gateway, error) {
	if store == nil || targets == nil || agents == nil || auth == nil || timeout <= 0 {
		return nil, contract.NewError("Invalid", "store, routing, executor resolver, authorizer and timeout are required")
	}
	return &Gateway{store, targets, agents, auth, timeout}, nil
}

func (g *Gateway) authorize(ctx context.Context, r Resource, action string, thread bool) error {
	if r.Scope == "" || r.DeploymentID == "" || r.WorkspaceID == "" || (thread && r.ThreadID == "") {
		return contract.NewError("Invalid", "resource scope, deployment, workspace and thread are required")
	}
	if err := g.auth.Authorize(ctx, r, action); err != nil {
		return err
	}
	if thread {
		ok, err := g.store.HasThread(ctx, r)
		if err != nil {
			return err
		}
		if !ok {
			return contract.NewError("NotFound", "thread does not belong to the requested resource")
		}
	}
	return nil
}

func fingerprint(value any) string {
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func newID() string {
	var b [16]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (g *Gateway) claim(ctx context.Context, r Resource, kind, key, hash string, admission bool) (Record, bool, error) {
	if key == "" || len(key) > 256 {
		return Record{}, false, contract.NewError("Invalid", "request key must contain 1 to 256 bytes")
	}
	old, err := g.store.ByKey(ctx, r, kind, key)
	if err == nil {
		if old.Fingerprint != hash || old.State != "Rejected" {
			return checkClaim(old, false, hash)
		}
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Record{}, false, err
	}
	target, err := g.targets.ReadExecutionTarget(ctx, r)
	if err != nil {
		return Record{}, false, err
	}
	if admission && !target.AdmissionOpen {
		return Record{}, false, contract.NewError("AdmissionClosed", "new execution admission is closed; resubmit with the same key after it opens")
	}
	if err := contract.ValidateBinding(target.Binding); err != nil {
		return Record{}, false, err
	}
	b := target.Binding
	if b.Scope != r.Scope || b.DeploymentId != r.DeploymentID || b.WorkspaceId != r.WorkspaceID {
		return Record{}, false, contract.NewError("Conflict", "Control returned a different resource binding")
	}
	if old.ID != "" {
		old.Target = proto.Clone(b).(*v1.Binding)
		old.State, old.Result, old.UpdatedAt = "Dispatching", nil, time.Now().UTC()
		restarted, err := g.store.Save(ctx, old, old.Version)
		if errors.Is(err, ErrConflict) {
			latest, err := g.store.ByKey(ctx, r, kind, key)
			return latest, false, err
		}
		return restarted, err == nil, err
	}
	now := time.Now().UTC()
	record, created, err := g.store.Claim(ctx, Record{ID: newID(), Key: key, Kind: kind, Fingerprint: hash, Resource: r, Target: proto.Clone(b).(*v1.Binding), State: "Dispatching", Version: 1, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return Record{}, false, err
	}
	return checkClaim(record, created, hash)
}
func checkClaim(r Record, created bool, hash string) (Record, bool, error) {
	if r.Fingerprint != hash {
		return r, false, contract.NewError("Conflict", "request key already used for different content")
	}
	return r, created, nil
}

func unknownError(id, message string, cause error) *contract.Error {
	result := &contract.Error{Code: "OutcomeUnknown", Source: "execution", RequestID: id, Message: message, Cause: cause}
	var native *contract.Error
	if errors.As(cause, &native) {
		result.Source = native.Source
		result.NativeCode = native.NativeCode
	}
	return result
}

// finish uses an independent bounded context: a cancelled HTTP request must not
// prevent recording a known native receipt. A failed write retains the request ID.
func (g *Gateway) finish(ctx context.Context, r Record, result any, callErr error) (Record, error) {
	r.State = "Accepted"
	if callErr != nil {
		r.State = "OutcomeUnknown"
		switch contract.Code(callErr) {
		case "AdmissionClosed", "Invalid", "Forbidden", "NotFound", "Unsupported", "Conflict":
			r.State = "Rejected"
		}
	} else {
		var err error
		r.Result, err = json.Marshal(result)
		if err != nil {
			return r, err
		}
	}
	r.UpdatedAt = time.Now().UTC()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.timeout)
	defer cancel()
	saved, err := g.store.Save(writeCtx, r, r.Version)
	if err != nil {
		return r, &contract.Error{Code: "OutcomeUnknown", Source: "execution.storage", RequestID: r.ID, Message: "native result could not be recorded; query by the original request ID", Cause: err}
	}
	if callErr != nil {
		if r.State == "Rejected" {
			return saved, &contract.Error{Code: contract.Code(callErr), Source: "execution", RequestID: r.ID, Message: "executor rejected the request before execution; resubmit with the same key after correcting the condition", Cause: callErr}
		}
		return saved, unknownError(r.ID, "request may have reached the original executor; query by request ID", callErr)
	}
	return saved, nil
}

func (g *Gateway) OpenThread(ctx context.Context, r Resource, key string) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, r, "OpenThread", false); err != nil {
		return Record{}, err
	}
	if r.ThreadID != "" {
		return Record{}, contract.NewError("Invalid", "new thread must not specify an existing thread ID")
	}
	record, created, err := g.claim(ctx, r, "OpenThread", key, fingerprint(r), true)
	if err != nil || !created {
		return record, err
	}
	a, err := g.agents.Resolve(ctx, record.Target)
	if err != nil {
		return g.finish(ctx, record, nil, err)
	}
	result, err := a.OpenThread(ctx, &v1.OpenThreadRequest{Binding: record.Target, RequestId: record.ID})
	if err == nil && (result == nil || result.ThreadId == "") {
		err = fmt.Errorf("executor returned no thread ID")
	}
	if err == nil {
		record.Resource.ThreadID = result.ThreadId
	}
	return g.finish(ctx, record, result, err)
}

func (g *Gateway) SubmitInput(ctx context.Context, req SubmitRequest) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, req.Resource, "SubmitInput", true); err != nil {
		return Record{}, err
	}
	if req.Text == "" || len(req.Text) > 1024*1024 {
		return Record{}, contract.NewError("Invalid", "text must contain 1 to 1048576 bytes")
	}
	r, created, err := g.claim(ctx, req.Resource, "SubmitInput", req.RequestKey, fingerprint(req), true)
	if err != nil || !created {
		return r, err
	}
	a, err := g.agents.Resolve(ctx, r.Target)
	if err != nil {
		return g.finish(ctx, r, nil, err)
	}
	result, err := a.SubmitInput(ctx, &v1.SubmitInputRequest{Binding: r.Target, RequestId: r.ID, ThreadId: req.ThreadID, Text: req.Text, AuthorId: req.AuthorID})
	if err == nil && (result == nil || result.Result == nil || result.Result.RequestId != r.ID || result.Result.ThreadId != req.ThreadID) {
		err = fmt.Errorf("executor returned an invalid receipt")
	}
	// Full output is queried from the native store, not duplicated in platform storage.
	if err == nil {
		result = proto.Clone(result).(*v1.SubmitInputResponse)
		result.Result.OutputText = ""
	}
	return g.finish(ctx, r, result, err)
}

func (g *Gateway) GetInputResult(ctx context.Context, resource Resource, id string) (*v1.InputResult, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, resource, "GetInputResult", true); err != nil {
		return nil, err
	}
	r, err := g.store.ByID(ctx, resource, id)
	if err != nil {
		return nil, err
	}
	if r.Kind != "SubmitInput" || r.Resource.ThreadID != resource.ThreadID {
		return nil, contract.NewError("NotFound", "input does not belong to this thread")
	}
	previous := &v1.SubmitInputResponse{}
	if len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, previous); err != nil {
			return nil, err
		}
	}
	unknown := &v1.InputResult{RequestId: id, ThreadId: resource.ThreadID, State: "OutcomeUnknown"}
	a, err := g.agents.Resolve(ctx, r.Target)
	if err != nil {
		return unknown, unknownError(id, "original executor is unavailable", err)
	}
	q := &v1.GetInputResultRequest{Binding: r.Target, RequestId: id, ThreadId: resource.ThreadID}
	if previous.Result != nil {
		q.TurnId = previous.Result.TurnId
	}
	result, err := a.GetInputResult(ctx, q)
	if err != nil {
		return unknown, unknownError(id, "original receipt could not be reconciled", err)
	}
	if result == nil || result.Result == nil || result.Result.RequestId != id || result.Result.ThreadId != resource.ThreadID {
		return unknown, contract.NewError("OutcomeUnknown", "executor returned an invalid query result")
	}
	if result.Result.State != "OutcomeUnknown" && result.Result.TurnId != "" {
		receipt := proto.Clone(result.Result).(*v1.InputResult)
		receipt.OutputText = ""
		r.Result, err = json.Marshal(&v1.SubmitInputResponse{Result: receipt})
		if err != nil {
			return nil, err
		}
		r.State, r.UpdatedAt = receipt.State, time.Now().UTC()
		if _, err := g.store.Save(ctx, r, r.Version); err != nil && !errors.Is(err, ErrConflict) {
			return result.Result, err
		}
	}
	return result.Result, nil
}

// Commands are always routed to the original input's binding, even after an epoch switch.
func (g *Gateway) command(ctx context.Context, req CommandRequest, inputID, kind string) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, req.Resource, kind, true); err != nil {
		return Record{}, err
	}
	input, err := g.store.ByID(ctx, req.Resource, inputID)
	if err != nil {
		return Record{}, err
	}
	if input.Kind != "SubmitInput" || input.Resource.ThreadID != req.ThreadID {
		return Record{}, contract.NewError("NotFound", "input does not belong to this thread")
	}
	var submitted v1.SubmitInputResponse
	if err := json.Unmarshal(input.Result, &submitted); err != nil || submitted.Result == nil || req.TurnID == "" || submitted.Result.TurnId != req.TurnID {
		return Record{}, contract.NewError("Conflict", "turn must match the recorded native receipt; reconcile unknown input first")
	}
	hash := fingerprint(struct {
		CommandRequest
		InputID string
	}{req, inputID})
	if req.RequestKey == "" || len(req.RequestKey) > 256 {
		return Record{}, contract.NewError("Invalid", "request key must contain 1 to 256 bytes")
	}
	now := time.Now().UTC()
	r, created, err := g.store.Claim(ctx, Record{ID: newID(), Key: req.RequestKey, Kind: kind, Fingerprint: hash, Resource: req.Resource, Target: input.Target, State: "Dispatching", Version: 1, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return Record{}, err
	}
	r, created, err = checkClaim(r, created, hash)
	if err != nil || !created {
		return r, err
	}
	a, err := g.agents.Resolve(ctx, r.Target)
	if err != nil {
		return g.finish(ctx, r, nil, err)
	}
	if kind == "Cancel" {
		result, err := a.Cancel(ctx, &v1.CancelRequest{Binding: r.Target, RequestId: r.ID, ThreadId: req.ThreadID, TurnId: req.TurnID})
		return g.finish(ctx, r, result, err)
	}
	result, err := a.RespondInteraction(ctx, &v1.RespondInteractionRequest{Binding: r.Target, RequestId: r.ID, ThreadId: req.ThreadID, TurnId: req.TurnID, InteractionId: req.InteractionID, Decision: req.Decision})
	return g.finish(ctx, r, result, err)
}
func (g *Gateway) Cancel(ctx context.Context, req CommandRequest, inputID string) (Record, error) {
	return g.command(ctx, req, inputID, "Cancel")
}
func (g *Gateway) RespondInteraction(ctx context.Context, req CommandRequest, inputID string) (Record, error) {
	return g.command(ctx, req, inputID, "RespondInteraction")
}

func (g *Gateway) Subscribe(ctx context.Context, r Resource, inputID string, send func(*v1.SubscribeResponse) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := g.authorize(ctx, r, "Subscribe", true); err != nil {
		return err
	}
	record, err := g.store.ByID(ctx, r, inputID)
	if err != nil {
		return err
	}
	if record.Kind != "SubmitInput" || record.Resource.ThreadID != r.ThreadID {
		return contract.NewError("NotFound", "input does not belong to this thread")
	}
	a, err := g.agents.Resolve(ctx, record.Target)
	if err != nil {
		return err
	}
	return a.Subscribe(ctx, &v1.SubscribeRequest{Binding: record.Target, ThreadId: r.ThreadID}, send)
}

// Lookup preserves idempotent query access even when the current admission is closed.
func (g *Gateway) Lookup(ctx context.Context, r Resource, kind, key string) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, r, "GetInputResult", false); err != nil {
		return Record{}, err
	}
	return g.store.ByKey(ctx, r, kind, key)
}
