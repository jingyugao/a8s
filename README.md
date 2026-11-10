# a8s

Deploy agents from versioned content images, run them in persistent Kubernetes
workspaces, and reclaim idle runtime processes without losing conversations.

a8s separates **what an agent contains** from **how it runs**. Agent authors
provide instructions, Skills, CLI tools, and MCP configuration. The platform
supplies the harness, runtime image, storage, and execution routing.

The current implementation supports `AgentDeployment` with upstream Codex and a
built-in `codex-node` runtime class. It is an experimental implementation with
working create, conversation, and idle-reclamation paths. General replacement,
rollback, and active-agent removal remain guarded; see [project status](docs/project-status.md).

## What you deploy

```text
examples/frontend-assistant/
├── Agentfile                 # Build the Agent content image
├── Makefile                  # Build, publish, create, and inspect
├── deploy/
│   └── agent.yaml            # Register the AgentDeployment
└── src/
    ├── AGENTS.md             # Agent instructions
    ├── .agents/skills/       # Skills and their supporting files
    ├── .codex/config.toml    # MCP servers
    ├── bin/                 # Agent-owned CLI tools
    └── package*.json         # Locked remote CLI dependencies
```

```yaml
apiVersion: a8s/v1
kind: AgentDeployment
name: frontend-assistant
agentImage: registry.example.com/a8s:dev
runtimeClassRef: codex-node
workingDir: /runtime
resources:
  cpu: "1"
  memory: 1Gi
update:
  strategy: restart
idleReclaim:
  enabled: true
  idleTimeout: 10m
  checkInterval: 1m
```

`agent.yaml` is an a8s manifest, consumed by the management script. It is not a
Kubernetes CRD and cannot be installed directly with `kubectl apply`.

## Get started

For a configured platform, choose a writable registry in `agent.yaml`, then run:

```bash
make -C examples/frontend-assistant create
make -C examples/frontend-assistant status
```

The example installs the pinned `frontend-design` Skill and the locked `prettier`
CLI. It also includes a local environment-check Skill and an example MCP server.
Agent authors need Docker, kubectl, and uv; the platform provides Codex and Node.

A fresh installation first needs [operator setup](docs/operator-guide.md).
There is no bundled database or publicly hosted runtime image. Operators publish
the runtime once; deployments use its fixed digest.

To work on the implementation locally:

```bash
make build
make check
```

Go 1.26 or later is required. See [Contributing](CONTRIBUTING.md) for TypeScript
SDK checks and code generation.

## Runtime model

A workspace has one dedicated execution Pod. Multiple conversations in that
workspace share its native Codex state and files. Execution requests follow the
persisted workspace-to-instance binding.

The Pod contains a small root Guest supervisor. It starts the Agentc adapter,
upstream Codex app-server, and exec-server under a dedicated unprivileged UID.
Idle reclamation stops these runtime processes while retaining the Pod, binding,
PVC, history, and request receipts. New input wakes the same logical instance.

Active turns, queued inputs, pending interactions, background processes, or
unknown observations prevent reclamation. Completed-result polls use a local
cache, and passive event subscriptions do not keep a runtime active. Persistent
MCP processes conservatively keep it running. Pod resource requests are unchanged
by process reclamation.

## Documentation

| Guide | Audience |
| --- | --- |
| [Getting started](docs/getting-started.md) | Build an Agent and start a conversation |
| [Operator guide](docs/operator-guide.md) | Configure storage, model access, runtime images, and Kubernetes |
| [Agent manifest](docs/agent-manifest.md) | Agentfile, source layout, YAML fields, and artifacts |
| [Architecture](docs/architecture.md) | Module boundaries, ownership, and execution topology |
| [Idle reclamation](docs/idle-reclamation.md) | Activity checks, graceful parking, and wake-up behavior |
| [API and SDKs](docs/api-and-sdk.md) | CLI, Go, TypeScript, request keys, and uncertain outcomes |
| [Project status](docs/project-status.md) | Verified behavior and current limitations |
| [Contributing](CONTRIBUTING.md) | Build and verification workflow |

## License

a8s is available under the [MIT License](LICENSE). Third-party dependencies,
runtime binaries, and remote Skills retain their own licenses. See
[third-party components](docs/third-party.md); the Agentfile preserves the
upstream Skill's license file.
