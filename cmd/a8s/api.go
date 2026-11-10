package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/sdk"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func readRequest(file string, stdin io.Reader, request proto.Message) error {
	if file == "" {
		return errors.New("--file is required")
	}
	var reader io.Reader = stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		reader = f
	}
	data, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("request JSON exceeds 4 MiB")
	}
	if err := protojson.Unmarshal(data, request); err != nil {
		return errors.New("invalid request JSON; use the generated protobuf field names and types")
	}
	return nil
}

func requestKey(current *string, flagValue string) error {
	if *current != "" && flagValue != "" && *current != flagValue {
		return errors.New("--request-key differs from the request file")
	}
	if flagValue != "" {
		*current = flagValue
	}
	if *current == "" {
		return errors.New("a stable --request-key (or requestKey in JSON) is required; persist it before sending")
	}
	return nil
}

func applyScope(current **v1.Scope, c *config) {
	if *current == nil {
		*current = &v1.Scope{TenantId: c.tenant, ProjectId: c.project}
	}
}

func apiCommand(ctx context.Context, group, command string, args []string, c *config, stdin io.Reader, stderr io.Writer) (any, error) {
	fs := newFlags("a8s "+group+" "+command, c, stderr)
	file := fs.String("file", "", "protobuf JSON request file (- reads stdin)")
	id := fs.String("id", "", "stable resource or operation ID")
	deployment := fs.String("deployment", "", "Deployment ID for child resource access")
	key := fs.String("request-key", "", "caller-persisted idempotency key")
	version := fs.Uint64("version", 0, "expected resource version for a state change")
	release := fs.String("release", "", "historical Release ID for rollback")
	workspace := fs.String("workspace", "", "Workspace ID for execution target access")
	pageSize := fs.Uint("page-size", 100, "Deployment list page size")
	pageToken := fs.String("page-token", "", "Deployment list continuation token")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, errors.New("unexpected positional arguments; use named flags")
	}
	if c.timeout <= 0 || c.waitTimeout <= 0 {
		return nil, errors.New("timeouts must be positive")
	}
	headers := make(http.Header)
	if token := os.Getenv("A8S_TOKEN"); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	client, err := sdk.New(c.endpoint, sdk.Options{Headers: headers, RequestTimeout: c.timeout})
	if err != nil {
		return nil, err
	}
	scope := &v1.Scope{TenantId: c.tenant, ProjectId: c.project}
	var response proto.Message
	switch group + " " + command {
	case "agentc register":
		req := &v1.RegisterAgentcRequest{}
		if err := readRequest(*file, stdin, req); err != nil {
			return nil, err
		}
		if req.Agentc == nil {
			return nil, errors.New("agentc is required")
		}
		applyScope(&req.Agentc.Scope, c)
		res, err := client.Control.RegisterAgentc(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "agentc get":
		res, err := client.Control.GetAgentc(ctx, connect.NewRequest(&v1.GetAgentcRequest{Scope: scope, Id: *id}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "runtime create":
		req := &v1.CreateRuntimeRevisionRequest{}
		if err := readRequest(*file, stdin, req); err != nil {
			return nil, err
		}
		if req.RuntimeRevision == nil {
			return nil, errors.New("runtimeRevision is required")
		}
		applyScope(&req.RuntimeRevision.Scope, c)
		res, err := client.Control.CreateRuntimeRevision(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "runtime get":
		res, err := client.Control.GetRuntimeRevision(ctx, connect.NewRequest(&v1.GetRuntimeRevisionRequest{Scope: scope, Id: *id}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "deployment create":
		req := &v1.CreateDeploymentRequest{}
		if err := readRequest(*file, stdin, req); err != nil {
			return nil, err
		}
		applyScope(&req.Scope, c)
		scope = req.Scope
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Control.CreateDeployment(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "deployment get":
		res, err := client.Control.GetDeployment(ctx, connect.NewRequest(&v1.GetDeploymentRequest{Scope: scope, Id: *id}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "deployment list":
		if *pageSize > 1000 {
			return nil, errors.New("--page-size must be at most 1000")
		}
		res, err := client.Control.ListDeployments(ctx, connect.NewRequest(&v1.ListDeploymentsRequest{Scope: scope, PageSize: uint32(*pageSize), PageToken: *pageToken}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "deployment update":
		req := &v1.UpdateDeploymentRequest{}
		if err := readRequest(*file, stdin, req); err != nil {
			return nil, err
		}
		applyScope(&req.Scope, c)
		scope = req.Scope
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Control.UpdateDeployment(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "deployment online", "deployment offline":
		req := &v1.SetDesiredStateRequest{Scope: scope, Id: *id, ExpectedResourceVersion: *version}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			applyScope(&req.Scope, c)
			scope = req.Scope
		}
		if command == "online" {
			req.DesiredState = v1.DesiredState_DESIRED_STATE_ONLINE
		} else {
			req.DesiredState = v1.DesiredState_DESIRED_STATE_OFFLINE
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Control.SetDesiredState(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "deployment rollback":
		req := &v1.RollbackRequest{Scope: scope, Id: *id, ReleaseId: *release, ExpectedResourceVersion: *version}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			applyScope(&req.Scope, c)
			scope = req.Scope
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Control.Rollback(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "operation get", "operation wait":
		req := &v1.GetOperationRequest{Scope: scope, DeploymentId: *deployment, Id: *id}
		if command == "wait" {
			op, err := client.WaitOperation(ctx, req, c.waitTimeout)
			if op == nil {
				return nil, err
			}
			return &v1.GetOperationResponse{Operation: op}, err
		}
		res, err := client.Control.GetOperation(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "workspace get":
		res, err := client.Control.GetWorkspace(ctx, connect.NewRequest(&v1.GetWorkspaceRequest{Scope: scope, DeploymentId: *deployment, Id: *id}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "release get":
		res, err := client.Control.GetRelease(ctx, connect.NewRequest(&v1.GetReleaseRequest{Scope: scope, DeploymentId: *deployment, Id: *id}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	case "target get":
		res, err := client.Control.ReadExecutionTarget(ctx, connect.NewRequest(&v1.ReadExecutionTargetRequest{Scope: scope, DeploymentId: *deployment, WorkspaceId: *workspace}))
		if err != nil {
			return nil, err
		}
		response = res.Msg
	default:
		return nil, fmt.Errorf("unknown command %q", group+" "+command)
	}
	if c.wait {
		if withOp, ok := response.(interface{ GetOperation() *v1.Operation }); ok {
			op := withOp.GetOperation()
			if op == nil {
				return response, errors.New("mutation returned no Operation")
			}
			latest, err := client.WaitOperation(ctx, &v1.GetOperationRequest{Scope: scope, DeploymentId: op.DeploymentId, Id: op.Id}, c.waitTimeout)
			if latest != nil {
				switch res := response.(type) {
				case *v1.CreateDeploymentResponse:
					res.Operation = latest
				case *v1.UpdateDeploymentResponse:
					res.Operation = latest
				case *v1.SetDesiredStateResponse:
					res.Operation = latest
				case *v1.RollbackResponse:
					res.Operation = latest
				}
			}
			return response, err
		}
	}
	return response, nil
}
