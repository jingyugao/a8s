//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control/controllers"
	"github.com/jingyugao/a8s/guest"
	"google.golang.org/protobuf/encoding/protojson"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "guest:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		f := flag.NewFlagSet("worker", flag.ContinueOnError)
		runtime := f.String("runtime-dir", "", "workspace state directory")
		state := f.String("state-dir", "", "persistent workspace-native state")
		layout := f.String("layout", "", "prepared package layout")
		host := f.String("host", "", "platform Codex binary")
		adapter := f.String("agentc", "", "platform Agentc binary")
		if err := f.Parse(os.Args[2:]); err != nil {
			return err
		}
		return guest.Worker(*runtime, *state, *layout, *host, *adapter)
	}
	listen := flag.String("listen", "0.0.0.0:8090", "guest API listen address")
	root := flag.String("root", "/runtime", "mounted shared workspace storage")
	flag.Parse()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Do not pass the guest API token or any control-plane/database credentials to
	// workload processes. Model configuration stays owned by this application.
	var environment []string
	for _, name := range []string{"OPENAI_API_KEY", "COLONY_MODEL_API_KEY", "TZ"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	manager, err := guest.New(guest.Config{Root: *root, SlotID: os.Getenv("A8S_GUEST_SLOT"), AgentRoot: "/agent", Layout: "/opt/a8s/package", Image: os.Getenv("A8S_AGENT_IMAGE"), AgentName: os.Getenv("A8S_AGENT_NAME"), Loader: "/usr/local/bin/a8s-agent-loader", Executable: self, Codex: "/usr/local/bin/codex", Agentc: "/usr/local/bin/a8s-codex-agentc", Environment: environment, PlainHTTP: os.Getenv("A8S_REGISTRY_PLAIN_HTTP") == "true"})
	if err != nil {
		return err
	}
	policy := &v1.IdleReclaimPolicy{}
	if raw := os.Getenv("A8S_IDLE_RECLAIM"); raw != "" {
		if err := protojson.Unmarshal([]byte(raw), policy); err != nil {
			return err
		}
	}
	idle, err := controllers.NewIdleController(policy, manager)
	if err != nil {
		return err
	}
	token := os.Getenv("A8S_GUEST_TOKEN")
	if token == "" {
		token = os.Getenv("A8S_TOKEN")
	}
	handler, err := guest.Handler(manager, idle, token)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 45 * time.Second, IdleTimeout: time.Minute}
	ended := make(chan error, 1)
	go func() { ended <- server.ListenAndServe() }()
	warming, endWarm := context.WithTimeout(ctx, 3*time.Minute)
	err = manager.Warm(warming)
	endWarm()
	if err != nil {
		_ = server.Close()
		return err
	}
	fmt.Println("guest content warm; waiting for a workspace claim")
	go idle.Run(ctx, func(err error) { fmt.Fprintln(os.Stderr, "idle check retained workspace:", err) })
	select {
	case <-ctx.Done():
	case err = <-ended:
	}
	_, value := manager.Status()
	if value.Request.WorkspaceID != "" && !value.ProcessReleased {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = manager.Stop(cleanup, guest.StopRequest{WorkspaceID: value.Request.WorkspaceID, Generation: value.Request.Generation, Force: true})
		stop()
	}
	_ = server.Close()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
