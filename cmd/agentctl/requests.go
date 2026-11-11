package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"connectrpc.com/connect"
	v1 "github.com/jingyugao/a8s/execution/gen/a8s/execution/v1"
	"google.golang.org/protobuf/proto"
)

// Retain request identities, not credentials, input text or a second transcript.
type request struct {
	Key, Method, ThreadID, RequestID, TurnID, State string
}

func (c *chat) resource(thread string) *v1.Resource {
	r := proto.Clone(c.connection.resource).(*v1.Resource)
	r.ThreadId = thread
	return r
}

func (c *chat) requestPath(key string) (string, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(key) {
		return "", errors.New("invalid local request key")
	}
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local/state")
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("XDG_STATE_HOME must be absolute")
	}
	r := c.connection.resource
	hash := sha256.Sum256([]byte(c.connection.identity + "\x00" + r.Scope + "\x00" + r.DeploymentId + "\x00" + r.WorkspaceId))
	return filepath.Join(root, "agentctl/requests", fmt.Sprintf("%x", hash[:16]), key+".json"), nil
}

func (c *chat) save(r *request) error {
	path, err := c.requestPath(r.Key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".request-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(r); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (c *chat) intent(method string) (*request, error) {
	r := &request{Key: rand.Text(), Method: method, ThreadID: c.thread, State: "Dispatching"}
	if err := c.save(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (c *chat) record(r *request, record *v1.Record) error {
	if record == nil || record.RequestId == "" {
		return errors.New("server returned no request identity")
	}
	if r.RequestID != "" && r.RequestID != record.RequestId {
		return errors.New("server returned a different request identity")
	}
	if r.Method != "OpenThread" && r.ThreadID != "" && record.ThreadId != "" && r.ThreadID != record.ThreadId {
		return errors.New("server returned a different Thread identity")
	}
	r.RequestID, r.TurnID, r.State = record.RequestId, record.TurnId, record.State
	if record.ThreadId != "" {
		r.ThreadID = record.ThreadId
	}
	return c.save(r)
}

func (c *chat) lookup(ctx context.Context, r *request) error {
	response, err := c.connection.client.Execution.Lookup(ctx, connect.NewRequest(&v1.LookupRequest{Resource: c.resource(r.ThreadID), Kind: r.Method, RequestKey: r.Key}))
	if err != nil {
		return err
	}
	return c.record(r, response.Msg.Record)
}

func (c *chat) unresolved(r *request, cause error) error {
	path, _ := c.requestPath(r.Key)
	return fmt.Errorf("%w\nRequest retained at %s\nReconcile with: agentctl recover %s %s (use the same connection options)", cause, path, c.connection.resource.DeploymentId, r.Key)
}

func (c *chat) open(ctx context.Context) error {
	r, err := c.intent("OpenThread")
	if err != nil {
		return err
	}
	c.pending = r
	response, callErr := c.connection.client.Execution.OpenThread(ctx, connect.NewRequest(&v1.OpenThreadRequest{Resource: c.resource(""), RequestKey: r.Key}))
	if callErr != nil {
		if err := c.lookup(ctx, r); err != nil {
			return c.unresolved(r, callErr)
		}
	} else if err := c.record(r, response.Msg.Record); err != nil {
		return c.unresolved(r, err)
	}
	if r.ThreadID == "" {
		return c.unresolved(r, errors.New("thread creation is still unconfirmed"))
	}
	c.thread, c.pending, c.before = r.ThreadID, nil, ""
	fmt.Fprintln(c.out(), "Thread:", c.thread)
	return nil
}

func (c *chat) recover(ctx context.Context, key string) error {
	path, err := c.requestPath(key)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var r request
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.Key != key {
		return errors.New("request journal identity mismatch")
	}
	if err := c.lookup(ctx, &r); err != nil {
		return c.unresolved(&r, err)
	}
	fmt.Fprintf(c.out(), "%s: %s · request %s · thread %s\n", r.Method, r.State, r.RequestID, r.ThreadID)
	if r.Method == "SubmitInput" {
		return c.watch(ctx, &r)
	}
	if r.Method == "OpenThread" && r.ThreadID == "" {
		return c.unresolved(&r, errors.New("thread creation is still unconfirmed"))
	}
	return nil
}
