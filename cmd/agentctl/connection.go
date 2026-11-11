package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	execution "github.com/jingyugao/a8s/execution/gen/a8s/execution/v1"
	"github.com/jingyugao/a8s/sdk"
)

type connection struct {
	client   *sdk.Client
	resource *execution.Resource
	identity string
	close    func()
}

func platformDefaults() (map[string]string, error) {
	values := make(map[string]string)
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	paths := []string{cwd}
	if binary, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Dir(binary))
	}
	for _, start := range paths {
		for root := start; ; root = filepath.Dir(root) {
			if _, err := os.Stat(filepath.Join(root, "deploy/k3s/platform.env")); err == nil {
				for _, name := range []string{"platform.env", "platform.local.env"} {
					data, err := os.ReadFile(filepath.Join(root, "deploy/k3s", name))
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					if err != nil {
						return nil, err
					}
					for _, line := range strings.Split(string(data), "\n") {
						line = strings.TrimSpace(line)
						if line == "" || strings.HasPrefix(line, "#") {
							continue
						}
						key, value, ok := strings.Cut(line, "=")
						if !ok {
							return nil, fmt.Errorf("invalid operator configuration in %s", name)
						}
						values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
					}
				}
				break
			}
			if parent := filepath.Dir(root); parent == root {
				break
			}
		}
		if len(values) != 0 {
			break
		}
	}
	for _, key := range []string{"A8S_PLATFORM_CONTEXT", "A8S_PLATFORM_NAMESPACE", "A8S_PLATFORM_TENANT"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	return values, nil
}

func connectAgent(ctx context.Context, name string, o options) (_ *connection, resultErr error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`).MatchString(name) {
		return nil, errors.New("invalid AgentDeployment name")
	}
	close := func() {}
	defer func() {
		if resultErr != nil {
			close()
		}
	}()
	endpoint, token := o.endpoint, os.Getenv("A8S_TOKEN")
	if endpoint == "" {
		args := []string{"--namespace", o.namespace}
		if o.context != "" {
			args = append(args, "--context", o.context)
		}
		if token == "" {
			callCtx, cancel := context.WithTimeout(ctx, o.timeout)
			cmd := exec.CommandContext(callCtx, "kubectl", append(args, "get", "secret", "a8s-server", "-o", "jsonpath={.data.A8S_TOKEN}")...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			encoded, err := cmd.Output()
			cancel()
			if err != nil {
				return nil, fmt.Errorf("read API credential: %w: %s", err, strings.TrimSpace(stderr.String()))
			}
			data, err := base64.StdEncoding.DecodeString(string(encoded))
			if err != nil {
				return nil, errors.New("invalid API credential encoding")
			}
			token = string(data)
		}
		var err error
		var stop func()
		endpoint, stop, err = forward(ctx, args, "agent-"+name+"-a8s-server")
		if err != nil {
			return nil, err
		}
		close = stop
	}
	if token == "" {
		return nil, errors.New("A8S_TOKEN is required for a direct endpoint")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	client, err := sdk.New(endpoint, sdk.Options{Headers: headers, RequestTimeout: o.timeout})
	if err != nil {
		return nil, err
	}
	if o.project == "" {
		o.project = "agent-" + name
	}
	response, err := client.Control.GetDeployment(ctx, connect.NewRequest(&v1.GetDeploymentRequest{Scope: &v1.Scope{TenantId: o.tenant, ProjectId: o.project}, Id: name}))
	if err != nil {
		return nil, fmt.Errorf("connect AgentDeployment %s: %w", name, err)
	}
	if response.Msg.Deployment.GetWorkspaceId() == "" {
		return nil, errors.New("Deployment has no workspace")
	}
	workspace := response.Msg.Deployment.WorkspaceId
	if o.workspace != "" && o.workspace != workspace {
		return nil, errors.New("workspace does not belong to this AgentDeployment")
	}
	identity := endpoint
	if o.endpoint == "" {
		identity = "kube:" + o.context + ":" + o.namespace
	}
	resource := &execution.Resource{Scope: o.tenant + "/" + o.project, DeploymentId: name, WorkspaceId: workspace}
	return &connection{client: client, resource: resource, identity: identity, close: close}, nil
}

func forward(ctx context.Context, args []string, service string) (string, func(), error) {
	cmd := exec.CommandContext(ctx, "kubectl", append(args, "port-forward", "--address=127.0.0.1", "service/"+service, "0:8090")...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		pattern := regexp.MustCompile(`Forwarding from 127\.0\.0\.1:([0-9]+) ->`)
		for scanner.Scan() {
			if found := pattern.FindStringSubmatch(scanner.Text()); len(found) == 2 {
				select {
				case ready <- "http://127.0.0.1:" + found[1]:
				default:
				}
			}
		}
	}()
	go func() { done <- cmd.Wait(); close(done) }()
	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	select {
	case endpoint := <-ready:
		return endpoint, stop, nil
	case err := <-done:
		return "", nil, fmt.Errorf("port-forward failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	case <-time.After(20 * time.Second):
		stop()
		return "", nil, errors.New("port-forward did not become ready within 20 seconds")
	}
}
