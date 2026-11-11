// Command agentctl connects an operator's terminal to an AgentDeployment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const usage = `Usage:
  agentctl chat AGENT [options]                Start a new conversation
  agentctl resume AGENT [THREAD_ID] [options]  Choose or resume a conversation
  agentctl threads AGENT [options]             List conversations
  agentctl recover AGENT REQUEST_KEY [options] Query an interrupted request

Connects through kubectl port-forward by default. Inside a source checkout,
deploy/k3s/platform.local.env supplies the operator defaults. An explicit
--endpoint uses A8S_TOKEN instead of Kubernetes secret discovery.

Chat commands: /new, /resume [THREAD_ID], /threads, /history, /older,
              /retry, /cancel, /help, /quit.
Ctrl-C while waiting detaches the client; the remote task keeps running.
`

type options struct {
	context, namespace, endpoint, tenant, project, workspace, thread string
	timeout, wait                                                    time.Duration
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentctl:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	defaults, err := platformDefaults()
	if err != nil {
		return err
	}
	o := options{context: defaults["A8S_PLATFORM_CONTEXT"], namespace: defaults["A8S_PLATFORM_NAMESPACE"], tenant: defaults["A8S_PLATFORM_TENANT"], timeout: 30 * time.Second, wait: 5 * time.Minute}
	if o.namespace == "" {
		o.namespace = "a8s"
	}
	if o.tenant == "" {
		o.tenant = "default"
	}
	o.endpoint, o.project = os.Getenv("A8S_ENDPOINT"), os.Getenv("A8S_PROJECT")
	if value := os.Getenv("A8S_TENANT"); value != "" {
		o.tenant = value
	}
	fs := flag.NewFlagSet("agentctl", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	fs.StringVar(&o.context, "context", o.context, "kubectl context (otherwise current context)")
	fs.StringVar(&o.namespace, "namespace", o.namespace, "Kubernetes namespace")
	fs.StringVar(&o.namespace, "n", o.namespace, "Kubernetes namespace")
	fs.StringVar(&o.endpoint, "endpoint", o.endpoint, "connect directly to an API endpoint")
	fs.StringVar(&o.tenant, "tenant", o.tenant, "tenant ID")
	fs.StringVar(&o.project, "project", o.project, "project ID (default agent-AGENT)")
	fs.StringVar(&o.workspace, "workspace", "", "workspace ID (default discovered from Deployment)")
	fs.StringVar(&o.thread, "thread", "", "resume this Thread instead of creating a new one")
	fs.DurationVar(&o.timeout, "timeout", o.timeout, "timeout for one API call")
	fs.DurationVar(&o.wait, "wait-timeout", o.wait, "maximum time to watch one input")
	// Accept kubectl-style flags both before and after the resource name.
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "h" || name == "help" {
			fs.Usage()
			return nil
		}
		if fs.Lookup(name) == nil {
			return fmt.Errorf("unknown option %s", arg)
		}
		if !hasValue {
			i++
			if i == len(args) {
				return fmt.Errorf("option %s requires a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "help" {
		fs.Usage()
		return nil
	}
	if len(positional) < 2 {
		return errors.New("an AgentDeployment name is required")
	}
	command, name := positional[0], positional[1]
	if command != "chat" && command != "resume" && command != "threads" && command != "recover" {
		return fmt.Errorf("unknown command %q", command)
	}
	if len(positional) > 3 || (len(positional) == 3 && command != "resume" && command != "recover") {
		return errors.New("unexpected positional arguments")
	}
	if command == "recover" && len(positional) != 3 {
		return errors.New("recover requires the original request key")
	}
	if command == "resume" && len(positional) == 3 {
		o.thread = positional[2]
	}
	if o.timeout <= 0 || o.wait <= 0 {
		return errors.New("timeouts must be positive")
	}
	connection, err := connectAgent(ctx, name, o)
	if err != nil {
		return err
	}
	defer connection.close()
	c := &chat{connection: connection, console: newConsole(), wait: o.wait}
	if command == "threads" {
		_, err := c.list(ctx, "")
		return err
	}
	if command == "recover" {
		return c.recover(ctx, positional[2])
	}
	fmt.Fprintf(c.out(), "agentctl · %s · workspace %s\n", name, connection.resource.WorkspaceId)
	fmt.Fprintln(c.out(), "Type a message or /help. Ctrl-D exits; Ctrl-C while waiting detaches.")
	fmt.Fprintln(c.out())
	if command == "resume" || o.thread != "" {
		if err := c.resume(ctx, o.thread); err != nil {
			return err
		}
	}
	return c.loop(ctx)
}
