//go:build linux

package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	agent "github.com/jingyugao/a8s/agentc/contract"
	"github.com/jingyugao/a8s/infra/processes"
)

func (m *Manager) runtimeCall(ctx context.Context, route string, result any) error {
	_, value := m.Status()
	data, err := json.Marshal(agent.RuntimeRequest{Binding: value.Binding})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:8096/runtime/"+route, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("native activity or graceful stop is unconfirmed")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}

// ObserveActivity combines the harness inventory with every process belonging
// to the workspace UID. Detached and sleeping processes are still active work.
func (m *Manager) ObserveActivity(ctx context.Context) (agent.Activity, error) {
	m.op.Lock()
	defer m.op.Unlock()
	return m.observeActivity(ctx)
}

func (m *Manager) observeActivity(ctx context.Context) (result agent.Activity, observeErr error) {
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		copy := result
		m.activity, m.activityCheckedAt = &copy, time.Now()
		m.activityError = ""
		if observeErr != nil {
			m.activityError = "activity observation failed; workspace retained"
		}
	}()
	_, value := m.Status()
	result = agent.Activity{}
	if value.Phase == "Idle" || value.Phase == "Stopped" || value.Phase == "Suspended" {
		result.Known = true
		return result, nil
	}
	if value.Phase != "Running" {
		result.Reason = "workspace transition is unconfirmed"
		return result, nil
	}
	if err := m.runtimeCall(ctx, "observe", &result); err != nil {
		return result, err
	}
	if !result.Known {
		return result, nil
	}
	// These identities are written by the worker immediately after each spawn.
	var framework []agent.ProcessIdentity
	data, err := os.ReadFile(filepath.Join(value.RuntimeDirectory, "worker-processes.json"))
	if err != nil {
		return agent.Activity{}, err
	}
	if err := json.Unmarshal(data, &framework); err != nil {
		return agent.Activity{}, err
	}
	framework = append(framework, result.FrameworkProcesses...)
	extras, err := m.extraProcesses(value.Request.UID, framework)
	if err != nil {
		return agent.Activity{}, err
	}
	result.BackgroundTasks += uint32(len(extras))
	if len(extras) != 0 {
		result.Reason = "workspace has background OS processes"
	}
	return result, nil
}

func (m *Manager) extraProcesses(uid uint32, framework []agent.ProcessIdentity) ([]processes.Info, error) {
	allowed := map[agent.ProcessIdentity]bool{}
	for _, identity := range framework {
		allowed[identity] = true
	}
	all, err := processes.ForUID(uid)
	if err != nil {
		return nil, err
	}
	var extras []processes.Info
	for _, process := range all {
		if process.State == "Z" {
			// As PID 1, reap only adopted children; the worker has its own Wait goroutine.
			if process.ParentPID == os.Getpid() && (m.process == nil || process.PID != m.process.Process.Pid) {
				var status syscall.WaitStatus
				_, _ = syscall.Wait4(process.PID, &status, syscall.WNOHANG, nil)
			}
			continue
		}
		if !allowed[process.ProcessIdentity] {
			extras = append(extras, process)
		}
	}
	return extras, nil
}

func (m *Manager) Wake(ctx context.Context) error {
	m.op.Lock()
	defer m.op.Unlock()
	_, value := m.Status()
	if value.Phase == "Running" {
		return nil
	}
	if value.Phase != "Suspended" || !value.HistoryFlushed || !value.ProcessReleased {
		return agent.NewError("NotReady", "workspace cannot be safely resumed")
	}
	if !value.AdmissionOpen {
		return agent.NewError("AdmissionClosed", "instance admission is closed")
	}
	_, err := m.startRuntime(ctx, value, time.Now())
	return err
}

// Park never uses SIGKILL or signals a user process. Any ambiguous exit leaves
// the slot quarantined for inspection, with its workspace lease retained.
func (m *Manager) Park(ctx context.Context) error {
	m.op.Lock()
	defer m.op.Unlock()
	_, value := m.Status()
	if value.Phase == "Idle" || value.Phase == "Stopped" || value.Phase == "Suspended" {
		return nil
	}
	activity, err := m.observeActivity(ctx)
	if err != nil {
		return err
	}
	if !activity.Idle() {
		return errors.New("workspace activity prevents reclamation")
	}
	// Refuse new entries before the last native check. Read-only streams are
	// cancelled; mutation callers are fenced by the Control idle controller.
	value.Phase = "Parking"
	if err := m.save(value); err != nil {
		return err
	}
	m.mu.Lock()
	if m.cancelSession != nil {
		m.cancelSession()
	}
	m.mu.Unlock()
	restore := func(err error) error {
		value.Phase = "Running"
		m.mu.Lock()
		m.session, m.cancelSession = context.WithCancel(context.Background())
		m.mu.Unlock()
		_ = m.save(value)
		return err
	}
	for {
		m.mu.Lock()
		pending := m.rpcCount
		m.mu.Unlock()
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return restore(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Repeat process discovery while the public entry gate is frozen.
	var framework []agent.ProcessIdentity
	data, err := os.ReadFile(filepath.Join(value.RuntimeDirectory, "worker-processes.json"))
	if err != nil {
		return restore(err)
	}
	if err = json.Unmarshal(data, &framework); err != nil {
		return restore(err)
	}
	core := map[agent.ProcessIdentity]bool{}
	for _, identity := range framework {
		core[identity] = true
	}
	framework = append(framework, activity.FrameworkProcesses...)
	extras, err := m.extraProcesses(value.Request.UID, framework)
	if err != nil || len(extras) != 0 {
		return restore(errors.Join(err, errors.New("workspace process inventory changed")))
	}
	var proof agent.IdleStopResult
	err = m.runtimeCall(ctx, "park", &proof)
	if err == nil && (!proof.ProcessStopped || !proof.HistoryPersisted) {
		err = errors.New("native persistence proof is missing")
	}

	if err == nil {
		extras, err = m.extraProcesses(value.Request.UID, framework)
		if len(extras) != 0 {
			err = errors.New("new background process prevents framework shutdown")
		}
	}
	if err == nil {
		// Native helpers may outlive their parent after EOF. Signal only the
		// observed, pinned helpers; never sweep or signal a whole process group.
		for _, known := range activity.FrameworkProcesses {
			if core[known] {
				continue
			}
			info, check := processes.Read(known.PID)
			if errors.Is(check, os.ErrNotExist) {
				continue
			}
			if check != nil {
				err = check
				break
			}
			if info.ProcessIdentity != known {
				err = errors.New("native helper PID identity changed")
				break
			}
			if check = syscall.Kill(known.PID, syscall.SIGTERM); check != nil && check != syscall.ESRCH {
				err = check
				break
			}
		}
	}
	if err == nil {
		// The only remaining live processes are the observed worker and its two
		// framework servers. A graceful worker signal shuts those down and waits.
		identity, check := processes.Read(value.WorkerPID)
		if check != nil {
			err = check
		} else {
			found := false
			for _, known := range framework {
				if identity.ProcessIdentity == known {
					found = true
				}
			}
			if !found {
				err = errors.New("worker PID identity changed")
			} else {
				err = syscall.Kill(value.WorkerPID, syscall.SIGTERM)
			}
		}
	}
	if err == nil {
		select {
		case <-m.exited:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil {
		for {
			extras, err = m.extraProcesses(value.Request.UID, nil)
			all, scanErr := processes.ForUID(value.Request.UID)
			if err != nil || scanErr != nil {
				err = errors.Join(err, scanErr)
				break
			}
			if len(extras) == 0 && len(all) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		value.Phase = "Quarantined"
		_ = m.save(value)
		return fmt.Errorf("idle stop unconfirmed; workspace retained: %w", err)
	}
	value.Phase, value.HistoryFlushed, value.ProcessReleased, value.Forced = "Suspended", true, true, false
	return m.save(value)
}
