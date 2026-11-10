//go:build linux

package guest

import (
	"encoding/json"
	"errors"
	agent "github.com/jingyugao/a8s/agentc/contract"
	"github.com/jingyugao/a8s/infra/processes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Worker is always entered after dropping UID, supplementary groups, capabilities
// and privilege escalation. Both remote shell execution and Codex inherit this
// identity. The root manager is never used as an agent's shell executor.
func Worker(runtime, state, layout, host, agentc string) error {
	if os.Geteuid() < 10000 {
		return errors.New("worker requires an unprivileged workspace UID")
	}
	executor := exec.Command("/usr/local/bin/codex-exec-server", "--listen", "ws://127.0.0.1:8095")
	adapter := exec.Command(agentc, "--listen=127.0.0.1:8096", "--idle-management", "--host="+host, "--runtime-dir="+runtime, "--state-dir="+state, "--package-layout="+layout, "--pass-env=PATH,HOME,TMPDIR,LANG,AGENT_ROOT,OPENAI_API_KEY,COLONY_MODEL_API_KEY")
	children := []*exec.Cmd{executor, adapter}
	ended := make(chan error, 2)
	for _, child := range children {
		child.Stderr = io.Discard
		child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := child.Start(); err != nil {
			for _, old := range children {
				if old.Process != nil {
					_ = old.Process.Kill()
				}
			}
			return err
		}
		go func(cmd *exec.Cmd) { ended <- cmd.Wait() }(child)
	}

	if err := os.MkdirAll(runtime, 0700); err != nil {
		return err
	}
	var identities []agent.ProcessIdentity
	for _, pid := range []int{os.Getpid(), executor.Process.Pid, adapter.Process.Pid} {
		info, err := processes.Read(pid)
		if err != nil {
			return err
		}
		identities = append(identities, info.ProcessIdentity)
	}
	data, _ := json.Marshal(identities)
	if err := os.WriteFile(filepath.Join(runtime, "worker-processes.json"), data, 0600); err != nil {
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	var err error
	remaining := 2
	select {
	case err = <-ended:
		remaining--
	case <-stop:
	}
	for _, child := range children {
		_ = child.Process.Signal(syscall.SIGTERM)
	}
	for remaining > 0 {
		select {
		case <-ended:
			remaining--
		case <-time.After(8 * time.Second):
			return errors.New("framework process exit is unconfirmed")
		}
	}
	return err
}
