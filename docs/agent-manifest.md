# Agent content and manifest reference

An Agent consists of an Agentfile, source content, one deployment manifest, and
optional build commands. The platform owns the runtime Dockerfile.

## Agentfile and source layout

In this implementation, `Agentfile` is a Docker-compatible build recipe consumed
with `docker build -f Agentfile`. It assembles `/agent` in the final image. It is
not a separate declarative instruction language.

The [frontend example](../examples/frontend-assistant/Agentfile) uses a
build stage to install locked npm dependencies and a pinned remote Skill, then
copies only `/agent` into a scratch image. The final content image does not need
an entrypoint, Codex, Node, or the platform binaries.

| Source | Purpose |
| --- | --- |
| `src/AGENTS.md` | Agent instructions; required in the built `/agent` directory |
| `src/.agents/skills/<name>/` | A Skill's `SKILL.md`, scripts, and referenced assets |
| `src/.codex/config.toml` | Native MCP configuration; only `mcp_servers` entries |
| `src/bin/` | Example-owned CLI files copied to `/agent/node_modules/.bin` |
| `src/package.json`, `src/package-lock.json` | Locked CLI dependencies installed during the build |

Install complete Skill directories, including referenced files and licenses.
Pin remote content to an immutable revision. Agent code can use the runtime's
`AGENT_ROOT=/agent` and the CLI directory on `PATH`.

The example's `agent-tools` command also runs as a small read-only MCP stdio
server. `prettier` is the remote CLI dependency. The platform merges MCP settings
with its model configuration and assigns the remote execution environment.
The declared `workingDir` is a mount root; the actual shell working directory is
the Guest's private workspace directory beneath that root.

## AgentDeployment YAML

The current YAML format has these top-level fields:

| Field | Meaning |
| --- | --- |
| `apiVersion` | Exactly `a8s/v1` |
| `kind` | Exactly `AgentDeployment` |
| `name` | Lowercase letter followed by lowercase letters, digits, or hyphens; up to 40 characters |
| `agentImage` | Agent content image reference; source build/publish uses a reused `:dev` tag |
| `runtimeClassRef` | Built-in runtime class name, optionally pinned with `@sha256:...` |
| `workingDir` | Absolute workspace mount root; defaults to `/runtime` |
| `resources.cpu` | Positive CPU quantity such as `"1"` or `"500m"` |
| `resources.memory` | Positive memory quantity such as `512Mi` or `1Gi` |
| `update.strategy` | `restart`; declaring it does not enable general replacement yet |
| `idleReclaim.enabled` | Enable process reclamation; defaults to `false` |
| `idleReclaim.idleTimeout` | Positive duration; defaults to `10m` |
| `idleReclaim.checkInterval` | Positive duration; defaults to `1m` |

Unknown fields are rejected. A nonempty top-level `env` mapping is currently
rejected. Put nonsecret content defaults in the Agentfile and let the platform
supply credentials. Other workload kinds, including `AgentJob`, are unsupported.

The resource values become Pod limits. Kubernetes may also default requests
from those limits; process reclamation does not reduce the reserved requests.

## What is published

There are three related artifacts:

1. **Agent content image:** the OCI image built from the Agentfile, containing
   `/agent` with instructions, Skills, CLI dependencies, and MCP configuration.
2. **Agent metadata package:** a verified `a8s.package/v1` OCI artifact containing
   explicitly selected instructions/Skill files, package metadata, and digests.
   The convenience publisher generates and publishes it for Control.
3. **Runtime image:** the operator-built `codex-node` image containing upstream
   Codex, Node, Guest, Agentc, the exec-server, and the loader.

Control snapshots fixed package/runtime references into a Release. The Guest
loader pulls the fixed Agent content image and verifies its generated package
against the registered digest before admitting a workspace. Content is
materialized read-only; writable history, receipts, cache, and work files live
on the workspace volume.

The lower-level package CLI also supports `a8s-package.json`, local OCI layouts,
and verified materialization. Its declared CLI/runtime dependency support is
more limited than placing actual CLI files in the Agent content image. The
convenience Agentfile workflow does not require authors to hand-write that
metadata manifest.

## Commands

From `examples/frontend-assistant`:

| Command | Behavior |
| --- | --- |
| `make build` | Build Agent content; does not create an Agent |
| `make publish` | Build and push content, then publish its metadata package |
| `make plan` | Resolve published artifacts and write an unapplied local plan |
| `make create` | Publish and register; wait for the lifecycle operation |
| `make status` | Query the Deployment and its operation |
| `make update` | Currently refuses; replacement remains guarded |
| `make delete` | Preserve active Agents; clean supporting services only after verified offline state |

Use a new immutable image/package digest when content changes. Do not edit
runtime content in a live Pod or move an uncertain request to a new instance.
