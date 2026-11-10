//go:build linux

// Command agentc runs one Codex Agentc per Instance, outside its execution Box.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jingyugao/a8s/agentc/codex"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"github.com/jingyugao/a8s/agentc/contract/transport"
)

var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentc: startup or shutdown failed; inspect configured paths and runtime dependencies")
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", "127.0.0.1:8094", "Connect listen address; service-mesh identity is owned by deployment")
	host := flag.String("host", "", "path to the pinned native Codex CLI binary")
	runtimeDir := flag.String("runtime-dir", "", "durable, dedicated directory for this immutable Instance")
	stateDir := flag.String("state-dir", "", "optional persistent workspace-native state, protected by its own writer lock")
	layout := flag.String("package-layout", "", "verified OCI package layout, materialized by fixed digest")
	envNames := flag.String("pass-env", "PATH,LANG,TZ", "comma-separated environment variable names to inject into the host; values are never logged")
	timeout := flag.Duration("timeout", 30*time.Second, "unary native RPC timeout")
	idle := flag.Bool("idle-management", false, "enable loopback-only Guest runtime observation and graceful parking")
	describe := flag.Bool("describe", false, "print independent Agentc and host digest/version metadata")
	flag.Parse()
	if *describe {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		ownDigest, err := fileDigest(self)
		if err != nil {
			return err
		}
		hostDigest, err := fileDigest(*host)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"version": version, "adapterDigest": ownDigest, "hostDigest": hostDigest, "hostKind": "codex-cli", "validatedCodexVersion": codex.CodexVersion, "topology": "outside-box-single-instance", "protocol": "a8s.agentc.v1"})
	}
	a, err := codex.New(codex.Config{HostPath: *host, RuntimeDirectory: *runtimeDir, StateDirectory: *stateDir, Packages: codex.LocalLayout{Directory: *layout}, Timeout: *timeout, IdleManagement: *idle, Environment: func(ctx context.Context, b *v1.Binding) (map[string]string, error) {
		values := map[string]string{}
		for _, name := range strings.Split(*envNames, ",") {
			if value, ok := os.LookupEnv(name); ok {
				values[name] = value
			}
		}
		return values, nil
	}})
	if err != nil {
		return err
	}
	path, handler := transport.NewHandler(a, *timeout, connect.WithReadMaxBytes(2<<20))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	if *idle {
		if !strings.HasPrefix(*listen, "127.0.0.1:") {
			return fmt.Errorf("idle management requires a loopback listener")
		}
		mux.Handle("/runtime/", a.RuntimeHandler())
	}
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, WriteTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ended := make(chan error, 1)
	go func() { ended <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err := <-ended:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.Close() // Cancel live subscriptions before waiting on the owned host.
	return a.Shutdown(shutdown)
}
func fileDigest(path string) (string, error) {
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
