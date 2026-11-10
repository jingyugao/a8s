# Getting started

This guide assumes an operator has completed [platform setup](operator-guide.md).
The platform must have a published `codex-node` runtime, model configuration,
PostgreSQL schemas, and permission to create execution Pods and PVCs.

## 1. Prepare the example

Use [examples/frontend-assistant](../examples/frontend-assistant/). Keep
its `Agentfile`, `Makefile`, `src/`, and `deploy/agent.yaml` together. Set
`agentImage` to a repository you can push:

```yaml
agentImage: registry.example.com/a8s:dev
```

Replace `registry.example.com` with your registry. The repository defaults are
placeholders, not hosted services. Runtime publication and Agent content builds
reuse this repository's development tag; running deployments retain fixed
references and are not changed by overwriting the tag.

The example contains:

- `src/AGENTS.md`: instructions for the assistant.
- A pinned remote `frontend-design` Skill, installed by the Agentfile with its license.
- A local `runtime-check` Skill, including its script.
- A locked `prettier` dependency and the `agent-tools` CLI/MCP example.

Codex and Node come from the platform runtime. Do not add either to the Agent
content image.

## 2. Build, publish, and create

```bash
make -C examples/frontend-assistant build
make -C examples/frontend-assistant publish
make -C examples/frontend-assistant plan
make -C examples/frontend-assistant create
make -C examples/frontend-assistant status
```

`publish` already builds content, and `create` already publishes it. The separate
commands above show the stages; a normal first deployment only needs `make create`.
`plan` resolves registry content and writes local files, but does not apply
Kubernetes resources or create a Deployment in the Control API.

Plans and request files are stored under `tmp/agents/frontend-assistant/`. The
create path provisions a per-Agent API server and a workspace PVC, registers
immutable runtime/package revisions, and lets Control provision the execution
Pod. A repeated create with unchanged content returns the existing Deployment.
Changed content is rejected until safe replacement is available.

## 3. Connect the CLI

In a separate terminal, forward the Agent's API service:

```bash
kubectl --context "$A8S_PLATFORM_CONTEXT" \
  -n "$A8S_PLATFORM_NAMESPACE" port-forward --address=127.0.0.1 \
  service/agent-frontend-assistant-a8s-server 8090:8090
```

Configure the CLI in your working terminal. Load `A8S_TOKEN` from the operator's
existing secret source; do not put it in the Agent manifest.

```bash
export A8S_ENDPOINT=http://127.0.0.1:8090
export A8S_TENANT="$A8S_PLATFORM_TENANT"
export A8S_PROJECT=agent-frontend-assistant
```

A local source checkout can use `dist/a8s` after `make binaries`. The management
script can instead extract the CLI from the published runtime image when the
binary is absent.

## 4. Start a conversation

Persist the request keys you choose. Reuse a key only for the same logical action
and identical payload.

```bash
dist/a8s --json execution open-thread \
  --deployment frontend-assistant \
  --workspace frontend-assistant-workspace \
  --request-key intro-thread-v1
```

Save the returned `record.threadId` as `THREAD_ID`, then submit input:

```bash
dist/a8s --json execution submit \
  --deployment frontend-assistant \
  --workspace frontend-assistant-workspace \
  --thread "$THREAD_ID" \
  --request-key intro-input-v1 \
  --text 'Use the runtime-check Skill and report the environment.'
```

Save `record.requestId` as `INPUT_REQUEST_ID`. Query that original input until it
reaches a terminal state:

```bash
dist/a8s --json execution get \
  --deployment frontend-assistant \
  --workspace frontend-assistant-workspace \
  --thread "$THREAD_ID" \
  --id "$INPUT_REQUEST_ID"
```

For live events, use the same resource and input ID:

```bash
dist/a8s --json execution events \
  --deployment frontend-assistant \
  --workspace frontend-assistant-workspace \
  --thread "$THREAD_ID" \
  --id "$INPUT_REQUEST_ID"
```

Events are live notifications, not a durable transcript. After a disconnect,
query the original input result. An early `OutcomeUnknown` is not permission to
send the input again under a new key.

## 5. Understand idle behavior and lifecycle limits

The example requests a check every minute after ten minutes of inactivity.
Background tasks or the example's persistent MCP processes keep the runtime
running. See [idle reclamation](idle-reclamation.md) for the exact conditions.

`make update` currently refuses replacement. For an active Agent, `make delete`
saves an offline request and refuses destructive cleanup. These guards preserve
workspace and history ownership while general safe handoff remains unverified.
An already verified offline Agent can release its supporting services while
retaining data. Do not use raw Pod/PVC deletion as the normal lifecycle workflow.
