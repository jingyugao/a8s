//go:build linux

package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/artifacts"
	"google.golang.org/protobuf/proto"
)

const CodexVersion = "0.166.0"

type Packages interface {
	Materialize(context.Context, string, string) (artifacts.Package, error)
}
type LocalLayout struct{ Directory string }

func (l LocalLayout) Materialize(ctx context.Context, digest, destination string) (artifacts.Package, error) {
	return artifacts.MaterializeLayout(ctx, l.Directory, digest, destination)
}

type Config struct {
	HostPath, RuntimeDirectory string
	// StateDirectory optionally keeps native history and thread ownership across
	// instance generations. RuntimeDirectory still owns admission and receipts.
	// A single writer lock covers StateDirectory, including process restarts.
	StateDirectory string
	Packages       Packages
	// Environment resolves credentials at runtime. Values are never persisted or logged.
	// Native Codex history is kept in the dedicated state directory.
	Environment    func(context.Context, *v1.Binding) (map[string]string, error)
	Timeout        time.Duration
	IdleManagement bool
}
type interaction struct {
	ID                     json.RawMessage
	ThreadID, TurnID, Kind string
}
type reply struct {
	requestID string
	done      chan error
}
type subscription struct {
	thread string
	events chan *v1.SubscribeResponse
	failed chan struct{}
}
type Agent struct {
	mu              sync.Mutex
	cfg             Config
	ready           *v1.Ready
	prepare         *v1.PrepareRequest
	preparing       bool
	rpc             *rpc
	process         *exec.Cmd
	exited          chan struct{}
	lock            *os.File
	interactions    map[string]interaction
	replies         map[string]reply
	subscribers     map[*subscription]struct{}
	streamLost      bool
	inflight        uint32
	activeMutations uint32
	stdin           io.WriteCloser
	exitError       error
	idleClosing     bool
	idleDone        chan struct{}
	idleResult      *contract.IdleStopResult
	idleError       error
	lastActivity    time.Time
	journalMu       sync.Mutex
	pendingIntents  map[string]bool
	emptyThreads    map[string]bool
}

var _ contract.Agentc = (*Agent)(nil)

func New(cfg Config) (*Agent, error) {
	if cfg.HostPath == "" || cfg.RuntimeDirectory == "" || cfg.Packages == nil || cfg.Environment == nil || cfg.Timeout <= 0 {
		return nil, contract.NewError("Invalid", "host, dedicated runtime directory, packages, environment resolver and timeout are required")
	}
	var err error
	cfg.HostPath, err = filepath.Abs(cfg.HostPath)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeDirectory, err = filepath.Abs(cfg.RuntimeDirectory)
	if err != nil {
		return nil, err
	}
	if cfg.StateDirectory == "" {
		cfg.StateDirectory = cfg.RuntimeDirectory
	} else if cfg.StateDirectory, err = filepath.Abs(cfg.StateDirectory); err != nil {
		return nil, err
	}
	return &Agent{cfg: cfg, interactions: make(map[string]interaction), replies: make(map[string]reply), subscribers: make(map[*subscription]struct{}), pendingIntents: make(map[string]bool), emptyThreads: make(map[string]bool), lastActivity: time.Now()}, nil
}
func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
func validDigest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	for _, c := range s[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (a *Agent) Prepare(ctx context.Context, req *v1.PrepareRequest) (_ *v1.PrepareResponse, resultErr error) {
	if req == nil {
		return nil, contract.NewError("Invalid", "prepare request is required")
	}
	if err := contract.ValidateBinding(req.Binding); err != nil {
		return nil, err
	}
	if !validDigest(req.PackageDigest) || !validDigest(req.HostDigest) || !filepath.IsAbs(req.WorkspaceDirectory) {
		return nil, contract.NewError("Invalid", "fixed package/host digests and an absolute workspace directory are required")
	}
	u, err := url.Parse(req.ExecServerUrl)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "unix") || (u.Host == "" && u.Path == "") {
		return nil, contract.NewError("Invalid", "a credential-free remote exec-server URL is required")
	}
	a.mu.Lock()
	if a.preparing {
		a.mu.Unlock()
		return nil, contract.NewError("NotReady", "binding preparation is in progress")
	}
	if a.prepare != nil {
		if !proto.Equal(a.prepare, req) {
			a.mu.Unlock()
			return nil, contract.NewError("Conflict", "Agentc process is already bound to another immutable instance")
		}
		a.mu.Unlock()
		observed, err := a.Inspect(ctx, &v1.InspectRequest{Binding: req.Binding})
		if err != nil {
			return nil, err
		}
		return &v1.PrepareResponse{Ready: observed.Ready}, nil
	}
	a.preparing = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.preparing = false; a.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	if err := os.MkdirAll(a.cfg.RuntimeDirectory, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.cfg.StateDirectory, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(a.cfg.StateDirectory, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, contract.NewError("Conflict", "runtime directory is held by another Agentc process")
	}
	transferred := false
	defer func() {
		if resultErr != nil && !transferred {
			lock.Close()
		}
	}()
	identity, _ := json.Marshal(req)
	bindingFile := filepath.Join(a.cfg.RuntimeDirectory, "binding.json")
	if b, err := os.ReadFile(bindingFile); err == nil {
		var previous v1.PrepareRequest
		if json.Unmarshal(b, &previous) != nil || !proto.Equal(&previous, req) {
			return nil, contract.NewError("Conflict", "runtime directory belongs to another immutable binding")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err := atomicFile(bindingFile, identity); err != nil {
		return nil, err
	}
	hostDigest, err := digestFile(a.cfg.HostPath)
	if err != nil {
		return nil, err
	}
	if hostDigest != req.HostDigest {
		return nil, contract.NewError("Incompatible", "actual host digest does not match the fixed release")
	}
	packageRoot := filepath.Join(a.cfg.StateDirectory, "package")
	// Re-materialize from the verified OCI manifest on process restart. A mutable
	// extracted directory cannot independently prove its original manifest digest.
	if err := removeMaterializedPackage(packageRoot); err != nil {
		return nil, err
	}
	p, err := a.cfg.Packages.Materialize(ctx, req.PackageDigest, packageRoot)
	if err != nil {
		return nil, err
	}
	if p.Digest != req.PackageDigest {
		return nil, contract.NewError("Incompatible", "materialized package digest differs from the fixed release")
	}
	if len(p.Metadata.Manifest.CLIs) > 0 {
		return nil, contract.NewError("Unsupported", "remote package CLI installation has not been verified")
	}
	derived := filepath.Join(a.cfg.StateDirectory, "codex")
	for _, d := range []string{derived, filepath.Join(derived, "skills"), filepath.Join(a.cfg.RuntimeDirectory, "receipts")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	if err := a.restorePendingIntents(); err != nil {
		return nil, err
	}
	// Copy content into the derived home; package bytes are never a writable config tree.
	instructions, err := os.ReadFile(filepath.Join(packageRoot, p.Metadata.Manifest.Instructions))
	if err != nil {
		return nil, err
	}
	if err := atomicFile(filepath.Join(derived, "AGENTS.md"), instructions); err != nil {
		return nil, err
	}
	for _, skill := range p.Metadata.Manifest.Skills {
		link := filepath.Join(derived, "skills", skill.Name)
		target := filepath.Join(packageRoot, skill.Path)
		if old, err := os.Readlink(link); err == nil {
			if old != target {
				return nil, contract.NewError("Conflict", "skill projection differs from immutable package")
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if err := os.Symlink(target, link); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	environment, err := a.cfg.Environment(ctx, req.Binding)
	if err != nil {
		return nil, err
	}
	// All shell policy values and credentials are caller-owned; no secret config file is generated.
	config := "allow_login_shell = false\n"
	if err := atomicFile(filepath.Join(derived, "config.toml"), []byte(config)); err != nil {
		return nil, err
	}
	cmd := exec.Command(a.cfg.HostPath, "app-server", "--listen", "stdio://")
	cmd.Dir = a.cfg.RuntimeDirectory
	cmd.Env = make([]string, 0, len(environment)+2)
	for k, v := range environment {
		if k == "CODEX_HOME" || k == "CODEX_EXEC_SERVER_URL" {
			return nil, contract.NewError("Invalid", "runtime-owned environment variable supplied by resolver")
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+derived, "CODEX_EXEC_SERVER_URL="+req.ExecServerUrl)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	// Raw stderr can contain tool arguments, URLs or DSNs. Diagnostics stay outside public logs.
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	transferred = true
	exited := make(chan struct{})
	native := newRPC(stdout, stdin, a.onFrame, a.onLost)
	a.mu.Lock()
	a.rpc = native
	a.process = cmd
	a.exited = exited
	a.stdin = stdin
	a.lock = lock
	a.streamLost = false
	a.mu.Unlock()
	go func() {
		exitErr := cmd.Wait()
		a.mu.Lock()
		a.exitError = exitErr
		idleClosing := a.idleClosing
		a.mu.Unlock()
		if !idleClosing {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		stdin.Close()
		native.fail(io.EOF)
		lock.Close()
		close(exited)
	}()
	defer func() {
		if resultErr != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-exited:
			case <-ctx.Done():
			}
		}
	}()
	if err := native.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "a8s_agentc", "title": "a8s Codex Agentc", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
		return nil, err
	}
	if err := native.notify(ctx, "initialized", map[string]any{}); err != nil {
		return nil, err
	}
	// A listening app-server is insufficient: require an initialized remote
	// executor. Provider supplies the validated workspace path; native fs/* only
	// addresses a local executor and must not introduce a local fallback here.
	var environmentInfo struct{ Shell struct{ Name, Path string } }
	if err := native.call(ctx, "environment/info", map[string]any{"environmentId": "remote"}, &environmentInfo); err != nil {
		return nil, err
	}
	if environmentInfo.Shell.Name == "" {
		return nil, contract.NewError("NotReady", "remote execution environment has not reported a shell")
	}
	if len(p.Metadata.Manifest.Skills) > 0 {
		var listing struct {
			Data []struct{ Skills []struct{ Name string } }
		}
		if err := native.call(ctx, "skills/list", map[string]any{"cwds": []string{req.WorkspaceDirectory}, "forceReload": true}, &listing); err != nil {
			return nil, err
		}
		found := make(map[string]bool)
		for _, entry := range listing.Data {
			for _, skill := range entry.Skills {
				found[skill.Name] = true
			}
		}
		for _, skill := range p.Metadata.Manifest.Skills {
			if !found[skill.Name] {
				return nil, contract.NewError("Incompatible", "native skill catalog does not include a declared package skill")
			}
		}
	}
	ready := &v1.Ready{Binding: proto.Clone(req.Binding).(*v1.Binding), Ready: true, PackageDigest: p.Digest, HostDigest: hostDigest, Capabilities: map[string]string{
		"native_request_dedup": "unsupported", "positive_receipt_lookup": "supported", "instance_dispatch_journal": "supported", "live_events": "supported", "event_replay": "unsupported", "complete_tool_drain": "unverified", "native_flush_proof": "unverified", "thread_restore": "unverified", "box_checkpoint": "unsupported", "shared_host": "unsupported", "remote_package_cli": "unsupported", "reopen_closed_binding": "unsupported", "package_skills": "supported",
	}}
	a.mu.Lock()
	a.prepare = proto.Clone(req).(*v1.PrepareRequest)
	a.ready = ready
	select {
	case <-native.done:
		a.streamLost = true
		a.mu.Unlock()
		return nil, contract.NewError("NotReady", "native host exited during preparation")
	default:
	}
	copyReady := proto.Clone(ready).(*v1.Ready)
	a.mu.Unlock()
	return &v1.PrepareResponse{Ready: copyReady}, nil
}

// Materialize makes directories read-only. Only this instance-owned extracted
// copy is made removable; the source OCI layout and shared artifacts are untouched.
func removeMaterializedPackage(root string) error {
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// check and inflight admission are serialized with SetAdmission, closing the old-view race.
func (a *Agent) enter(binding *v1.Binding, admission bool) (*rpc, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready == nil || !a.ready.Ready || a.rpc == nil || a.idleClosing {
		return nil, nil, contract.NewError("NotReady", "Agentc is not ready")
	}
	if !proto.Equal(a.ready.Binding, binding) {
		return nil, nil, contract.NewError("Conflict", "stale instance, workspace or epoch")
	}
	if admission && !a.ready.AdmissionOpen {
		return nil, nil, contract.NewError("AdmissionClosed", "instance admission is closed")
	}
	if a.streamLost {
		return nil, nil, contract.NewError("OutcomeUnknown", "native connection was lost")
	}
	a.inflight++
	return a.rpc, func() { a.mu.Lock(); a.inflight--; a.mu.Unlock() }, nil
}

// Reserve the intent under the admission lock. Checking the journal first is
// essential: a mesh retry arriving after close must not turn an earlier accepted
// request into an apparent pre-execution rejection.
func (a *Agent) begin(binding *v1.Binding, admission bool, kind, id string, request, response any) (*rpc, func(), bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ready == nil {
		return nil, nil, false, contract.NewError("NotReady", "Agentc is not prepared")
	}
	if !proto.Equal(a.ready.Binding, binding) {
		return nil, nil, false, contract.NewError("Conflict", "stale instance, workspace or epoch")
	}
	if found, err := a.replay(kind, id, request, response); found {
		return nil, func() {}, false, err
	}
	if !a.ready.Ready || a.streamLost || a.idleClosing {
		return nil, nil, false, contract.NewError("OutcomeUnknown", "native connection is unavailable")
	}
	if admission && !a.ready.AdmissionOpen {
		return nil, nil, false, contract.NewError("AdmissionClosed", "instance admission is closed")
	}
	created, err := a.claim(kind, id, request, response)
	if err != nil {
		return nil, nil, false, err
	}
	if !created {
		return nil, func() {}, false, nil
	}
	a.inflight++
	a.activeMutations++
	if admission {
		a.lastActivity = time.Now()
	}
	return a.rpc, func() { a.mu.Lock(); a.inflight--; a.activeMutations--; a.mu.Unlock() }, true, nil

}
func (a *Agent) Inspect(ctx context.Context, req *v1.InspectRequest) (*v1.InspectResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.ready == nil {
		a.mu.Unlock()
		return nil, contract.NewError("NotReady", "Agentc is not prepared")
	}
	if req == nil || !proto.Equal(a.ready.Binding, req.Binding) {
		a.mu.Unlock()
		return nil, contract.NewError("Conflict", "binding does not match")
	}
	ready := proto.Clone(a.ready).(*v1.Ready)
	ready.Ready = ready.Ready && !a.streamLost
	native := a.rpc
	a.mu.Unlock()
	if ready.Ready {
		ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
		defer cancel()
		var status struct{ Status string }
		if err := native.call(ctx, "environment/status", map[string]any{"environmentId": "remote"}, &status); err != nil {
			return nil, err
		}
		ready.Ready = status.Status == "ready"
	}
	return &v1.InspectResponse{Ready: ready}, nil
}
func (a *Agent) SetAdmission(ctx context.Context, req *v1.SetAdmissionRequest) (*v1.SetAdmissionResponse, error) {
	if req == nil {
		return nil, contract.NewError("Invalid", "admission request is required")
	}
	_, done, err := a.enter(req.Binding, false)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	// v1 closure is irreversible for this immutable Instance, so a delayed
	// initial Open cannot reopen a binding being drained, including after restart.
	closedPath := filepath.Join(a.cfg.RuntimeDirectory, "admission.closed")
	if req.Open {
		if _, err := os.Stat(closedPath); err == nil {
			a.mu.Unlock()
			return nil, contract.NewError("Conflict", "closed instance admission cannot be reopened")
		} else if !errors.Is(err, os.ErrNotExist) {
			a.mu.Unlock()
			return nil, err
		}
	} else if err := atomicFile(closedPath, []byte("closed\n")); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.ready.AdmissionOpen = req.Open
	ready := proto.Clone(a.ready).(*v1.Ready)
	a.mu.Unlock()
	return &v1.SetAdmissionResponse{Ready: ready}, nil
}

// Shutdown stops this process group only. It is NOT a storage release or a safe
// drained handoff. Control must use Drain/Stop and separate Provider evidence.
func (a *Agent) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	cmd, exited := a.process, a.exited
	if a.ready != nil {
		a.ready.AdmissionOpen = false
		a.ready.Ready = false
	}
	a.mu.Unlock()
	if cmd == nil {
		return nil
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// A kill request is not exit evidence. Keep the directory lock until the
		// waiter observes exit, and return within the caller's deadline.
		return ctx.Err()
	}
	a.mu.Lock()
	if a.lock != nil {
		a.lock.Close()
		a.lock = nil
	}
	a.mu.Unlock()
	return ctx.Err()
}
