package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/state"
)

type nativeItem struct{ Type, ID, ClientID, Text string }
type nativeTurn struct {
	ID, Status string
	Items      []nativeItem
}
type nativeThread struct {
	ID     string
	Status struct {
		Type        string
		ActiveFlags []string
	}
	Turns []nativeTurn
}

func (a *Agent) threadPath(id string) string {
	return filepath.Join(a.cfg.StateDirectory, "threads", fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
}
func (a *Agent) ownThread(id string) error {
	if id == "" {
		return contract.NewError("Invalid", "thread ID is required")
	}
	if _, err := os.Stat(a.threadPath(id)); err != nil {
		return contract.NewError("Unsupported", "thread is not owned by this instance; cross-instance history restore is not verified")
	}
	return nil
}
func (a *Agent) threadParams() (map[string]any, error) {
	a.mu.Lock()
	directory := a.prepare.WorkspaceDirectory
	a.mu.Unlock()
	instructions, err := os.ReadFile(filepath.Join(a.cfg.StateDirectory, "codex", "AGENTS.md"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"cwd": directory, "developerInstructions": string(instructions)}, nil
}
func (a *Agent) OpenThread(ctx context.Context, req *v1.OpenThreadRequest) (*v1.OpenThreadResponse, error) {
	if req == nil {
		return nil, contract.NewError("Invalid", "thread request is required")
	}
	result := &v1.OpenThreadResponse{}
	native, done, created, err := a.begin(req.Binding, true, "thread", req.RequestId, req, result)
	if err != nil || !created {
		return result, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	params, err := a.threadParams()
	if err != nil {
		return nil, err
	}
	// Codex's local store supports legacy rollout history without a custom store.
	params["historyMode"] = "legacy"
	var response struct{ Thread nativeThread }
	if err := native.call(ctx, "thread/start", params, &response); err != nil {
		return nil, err
	}
	if response.Thread.ID == "" {
		return nil, contract.NewError("OutcomeUnknown", "native thread response omitted identity")
	}
	if err := os.MkdirAll(filepath.Join(a.cfg.StateDirectory, "threads"), 0700); err != nil {
		return nil, err
	}
	if err := atomicFile(a.threadPath(response.Thread.ID), []byte(req.RequestId)); err != nil {
		return nil, err
	}
	result.ThreadId = response.Thread.ID
	if err := a.complete("thread", req.RequestId, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (a *Agent) SubmitInput(ctx context.Context, req *v1.SubmitInputRequest) (*v1.SubmitInputResponse, error) {
	if req == nil || req.Text == "" || len(req.Text) > 1024*1024 {
		return nil, contract.NewError("Invalid", "bounded nonempty input is required")
	}
	if err := a.ownThread(req.ThreadId); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	result := &v1.SubmitInputResponse{}
	native, done, created, err := a.begin(req.Binding, true, "input", req.RequestId, req, result)
	if err != nil || !created {
		return result, err
	}
	defer done()
	if err := a.ensureLoaded(ctx, native, req.ThreadId); err != nil {
		return nil, err
	}
	var response struct{ Turn nativeTurn }
	err = native.call(ctx, "turn/start", map[string]any{"threadId": req.ThreadId, "clientUserMessageId": req.RequestId, "input": []any{map[string]any{"type": "text", "text": req.Text, "text_elements": []any{}}}}, &response)
	if err != nil {
		return nil, err
	}
	if response.Turn.ID == "" {
		return nil, contract.NewError("OutcomeUnknown", "native turn response omitted identity")
	}
	result.Result = turnResult(req.RequestId, req.ThreadId, response.Turn)
	if err := a.complete("input", req.RequestId, result); err != nil {
		return nil, err
	}
	return result, nil
}

// A newly created thread may not have a rollout until its first input. Resuming
// that already loaded thread would fail before turn/start can materialize it.
func (a *Agent) ensureLoaded(ctx context.Context, native *rpc, threadID string) error {
	var cursor string
	for page := 0; page < 64; page++ {
		var loaded struct {
			Data       []string
			NextCursor string
		}
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := native.call(ctx, "thread/loaded/list", params, &loaded); err != nil {
			return err
		}
		for _, id := range loaded.Data {
			if id == threadID {
				return nil
			}
		}
		if loaded.NextCursor == "" {
			params, err := a.threadParams()
			if err != nil {
				return err
			}
			params["threadId"] = threadID
			params["excludeTurns"] = true
			return native.call(ctx, "thread/resume", params, nil)
		}
		if loaded.NextCursor == cursor {
			break
		}
		cursor = loaded.NextCursor
	}
	return contract.NewError("OutcomeUnknown", "native loaded-thread inventory exceeded the bounded observation window")
}

func turnResult(requestID, threadID string, turn nativeTurn) *v1.InputResult {
	state := "Accepted"
	switch turn.Status {
	case "inProgress":
		state = "Running"
	case "completed":
		state = "Succeeded"
	case "failed":
		state = "Failed"
	case "interrupted":
		state = "Cancelled"
	}
	var text []string
	for _, item := range turn.Items {
		if item.Type == "agentMessage" {
			text = append(text, item.Text)
		}
	}
	return &v1.InputResult{RequestId: requestID, ThreadId: threadID, TurnId: turn.ID, NativeReceipt: turn.ID, State: state, OutputText: strings.Join(text, "\n")}
}

// A matching native clientId is positive evidence. Its absence is never proof
// of non-execution, and this method never resends a user input.
func (a *Agent) GetInputResult(ctx context.Context, req *v1.GetInputResultRequest) (*v1.GetInputResultResponse, error) {
	if req == nil || req.RequestId == "" {
		return nil, contract.NewError("Invalid", "request ID is required")
	}
	native, done, err := a.enter(req.Binding, false)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := a.ownThread(req.ThreadId); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	var result struct{ Thread nativeThread }
	if err := native.call(ctx, "thread/read", map[string]any{"threadId": req.ThreadId, "includeTurns": true}, &result); err != nil {
		return nil, err
	}
	for _, turn := range result.Thread.Turns {
		for _, item := range turn.Items {
			if item.Type == "userMessage" && item.ClientID == req.RequestId {
				if req.TurnId != "" && req.TurnId != turn.ID {
					return nil, contract.NewError("Conflict", "native request is associated with another turn")
				}
				input := turnResult(req.RequestId, req.ThreadId, turn)
				// Native history remains authoritative; this derived cache may fail without
				// turning a confirmed native outcome into an execution failure.
				_ = state.SaveResult(a.cfg.RuntimeDirectory, req.Binding, input)
				return &v1.GetInputResultResponse{Result: input}, nil
			}
		}
	}
	return &v1.GetInputResultResponse{Result: &v1.InputResult{RequestId: req.RequestId, ThreadId: req.ThreadId, State: "OutcomeUnknown"}}, nil
}

func (a *Agent) Cancel(ctx context.Context, req *v1.CancelRequest) (*v1.CancelResponse, error) {
	if req == nil || req.TurnId == "" {
		return nil, contract.NewError("Invalid", "turn ID is required")
	}
	native, done, err := a.enter(req.Binding, false)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := a.ownThread(req.ThreadId); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	result := &v1.CancelResponse{}
	created, err := a.claim("cancel", req.RequestId, req, result)
	if err != nil || !created {
		return result, err
	}
	if err := native.call(ctx, "turn/interrupt", map[string]any{"threadId": req.ThreadId, "turnId": req.TurnId}, nil); err != nil {
		return nil, err
	}
	result.Accepted = true // Native acknowledgement does not prove tools have stopped.
	if err := a.complete("cancel", req.RequestId, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (a *Agent) RespondInteraction(ctx context.Context, req *v1.RespondInteractionRequest) (*v1.RespondInteractionResponse, error) {
	if req == nil || (req.Decision != "accept" && req.Decision != "decline") {
		return nil, contract.NewError("Invalid", "typed accept/decline approval decision is required")
	}
	native, done, err := a.enter(req.Binding, false)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := a.ownThread(req.ThreadId); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	result := &v1.RespondInteractionResponse{}
	created, err := a.claim("interaction", req.RequestId, req, result)
	if err != nil || !created {
		return result, err
	}
	a.mu.Lock()
	pending, ok := a.interactions[req.InteractionId]
	_, responding := a.replies[req.InteractionId]
	ok = ok && !responding && pending.ThreadID == req.ThreadId && pending.TurnID == req.TurnId
	a.mu.Unlock()
	if !ok {
		return nil, contract.NewError("Conflict", "interaction is expired or belongs to another turn")
	}
	if pending.Kind != "item/commandExecution/requestApproval" && pending.Kind != "item/fileChange/requestApproval" {
		return nil, contract.NewError("Unsupported", "native interaction type is not supported")
	}
	ack := make(chan error, 1)
	a.mu.Lock()
	if _, exists := a.replies[req.InteractionId]; exists {
		a.mu.Unlock()
		return nil, contract.NewError("Conflict", "interaction response already in progress")
	}
	if _, exists := a.interactions[req.InteractionId]; !exists {
		a.mu.Unlock()
		return nil, contract.NewError("Conflict", "interaction already resolved")
	}
	a.replies[req.InteractionId] = reply{requestID: req.RequestId, done: ack}
	a.mu.Unlock()
	response, _ := json.Marshal(map[string]string{"decision": req.Decision})
	if err := native.send(ctx, frame{ID: pending.ID, Result: response}); err != nil {
		return nil, err
	}
	select {
	case err := <-ack:
		if err != nil {
			return nil, err
		}
		return &v1.RespondInteractionResponse{Accepted: true}, nil
	case <-ctx.Done():
		return nil, contract.NewError("OutcomeUnknown", "approval response sent but native resolution is unconfirmed")
	case <-native.done:
		return nil, contract.NewError("OutcomeUnknown", "native connection lost before interaction resolution")
	}
}

func (a *Agent) Drain(ctx context.Context, req *v1.DrainRequest) (*v1.DrainResponse, error) {
	if req == nil {
		return nil, contract.NewError("Invalid", "drain request is required")
	}
	native, done, err := a.enter(req.Binding, false)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	a.mu.Lock()
	report := &v1.DrainResponse{AdmissionClosed: !a.ready.AdmissionOpen, ActiveTurns: a.inflight - 1, WaitingInteractions: uint32(len(a.interactions)), Reason: "native protocol does not prove exhaustive tool shutdown or durable flush"}
	a.mu.Unlock()
	var cursor string
	for page := 0; page < 64; page++ {
		var loaded struct {
			Data       []string
			NextCursor string
		}
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := native.call(ctx, "thread/loaded/list", params, &loaded); err != nil {
			return nil, err
		}
		for _, id := range loaded.Data {
			var read struct{ Thread nativeThread }
			if err := native.call(ctx, "thread/read", map[string]any{"threadId": id}, &read); err != nil {
				return nil, err
			}
			if read.Thread.Status.Type != "idle" {
				report.ActiveTurns++
			}
			var queue struct {
				Data       []json.RawMessage
				NextCursor string
			}
			if err := native.call(ctx, "thread/queue/list", map[string]any{"threadId": id, "limit": 100}, &queue); err != nil {
				return nil, err
			}
			report.QueuedInputs += uint32(len(queue.Data))
			if queue.NextCursor != "" {
				report.QueuedInputs++
			} // Nonempty is sufficient to block, no false empty.
		}
		if loaded.NextCursor == "" {
			return report, nil
		}
		if loaded.NextCursor == cursor {
			break
		}
		cursor = loaded.NextCursor
	}
	report.Reason = "native thread inventory exceeded the bounded observation window"
	return report, nil
}
func (a *Agent) Stop(ctx context.Context, req *v1.StopRequest) (*v1.StopResponse, error) {
	if req == nil {
		return nil, contract.NewError("Invalid", "stop request is required")
	}
	report, err := a.Drain(ctx, &v1.DrainRequest{Binding: req.Binding})
	if err != nil {
		return nil, err
	}
	if !report.Drained {
		return nil, contract.NewError("Unsupported", "safe handoff stop requires verified tool-drain and native-flush evidence")
	}
	if err := a.Shutdown(ctx); err != nil {
		return nil, err
	}
	return &v1.StopResponse{ProcessStopped: true, NativeStatePersisted: report.NativeStatePersisted, Scope: "instance process group only; remote workspace release requires Provider evidence"}, nil
}
