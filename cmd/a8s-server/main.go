// Command a8s-server assembles Control, persistence and lifecycle adapters.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jingyugao/a8s/api"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/artifacts"
	"github.com/jingyugao/a8s/control"
	"github.com/jingyugao/a8s/control/controllers"
	"github.com/jingyugao/a8s/control/resources"
	"github.com/jingyugao/a8s/execution"
	"github.com/jingyugao/a8s/storage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("a8s-server stopped", "error", err)
		os.Exit(1)
	}
}

func apiToken() (string, error) {
	token := os.Getenv("A8S_TOKEN")
	if path := os.Getenv("A8S_TOKEN_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("cannot read A8S_TOKEN_FILE")
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return "", errors.New("A8S_TOKEN or A8S_TOKEN_FILE is required")
	}
	return token, nil
}

func authenticate(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "caller identity is required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("a8s-server", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8090", "HTTP listen address")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	token, err := apiToken()
	if err != nil {
		return err
	}
	scope := &v1.Scope{TenantId: os.Getenv("A8S_TENANT"), ProjectId: os.Getenv("A8S_PROJECT")}
	if err := resources.ValidateScope(scope); err != nil {
		return err
	}
	dsn := os.Getenv("A8S_DATABASE_URL")
	if dsn == "" {
		return errors.New("A8S_DATABASE_URL is required; apply storage/control_schema.sql explicitly before startup")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid A8S_DATABASE_URL")
	}
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return errors.New("cannot initialize Control database pool")
	}
	defer pool.Close()
	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(startupCtx); err != nil {
		return errors.New("cannot connect to Control database")
	}
	store, err := storage.NewControlPostgres(pool)
	if err != nil {
		return err
	}
	if err := store.CheckSchema(startupCtx); err != nil {
		return errors.New("Control schema unavailable; apply storage/control_schema.sql explicitly")
	}
	registryHost := os.Getenv("A8S_REGISTRY_HOST")
	if registryHost == "" && (os.Getenv("A8S_REGISTRY_USERNAME") != "" || os.Getenv("A8S_REGISTRY_PASSWORD") != "" || os.Getenv("A8S_REGISTRY_TOKEN") != "") {
		return errors.New("A8S_REGISTRY_HOST is required when registry credentials are configured")
	}
	plainHTTP := false
	if value := os.Getenv("A8S_REGISTRY_PLAIN_HTTP"); value != "" {
		plainHTTP, err = strconv.ParseBool(value)
		if err != nil {
			return errors.New("invalid A8S_REGISTRY_PLAIN_HTTP")
		}
	}
	registry, err := artifacts.NewRegistry(artifacts.RegistryOptions{PlainHTTP: plainHTTP, Timeout: 15 * time.Second, Credential: func(_ context.Context, host string) (artifacts.Credential, error) {
		if host != registryHost {
			return artifacts.Credential{}, nil
		}
		return artifacts.Credential{Username: os.Getenv("A8S_REGISTRY_USERNAME"), Password: os.Getenv("A8S_REGISTRY_PASSWORD"), AccessToken: os.Getenv("A8S_REGISTRY_TOKEN")}, nil
	}})
	if err != nil {
		return errors.New("cannot configure artifact registry")
	}
	dependencies := control.Dependencies{Resolver: &artifactAdapter{registry: registry}}
	if namespace := os.Getenv("A8S_KUBERNETES_NAMESPACE"); namespace != "" {
		mount := os.Getenv("A8S_WORKSPACE_DIRECTORY")
		if mount == "" {
			mount = "/home/agent"
		}
		dependencies.Environment, err = newEnvironment(namespace, mount)
		if err != nil {
			return err
		}
	}
	if endpoint := os.Getenv("A8S_AGENTC_URL"); endpoint != "" {
		directory := os.Getenv("A8S_WORKSPACE_DIRECTORY")
		if directory == "" {
			directory = "/home/agent"
		}
		agentcToken := os.Getenv("A8S_AGENTC_TOKEN")
		if agentcToken == "" {
			agentcToken = token
		}
		dependencies.Executor, err = newAgentAdapter(endpoint, agentcToken, os.Getenv("A8S_EXEC_SERVER_URL"), directory)
		if err != nil {
			return err
		}
	}
	service := &control.Service{Store: store, Dependencies: dependencies}
	path, handler, err := control.NewHandler(service, func(ctx context.Context, requested *v1.Scope, action, id string) error {
		if requested.GetTenantId() != scope.TenantId || requested.GetProjectId() != scope.ProjectId {
			return api.NewError(v1.ErrorCategory_ERROR_CATEGORY_FORBIDDEN, "caller is not authorized for this scope")
		}
		return nil
	})
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(path, authenticate(token, handler))
	if environment, ok := dependencies.Environment.(*environmentAdapter); ok && environment.pool != nil {
		mux.Handle("GET /v1/warm-pool", authenticate(token, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
			defer cancel()
			status, err := environment.pool.Status(ctx)
			if err != nil {
				http.Error(w, "warm pool state unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(status)
		})))
	}
	// Execution owns its tables, admission records and handlers. Assembly only
	// supplies Control's read-only target, scoped authorization and remote Agentc.
	executionDB := stdlib.OpenDBFromPool(pool)
	defer executionDB.Close()
	if dependencies.Executor != nil {
		rows, err := executionDB.QueryContext(startupCtx, "SELECT r.request_id, t.thread_id FROM a8s_execution_requests r CROSS JOIN a8s_execution_threads t LIMIT 0")
		if err != nil {
			return errors.New("Execution schema unavailable; apply storage/execution.sql explicitly")
		}
		rows.Close()
	}
	agents, _ := dependencies.Executor.(*agentAdapter)
	routing := &executionRouting{service: service, agents: agents, scope: scope}
	gateway, err := execution.New(&storage.ExecutionSQL{DB: executionDB}, routing, routing, routing, 15*time.Second)
	if err != nil {
		return err
	}
	executionPath, executionHandler := execution.NewHandler(gateway, connect.WithReadMaxBytes(1<<20))
	mux.Handle(executionPath, authenticate(token, executionHandler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		checkCtx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if dependencies.Environment == nil || dependencies.Executor == nil || pool.Ping(checkCtx) != nil {
			http.Error(w, "dependencies unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// Unary work has application deadlines; live event subscriptions can outlive
	// them and must not be cut by a server-wide absolute write deadline.
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second}
	controllerCtx, stopController := context.WithCancel(ctx)
	defer stopController()
	controllerDone := make(chan struct{})
	if environment, ok := dependencies.Environment.(*environmentAdapter); ok && environment.pool != nil {
		go environment.pool.Run(controllerCtx, func(err error) { slog.Error("warm pool reconcile failed", "error", err) })
	}
	reconciler := &controllers.Reconciler{Store: store, Scope: scope, Dependencies: dependencies, CallTimeout: 15 * time.Second}
	go func() {
		defer close(controllerDone)
		_ = reconciler.Run(controllerCtx, time.Second, func(err error) { slog.Error("Control reconcile failed", "error", err) })
	}()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	slog.Info("a8s-server listening", "address", listener.Addr().String(), "scope", api.ScopeKey(scope), "lifecycle_configured", dependencies.Environment != nil && dependencies.Executor != nil)
	select {
	case <-ctx.Done():
	case err = <-serveDone:
	}
	stopController()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
	}
	select {
	case <-controllerDone:
	case <-shutdownCtx.Done():
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
