//go:build linux

// Package guest manages one non-root workspace process tree inside a dedicated
// Pod. The application supplies the image, credentials and HTTP authorization.
package guest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	agent "github.com/jingyugao/a8s/agentc/contract"
	av1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/contract/transport"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	Root, AgentRoot, Layout, Image, AgentName string
	SlotID                                    string
	Loader, Executable, Codex, Agentc         string
	Environment                               []string
	PlainHTTP                                 bool
	ManagedOnly                               bool
}

type StartRequest struct {
	WorkspaceID string       `json:"workspaceId"`
	UID         uint32       `json:"uid"`
	Generation  uint64       `json:"generation"`
	RequestKey  string       `json:"requestKey"`
	Binding     *av1.Binding `json:"binding,omitempty"`
	Managed     bool         `json:"managed,omitempty"`
}

type StopRequest struct {
	WorkspaceID string `json:"workspaceId"`
	Generation  uint64 `json:"generation"`
	Force       bool   `json:"force"`
}

type Snapshot struct {
	AdmissionClosed   bool            `json:"admissionClosed"`
	Activity          *agent.Activity `json:"activity,omitempty"`
	ActivityCheckedAt time.Time       `json:"activityCheckedAt,omitempty"`
	ActivityError     string          `json:"activityError,omitempty"`
	Request           StartRequest    `json:"request"`
	Phase             string          `json:"phase"`
	Binding           *av1.Binding    `json:"binding,omitempty"`
	RuntimeDirectory  string          `json:"runtimeDirectory,omitempty"`
	AdmissionOpen     bool            `json:"admissionOpen"`
	WorkerPID         int             `json:"workerPid,omitempty"`
	Directory         string          `json:"directory,omitempty"`
	StartMS           int64           `json:"startMs"`
	StopMS            int64           `json:"stopMs,omitempty"`
	Forced            bool            `json:"forced,omitempty"`
	HistoryFlushed    bool            `json:"historyFlushed"`
	ProcessReleased   bool            `json:"processReleased"`
}

type Manager struct {
	op                        sync.Mutex
	mu                        sync.Mutex
	cfg                       Config
	ready                     bool
	packageDigest, hostDigest string
	current                   Snapshot
	process                   *exec.Cmd
	exited                    chan struct{}
	lease                     *os.File
	client                    *transport.Client
	session                   context.Context
	cancelSession             context.CancelFunc
	rpcCount                  int
	activity                  *agent.Activity
	activityCheckedAt         time.Time
	activityError             string
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

func New(cfg Config) (*Manager, error) {
	// UID-based cleanup must never enumerate a host's or another Pod's processes.
	if os.Geteuid() != 0 || os.Getpid() != 1 {
		return nil, errors.New("guest must be root PID 1 in a private container PID namespace")
	}
	for _, path := range []string{cfg.Root, cfg.AgentRoot, cfg.Layout, cfg.Loader, cfg.Executable, cfg.Codex, cfg.Agentc} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return nil, errors.New("guest paths must be clean absolute paths")
		}
	}
	if !regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`).MatchString(cfg.Image) || !identifier.MatchString(cfg.AgentName) || !regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`).MatchString(cfg.SlotID) {
		return nil, errors.New("fixed Agent image and valid Agent name are required")
	}
	for _, path := range []string{cfg.Root, filepath.Join(cfg.Root, ".guest"), filepath.Join(cfg.Root, "workspaces")} {
		if err := rootDirectory(path, 0711); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(filepath.Join(cfg.Root, ".guest"), 0700); err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, current: Snapshot{Phase: "Warming"}, client: transport.NewClient(&http.Client{}, "http://127.0.0.1:8096", 25*time.Second)}
	if data, err := os.ReadFile(m.statePath()); err == nil {
		if err := json.Unmarshal(data, &m.current); err != nil {
			return nil, err
		}
		pids, err := uidProcesses(m.current.Request.UID)
		if err != nil || len(pids) != 0 {
			return nil, errors.New("previous slot process release is unconfirmed")
		}
		if m.current.Phase != "Stopped" && (m.current.Phase != "Suspended" || !m.current.HistoryFlushed || !m.current.ProcessReleased) {
			m.current.Phase = "Quarantined"
		} else if m.current.Phase == "Suspended" {
			if _, err := m.reserve(m.current.Request); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m, nil
}

func (m *Manager) statePath() string {
	return filepath.Join(m.cfg.Root, ".guest", "slot-"+m.cfg.SlotID+".json")
}

// Warm loads immutable content before any workspace is admitted. It does not
// bind a user or start a model request. Runtime image caching is node-owned.
func (m *Manager) Warm(ctx context.Context) error {
	m.op.Lock()
	defer m.op.Unlock()
	args := []string{"--image", m.cfg.Image, "--name", m.cfg.AgentName, "--destination", m.cfg.AgentRoot, "--package-layout", m.cfg.Layout, "--extract-only"}
	if m.cfg.PlainHTTP {
		args = append(args, "--plain-http")
	}
	cmd := exec.CommandContext(ctx, m.cfg.Loader, args...)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("preload Agent content: %w", err)
	}
	var loaded struct {
		PackageDigest string `json:"packageDigest"`
	}
	if err := json.Unmarshal(output, &loaded); err != nil {
		return err
	}
	// Workload UIDs may read immutable artifacts but cannot change their bytes.
	for _, root := range []string{m.cfg.AgentRoot, m.cfg.Layout} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0444)
			if entry.IsDir() || info.Mode()&0111 != 0 {
				mode = 0555
			}
			return os.Chmod(path, mode)
		}); err != nil {
			return err
		}
	}
	file, err := os.Open(m.cfg.Codex)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, file)
	file.Close()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packageDigest, m.hostDigest = loaded.PackageDigest, "sha256:"+hex.EncodeToString(h.Sum(nil))
	m.ready = true
	if m.current.Request.WorkspaceID == "" {
		m.current.Phase = "Idle"
	}
	return nil
}

func (m *Manager) Status() (bool, Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value := m.current
	value.Activity, value.ActivityCheckedAt, value.ActivityError = m.activity, m.activityCheckedAt, m.activityError
	if value.Phase == "Running" && m.exited != nil {
		select {
		case <-m.exited:
			value.Phase = "Failed"
		default:
		}
	}
	if value.Binding != nil {
		value.Binding = proto.Clone(value.Binding).(*av1.Binding)
	}
	return m.ready, value
}

func (m *Manager) save(value Snapshot) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	path := m.statePath()
	file, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.current = value
	m.mu.Unlock()
	return nil
}

func rootDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || stat.Uid != 0 {
		return errors.New("guest directory must be a root-owned real directory")
	}
	return os.Chmod(path, mode)
}

func ownedDirectory(path string, uid uint32) error {
	err := os.Mkdir(path, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, check := os.Lstat(path)
	if check != nil {
		return check
	}
	stat := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || (stat.Uid != uid && err != nil) {
		return errors.New("workspace directory has a different owner or is a symlink")
	}
	if err == nil {
		if err := os.Chown(path, int(uid), int(uid)); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0700)
}

// reserve keeps UID ownership and the workspace lease outside agent-writable
// directories. Sharing a filesystem does not grant two slots the same writer.
func (m *Manager) reserve(req StartRequest) (string, error) {
	private := filepath.Join(m.cfg.Root, ".guest")
	lock, err := os.OpenFile(filepath.Join(private, "identities.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", errors.New("workspace identity registry is busy")
	}
	// Store both directions so a persistent workspace never changes numeric UID.
	for key, expected := range map[string]string{"uid-" + strconv.FormatUint(uint64(req.UID), 10): req.WorkspaceID, "workspace-" + req.WorkspaceID: strconv.FormatUint(uint64(req.UID), 10)} {
		path := filepath.Join(private, key)
		data, err := os.ReadFile(path)
		if err == nil && string(data) != expected {
			return "", errors.New("workspace or UID is already allocated to another identity")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	for key, value := range map[string]string{"uid-" + strconv.FormatUint(uint64(req.UID), 10): req.WorkspaceID, "workspace-" + req.WorkspaceID: strconv.FormatUint(uint64(req.UID), 10)} {
		if err := os.WriteFile(filepath.Join(private, key), []byte(value), 0600); err != nil {
			return "", err
		}
	}
	lease, err := os.OpenFile(filepath.Join(private, "lease-"+req.WorkspaceID), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lease.Close()
		return "", errors.New("workspace already has a guest owner")
	}
	m.lease = lease
	base := filepath.Join(m.cfg.Root, "workspaces", req.WorkspaceID)
	if err := rootDirectory(base, 0711); err != nil {
		return "", err
	}
	for _, child := range []string{"work", "home", "native", "runs", "tmp"} {
		if err := ownedDirectory(filepath.Join(base, child), req.UID); err != nil {
			return "", err
		}
	}
	return base, nil
}

func (m *Manager) Start(ctx context.Context, req StartRequest) (Snapshot, error) {
	m.op.Lock()
	defer m.op.Unlock()
	begin := time.Now()
	if m.cfg.ManagedOnly && (!req.Managed || req.Binding == nil) {
		return Snapshot{}, errors.New("pooled Guests accept only managed workspace bindings")
	}
	if !identifier.MatchString(req.WorkspaceID) || req.UID < 10000 || req.UID > 2147483647 || req.Generation == 0 || len(req.RequestKey) == 0 || len(req.RequestKey) > 128 {
		return Snapshot{}, errors.New("workspace ID, dedicated UID >=10000, generation and requestKey are required")
	}
	if req.Binding != nil {
		if err := agent.ValidateBinding(req.Binding); err != nil {
			return Snapshot{}, err
		}
		if req.Binding.DeploymentId != m.cfg.AgentName || req.Binding.Epoch != req.Generation {
			return Snapshot{}, agent.NewError("Conflict", "managed claim differs from its deployment or epoch")
		}
	}
	ready, current := m.Status()
	if !ready {
		return current, errors.New("guest content is not warm")
	}
	if sameClaim(current.Request, req) && current.Phase == "Running" {
		return current, nil
	}
	if current.Phase != "Idle" && current.Phase != "Stopped" {
		return current, errors.New("slot is occupied or quarantined; stop its original generation first")
	}
	if req.Generation <= current.Request.Generation {
		return current, errors.New("generation must advance; stale claim rejected")
	}
	base, err := m.reserve(req)
	if err != nil {
		if m.lease != nil {
			m.lease.Close()
			m.lease = nil
		}
		return current, err
	}
	// Every restart has a new immutable instance/admission boundary. Only native
	// workspace state and thread ownership survive; dispatch receipts stay scoped
	// to their original generation and are never implicitly replayed on a new one.
	binding := &av1.Binding{Scope: "guest", DeploymentId: m.cfg.AgentName, WorkspaceId: req.WorkspaceID, InstanceId: m.cfg.SlotID + "-g" + strconv.FormatUint(req.Generation, 10), ReleaseId: m.packageDigest, Epoch: req.Generation}
	if req.Binding != nil {
		binding = proto.Clone(req.Binding).(*av1.Binding)
	}
	value := Snapshot{Request: req, Phase: "Starting", Binding: binding, Directory: filepath.Join(base, "work"), RuntimeDirectory: filepath.Join(base, "runs", m.cfg.SlotID, strconv.FormatUint(req.Generation, 10)), AdmissionOpen: !req.Managed}
	if err := m.save(value); err != nil {
		m.lease.Close()
		m.lease = nil
		return value, err
	}
	return m.startRuntime(ctx, value, begin)
}

func sameClaim(a, b StartRequest) bool {
	return a.WorkspaceID == b.WorkspaceID && a.UID == b.UID && a.Generation == b.Generation && a.RequestKey == b.RequestKey && a.Managed == b.Managed && proto.Equal(a.Binding, b.Binding)
}

// startRuntime preserves the immutable binding and receipts on an idle wake.
// The caller holds op and the workspace lease throughout this transition.
func (m *Manager) startRuntime(ctx context.Context, value Snapshot, begin time.Time) (Snapshot, error) {
	req := value.Request
	base := filepath.Dir(value.Directory)
	binding := value.Binding
	value.Phase, value.HistoryFlushed, value.ProcessReleased = "Starting", false, false
	if err := m.save(value); err != nil {
		return value, err
	}
	uid := strconv.FormatUint(uint64(req.UID), 10)
	cmd := exec.Command("/usr/bin/setpriv", "--reuid="+uid, "--regid="+uid, "--clear-groups", "--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs", m.cfg.Executable, "worker", "--runtime-dir="+value.RuntimeDirectory, "--state-dir="+filepath.Join(base, "native"), "--layout="+m.cfg.Layout, "--host="+m.cfg.Codex, "--agentc="+m.cfg.Agentc)
	cmd.Dir = value.Directory
	cmd.Env = append([]string{}, m.cfg.Environment...)
	cmd.Env = append(cmd.Env, "HOME="+filepath.Join(base, "home"), "TMPDIR="+filepath.Join(base, "tmp"), "AGENT_ROOT="+m.cfg.AgentRoot, "PATH="+m.cfg.AgentRoot+"/node_modules/.bin:/opt/a8s/codex/codex-path:/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	// Native stderr may contain tool arguments or credentials.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		value.Phase = "Failed"
		_ = m.save(value)
		m.lease.Close()
		m.lease = nil
		return value, err
	}
	m.mu.Lock()
	m.process, m.exited = cmd, make(chan struct{})
	exited := m.exited
	m.mu.Unlock()
	value.WorkerPID = cmd.Process.Pid
	_ = m.save(value)
	go func() { _ = cmd.Wait(); close(exited) }()
	err := m.waitPort(ctx, exited, "127.0.0.1:8096")
	if err == nil {
		err = m.waitPort(ctx, exited, "127.0.0.1:8095")
	}
	if err == nil {
		var response *av1.PrepareResponse
		response, err = m.client.Prepare(ctx, &av1.PrepareRequest{Binding: binding, PackageDigest: m.packageDigest, HostDigest: m.hostDigest, ExecServerUrl: "ws://127.0.0.1:8095", WorkspaceDirectory: value.Directory})
		if err == nil && (response.Ready == nil || !response.Ready.Ready) {
			err = errors.New("native host is not ready")
		}
	}
	if err == nil && value.AdmissionOpen {
		_, err = m.client.SetAdmission(ctx, &av1.SetAdmissionRequest{Binding: binding, Open: value.AdmissionOpen})
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		released := m.killWorkload(cleanup, req.UID) == nil
		cancel()
		value.Phase, value.ProcessReleased = "Failed", released
		if released && m.lease != nil {
			m.lease.Close()
			m.lease = nil
		}
		_ = m.save(value)
		return value, fmt.Errorf("workspace startup failed: %w", err)
	}
	value.Phase, value.StartMS = "Running", time.Since(begin).Milliseconds()
	m.mu.Lock()
	m.session, m.cancelSession = context.WithCancel(context.Background())
	m.mu.Unlock()
	if err := m.save(value); err != nil {
		return value, err
	}
	return value, nil
}

func (m *Manager) waitPort(ctx context.Context, exited <-chan struct{}, address string) error {
	for {
		select {
		case <-exited:
			return errors.New("workspace worker exited")
		default:
		}
		conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "tcp", address)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-exited:
			return errors.New("workspace worker exited")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (m *Manager) Stop(ctx context.Context, req StopRequest) (Snapshot, error) {
	m.op.Lock()
	defer m.op.Unlock()
	begin := time.Now()
	_, value := m.Status()
	if value.Request.WorkspaceID != req.WorkspaceID || value.Request.Generation != req.Generation {
		return value, errors.New("stale workspace generation")
	}
	if value.Phase == "Stopped" {
		return value, nil
	}
	if value.Binding != nil {
		_, err := m.client.SetAdmission(ctx, &av1.SetAdmissionRequest{Binding: value.Binding, Open: false})
		if err != nil && !req.Force {
			return value, err
		}
	}
	if !req.Force {
		response, err := m.client.Stop(ctx, &av1.StopRequest{Binding: value.Binding})
		if err != nil {
			return value, fmt.Errorf("safe native drain is unverified; explicit force is required for interrupting a workspace: %w", err)
		}
		if !response.ProcessStopped || !response.NativeStatePersisted {
			return value, errors.New("native process stop and history persistence are unconfirmed")
		}
		value.HistoryFlushed = true
	}
	value.Phase = "Stopping"
	if err := m.save(value); err != nil {
		return value, err
	}
	m.mu.Lock()
	if m.cancelSession != nil {
		m.cancelSession()
	}
	m.mu.Unlock()
	for {
		m.mu.Lock()
		pending := m.rpcCount
		m.mu.Unlock()
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	value.Phase, value.Forced = "Stopping", req.Force
	if err := m.save(value); err != nil {
		return value, err
	}
	if err := m.killWorkload(ctx, value.Request.UID); err != nil {
		value.Phase = "Quarantined"
		_ = m.save(value)
		return value, err
	}
	if m.lease != nil {
		m.lease.Close()
		m.lease = nil
	}
	value.Phase, value.ProcessReleased, value.StopMS = "Stopped", true, time.Since(begin).Milliseconds()
	if err := m.save(value); err != nil {
		return value, err
	}
	return value, nil
}

// The container does not delegate writable cgroups. Since all workload processes
// have a dedicated, unprivileged UID in this private PID namespace, scan by UID
// as well as process group. This also catches tools that call setsid(). Unknown
// or unkillable processes quarantine the slot instead of granting a new owner.
func (m *Manager) killWorkload(ctx context.Context, uid uint32) error {
	if m.process != nil {
		_ = syscall.Kill(-m.process.Process.Pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		pids, err := uidProcesses(uid)
		if err != nil {
			return err
		}
		for _, pid := range pids {
			signal := syscall.SIGTERM
			if time.Now().After(deadline) {
				signal = syscall.SIGKILL
			}
			_ = syscall.Kill(pid, signal)
			if m.process == nil || pid != m.process.Process.Pid {
				var status syscall.WaitStatus
				_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			}
		}
		if len(pids) == 0 {
			if m.exited != nil {
				select {
				case <-m.exited:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("workload process release is unconfirmed; slot quarantined")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func uidProcesses(uid uint32) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			for _, field := range strings.Fields(line)[1:] {
				if field == strconv.FormatUint(uint64(uid), 10) {
					pids = append(pids, pid)
					break
				}
			}
		}
	}
	return pids, nil
}

// Execute uses the existing public Agentc contract, while fencing a reused slot.
func (m *Manager) Execute(ctx context.Context, workspace string, generation uint64, call func(context.Context) error) error {
	m.mu.Lock()
	value := m.current
	value.Activity, value.ActivityCheckedAt, value.ActivityError = m.activity, m.activityCheckedAt, m.activityError
	if value.Phase != "Running" || value.Request.WorkspaceID != workspace || value.Request.Generation != generation {
		m.mu.Unlock()
		return errors.New("workspace is not running on this generation")
	}
	select {
	case <-m.exited:
		m.mu.Unlock()
		return errors.New("workspace worker exited")
	default:
	}
	m.rpcCount++
	session := m.session
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.rpcCount--; m.mu.Unlock() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(session, cancel)
	defer stop()
	return call(ctx)
}
