//go:build linux

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/infra/processes"
	"google.golang.org/protobuf/proto"
)

func (a *Agent) restorePendingIntents() error {
	entries, err := os.ReadDir(filepath.Join(a.cfg.RuntimeDirectory, "receipts"))
	if err != nil {
		return err
	}
	materialized := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.cfg.RuntimeDirectory, "receipts", entry.Name()))
		if err != nil {
			return err
		}
		var receipt journalEntry
		if json.Unmarshal(data, &receipt) != nil || len(receipt.Result) == 0 || string(receipt.Result) == "null" {
			a.pendingIntents[entry.Name()] = true
			continue
		}
		var opened v1.OpenThreadResponse
		if json.Unmarshal(receipt.Result, &opened) == nil && opened.ThreadId != "" {
			a.emptyThreads[opened.ThreadId] = true
		}
		var input v1.SubmitInputResponse
		if json.Unmarshal(receipt.Result, &input) == nil && input.Result.GetTurnId() != "" {
			materialized[input.Result.ThreadId] = true
		}
	}
	for id := range materialized {
		delete(a.emptyThreads, id)
	}
	return nil
}

// ObserveActivity is read-only. Unknown native state or an incomplete dispatch
// journal remains ineligible for reclamation, even when no turn appears active.
func (a *Agent) ObserveActivity(ctx context.Context, binding *v1.Binding) (contract.Activity, error) {
	result := contract.Activity{}
	native, done, err := a.enter(binding, false)
	if err != nil {
		return result, err
	}
	defer done()
	a.mu.Lock()
	result.ActiveRequests, result.WaitingInteractions, result.LastActivity = a.activeMutations, uint32(len(a.interactions)), a.lastActivity
	pid := a.process.Process.Pid
	a.mu.Unlock()
	a.journalMu.Lock()
	pending, empty := len(a.pendingIntents), len(a.emptyThreads)
	a.journalMu.Unlock()
	if pending != 0 {
		result.Reason = "native dispatch outcome is unresolved"
		return result, nil
	}
	if empty != 0 {
		result.Reason = "new thread awaits its first persisted input"
		return result, nil
	}
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
			return result, err
		}
		for _, id := range loaded.Data {
			var read struct{ Thread nativeThread }
			if err := native.call(ctx, "thread/read", map[string]any{"threadId": id}, &read); err != nil {
				return result, err
			}
			if read.Thread.Status.Type != "idle" {
				result.ActiveTurns++
			}
			var queue struct {
				Data       []json.RawMessage
				NextCursor string
			}
			if err := native.call(ctx, "thread/queue/list", map[string]any{"threadId": id, "limit": 1}, &queue); err != nil {
				return result, err
			}
			result.QueuedInputs += uint32(len(queue.Data))
			if queue.NextCursor != "" {
				result.QueuedInputs++
			}
			var terminals struct {
				Data       []json.RawMessage
				NextCursor string
			}
			if err := native.call(ctx, "thread/backgroundTerminals/list", map[string]any{"threadId": id, "limit": 1}, &terminals); err != nil {
				return result, err
			}
			result.BackgroundTasks += uint32(len(terminals.Data))
			if terminals.NextCursor != "" {
				result.BackgroundTasks++
			}
			var goal struct{ Goal *struct{ Status string } }
			if err := native.call(ctx, "thread/goal/get", map[string]any{"threadId": id}, &goal); err != nil {
				return result, err
			}
			if goal.Goal != nil && goal.Goal.Status != "complete" {
				result.BackgroundTasks++
			}
		}
		if loaded.NextCursor == "" {
			result.Known = true
			break
		}
		if loaded.NextCursor == cursor {
			break
		}
		cursor = loaded.NextCursor
	}
	if !result.Known {
		result.Reason = "native thread inventory exceeded observation limit"
		return result, nil
	}
	for _, corePID := range []int{os.Getpid(), pid} {
		info, err := processes.Read(corePID)
		if err != nil {
			return result, err
		}
		result.FrameworkProcesses = append(result.FrameworkProcesses, info.ProcessIdentity)
	}
	// Only the pinned helper executable, directly owned by this native host, is
	// framework infrastructure. All other processes remain user work.
	helper, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(a.cfg.HostPath), "codex-code-mode-host"))
	if err != nil {
		return result, err
	}
	all, err := processes.ForUID(uint32(os.Geteuid()))
	if err != nil {
		return result, err
	}
	for _, info := range all {
		if info.ParentPID != pid || info.State == "Z" {
			continue
		}
		executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(info.PID), "exe"))
		if err == nil && executable == helper {
			result.FrameworkProcesses = append(result.FrameworkProcesses, info.ProcessIdentity)
		}
	}
	return result, nil
}

// IdleStop uses EOF rather than signals and requires a normal native exit plus
// explicit fsync. Guest separately verifies every OS process in this Workspace.
func (a *Agent) IdleStop(ctx context.Context, binding *v1.Binding) (*contract.IdleStopResult, error) {
	if !a.cfg.IdleManagement {
		return nil, errors.New("idle management is not enabled")
	}
	a.mu.Lock()
	if a.ready == nil || !proto.Equal(a.ready.Binding, binding) {
		a.mu.Unlock()
		return nil, contract.NewError("Conflict", "idle binding does not match")
	}
	pending := a.idleDone
	a.mu.Unlock()
	if pending == nil {
		activity, err := a.ObserveActivity(ctx, binding)
		if err != nil {
			return nil, err
		}
		if !activity.Idle() {
			return nil, errors.New("native activity prevents idle stop")
		}
		a.mu.Lock()
		if a.inflight != 0 || a.lastActivity.After(activity.LastActivity) || a.idleClosing {
			a.mu.Unlock()
			return nil, errors.New("native activity changed before idle stop")
		}
		if err := atomicFile(filepath.Join(a.cfg.RuntimeDirectory, "idle-stop.json"), []byte(`{"pending":true}`)); err != nil {
			a.mu.Unlock()
			return nil, err
		}
		a.idleClosing = true
		a.idleDone = make(chan struct{})
		pending = a.idleDone
		stdin, exited := a.stdin, a.exited
		a.mu.Unlock()
		go func() {
			err := stdin.Close()
			select {
			case <-exited:
			case <-time.After(15 * time.Second):
				err = errors.Join(err, errors.New("native graceful exit is unconfirmed"))
			}
			a.mu.Lock()
			exitErr := a.exitError
			a.mu.Unlock()
			select {
			case <-exited:
				err = errors.Join(err, exitErr)
			default:
				err = errors.Join(err, errors.New("native process has not exited"))
			}
			if err == nil {
				err = a.syncNativeState()
			}
			result := &contract.IdleStopResult{ProcessStopped: err == nil, HistoryPersisted: err == nil}
			if err == nil {
				data, _ := json.Marshal(result)
				err = atomicFile(filepath.Join(a.cfg.RuntimeDirectory, "idle-stop.json"), data)
			}
			a.mu.Lock()
			a.idleResult, a.idleError = result, err
			a.mu.Unlock()
			close(pending)
		}()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-pending:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.idleResult, a.idleError
}

func (a *Agent) syncNativeState() error {
	return filepath.WalkDir(filepath.Join(a.cfg.StateDirectory, "codex"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		return file.Sync()
	})
}

func (a *Agent) RuntimeHandler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range []string{"observe", "park"} {
		mux.HandleFunc("POST /runtime/"+route, func(w http.ResponseWriter, r *http.Request) {
			var request contract.RuntimeRequest
			r.Body = http.MaxBytesReader(w, r.Body, 16384)
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				http.Error(w, "invalid runtime request", 400)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			var value any
			var err error
			if route == "observe" {
				value, err = a.ObserveActivity(ctx, request.Binding)
			} else {
				value, err = a.IdleStop(ctx, request.Binding)
			}
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "native activity or graceful stop is unconfirmed"})
				return
			}
			_ = json.NewEncoder(w).Encode(value)
		})
	}
	return mux
}
