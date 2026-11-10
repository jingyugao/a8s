package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/jingyugao/a8s/execution/gen/a8s/execution/v1"
	"github.com/jingyugao/a8s/sdk"
	"google.golang.org/protobuf/encoding/protojson"
)

func executionCommand(ctx context.Context, command string, args []string, c *config, stdin io.Reader, stdout, stderr io.Writer) (any, error) {
	fs := newFlags("a8s execution "+command, c, stderr)
	file := fs.String("file", "", "protobuf JSON request file (- for stdin)")
	deployment := fs.String("deployment", "", "Deployment ID")
	workspace := fs.String("workspace", "", "Workspace ID")
	thread := fs.String("thread", "", "Thread ID")
	id := fs.String("id", "", "original input request ID")
	key := fs.String("request-key", "", "caller-persisted idempotency key")
	text := fs.String("text", "", "input text")
	author := fs.String("author", "", "author ID supplied by the upper application")
	turn := fs.String("turn", "", "original Turn ID")
	interaction := fs.String("interaction", "", "interaction ID")
	decision := fs.String("decision", "", "interaction decision")
	kind := fs.String("kind", "SubmitInput", "request kind for lookup")
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
	resource := &v1.Resource{Scope: c.tenant + "/" + c.project, DeploymentId: *deployment, WorkspaceId: *workspace, ThreadId: *thread}
	fill := func(r **v1.Resource) {
		if *r == nil {
			*r = resource
		}
	}
	switch command {
	case "open-thread":
		req := &v1.OpenThreadRequest{Resource: resource}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Execution.OpenThread(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	case "submit":
		req := &v1.SubmitInputRequest{Resource: resource, Text: *text, AuthorId: *author}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Execution.SubmitInput(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	case "get":
		req := &v1.GetInputResultRequest{Resource: resource, RequestId: *id}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		res, err := client.Execution.GetInputResult(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	case "lookup":
		req := &v1.LookupRequest{Resource: resource, Kind: *kind}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Execution.Lookup(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	case "cancel":
		req := &v1.CancelRequest{Resource: resource, InputRequestId: *id, TurnId: *turn}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Execution.Cancel(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	case "respond":
		req := &v1.RespondInteractionRequest{Resource: resource, InputRequestId: *id, TurnId: *turn, InteractionId: *interaction, Decision: *decision}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		if err := requestKey(&req.RequestKey, *key); err != nil {
			return nil, err
		}
		res, err := client.Execution.RespondInteraction(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	case "events":
		req := &v1.SubscribeRequest{Resource: resource, InputRequestId: *id}
		if *file != "" {
			if err := readRequest(*file, stdin, req); err != nil {
				return nil, err
			}
			fill(&req.Resource)
		}
		ctx, cancel := context.WithTimeout(ctx, c.waitTimeout)
		defer cancel()
		stream, err := client.Execution.Subscribe(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		for stream.Receive() {
			if c.json {
				data, err := protojson.Marshal(stream.Msg())
				if err != nil {
					return nil, err
				}
				if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
					return nil, err
				}
			} else if err := writeHuman(stdout, stream.Msg()); err != nil {
				return nil, err
			}
		}
		if err := stream.Err(); err != nil {
			return nil, fmt.Errorf("live event subscription ended for input %s; query its result: %w", req.InputRequestId, err)
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown execution command %q", command)
	}
}
