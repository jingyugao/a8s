// Command a8s is the a8s CLI entrypoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/jingyugao/a8s/artifacts"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/jingyugao/a8s/sdk"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type config struct {
	json                      bool
	endpoint, tenant, project string
	timeout, waitTimeout      time.Duration
	wait                      bool
}

func (c *config) flags(fs *flag.FlagSet) {
	fs.BoolVar(&c.json, "json", c.json, "write structured JSON")
	fs.StringVar(&c.endpoint, "endpoint", c.endpoint, "a8s API URL (A8S_ENDPOINT)")
	fs.StringVar(&c.tenant, "tenant", c.tenant, "tenant ID (A8S_TENANT)")
	fs.StringVar(&c.project, "project", c.project, "project ID (A8S_PROJECT)")
	fs.DurationVar(&c.timeout, "timeout", c.timeout, "timeout for a single call")
	fs.BoolVar(&c.wait, "wait", c.wait, "wait for a returned lifecycle operation")
	fs.DurationVar(&c.waitTimeout, "wait-timeout", c.waitTimeout, "bounded operation wait")
}

func newFlags(name string, c *config, errOut io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	c.flags(fs)
	return fs
}

const usage = `Usage: a8s [OPTIONS] GROUP COMMAND [OPTIONS]

package     init, validate, inspect, build, publish, resolve, fetch
agentc      register, get
runtime     create, get
deployment  create, get, list, update, online, offline, rollback
operation   get, wait
workspace   get
release     get
target      get
execution   open-thread, submit, get, lookup, cancel, respond, events

Use GROUP COMMAND --help for flags. Mutation bodies use --file JSON (or - for stdin).
Persist --request-key and reuse it when reconciling an uncertain network result.
Use --json for scripts and --wait for lifecycle completion.
API identity: A8S_TOKEN. Registry identity: A8S_REGISTRY_USERNAME and
A8S_REGISTRY_PASSWORD, or A8S_REGISTRY_TOKEN. Credentials are never flags.
`

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c := &config{endpoint: os.Getenv("A8S_ENDPOINT"), tenant: os.Getenv("A8S_TENANT"), project: os.Getenv("A8S_PROJECT"), timeout: 30 * time.Second, waitTimeout: 2 * time.Minute}
	global := newFlags("a8s", c, stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage); global.PrintDefaults() }
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	args = global.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if len(args) < 2 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var result any
	var err error
	if args[0] == "package" {
		result, err = packageCommand(ctx, args[1], args[2:], c, stdout, stderr)
	} else if args[0] == "execution" {
		result, err = executionCommand(ctx, args[1], args[2:], c, stdin, stdout, stderr)
	} else {
		result, err = apiCommand(ctx, args[0], args[1], args[2:], c, stdin, stderr)
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		detail := map[string]any{"message": err.Error()}
		var requestError *sdk.RequestError
		if errors.As(err, &requestError) {
			detail["code"] = requestError.Code
			detail["category"] = requestError.Category
			detail["source"] = requestError.Source
			detail["nativeCode"] = requestError.NativeCode
			detail["requestKey"] = requestError.RequestKey
			detail["requestId"] = requestError.RequestID
			detail["resourceId"] = requestError.ResourceID
			detail["operationId"] = requestError.OperationID
			detail["outcomeUnknown"] = requestError.OutcomeUnknown
			if requestError.Detail != nil {
				detail["detail"] = json.RawMessage(mustJSON(requestError.Detail))
			}
		}
		var waitError *sdk.WaitError
		if errors.As(err, &waitError) {
			detail["operationId"] = waitError.Request.GetId()
			if result == nil && waitError.Operation != nil {
				result = waitError.Operation
			}
		}
		if c.json {
			_ = writeJSON(stdout, map[string]any{"error": detail, "result": jsonValue(result)})
		} else {
			if result != nil {
				_ = writeHuman(stdout, result)
			}
			fmt.Fprintln(stderr, "a8s:", err)
			if requestError != nil && (requestError.RequestKey != "" || requestError.ResourceID != "" || requestError.RequestID != "" || requestError.OperationID != "") {
				fmt.Fprintf(stderr, "request key: %s; request ID: %s; resource: %s; operation: %s\n", requestError.RequestKey, requestError.RequestID, requestError.ResourceID, requestError.OperationID)
			}
		}
		return 1
	}
	if result != nil {
		if c.json {
			err = writeJSON(stdout, jsonValue(result))
		} else {
			err = writeHuman(stdout, result)
		}
		if err != nil {
			fmt.Fprintln(stderr, "a8s:", err)
			return 1
		}
	}
	return 0
}

func mustJSON(message proto.Message) []byte {
	data, _ := protojson.MarshalOptions{Indent: "  "}.Marshal(message)
	return data
}
func jsonValue(value any) any {
	if message, ok := value.(proto.Message); ok {
		return json.RawMessage(mustJSON(message))
	}
	return value
}
func writeJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
func writeHuman(out io.Writer, value any) error {
	switch v := value.(type) {
	case artifacts.BuildResult:
		if _, err := fmt.Fprintln(out, "layout:", v.Layout); err != nil {
			return err
		}
		return writeHuman(out, v.Package)
	case artifacts.Package:
		_, err := fmt.Fprintf(out, "package: %s\ndigest: %s\nreference: %s\nfiles: %d\n", v.Metadata.Manifest.Name, v.Digest, v.Reference, len(v.Metadata.Files))
		return err
	case artifacts.Metadata:
		_, err := fmt.Fprintf(out, "package: %s\nformat: %s\ninstructions: %s\nskills: %d\nCLI entries: %d\nfiles: %d\n", v.Manifest.Name, v.Manifest.SchemaVersion, v.Manifest.Instructions, len(v.Manifest.Skills), len(v.Manifest.CLIs), len(v.Files))
		return err
	}
	data, err := json.Marshal(jsonValue(value))
	if err != nil {
		return err
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	var printValue func(string, any) error
	printValue = func(prefix string, value any) error {
		if object, ok := value.(map[string]any); ok {
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				name := key
				if prefix != "" {
					name = prefix + "." + key
				}
				if err := printValue(name, object[key]); err != nil {
					return err
				}
			}
			return nil
		}
		if text, ok := value.(string); ok {
			_, err := fmt.Fprintf(out, "%s: %s\n", prefix, text)
			return err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s: %s\n", prefix, encoded)
		return err
	}
	return printValue("", parsed)
}
