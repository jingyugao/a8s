# Operator guide

The current installation workflow is a source-checkout workflow. It provisions
one API server per Agent and one execution Pod/PVC per workspace. It is not a
Helm release, a Kubernetes operator, or an installation of an `AgentDeployment`
CRD.

## Prerequisites

Provide:

- A Linux/amd64 Kubernetes cluster, kubectl context, and suitable StorageClass.
- PostgreSQL with a dedicated application database and account.
- An OCI registry reachable by the workstation and cluster nodes.
- A model endpoint compatible with the pinned Codex runtime.
- Docker, uv/Python 3.11+, Bash, curl, jq, tar, and sha256sum on the build host.
- Go 1.26+ to build the platform binaries.

The stock Codex distribution and Node version are locked in
[images/codex/lock.json](../images/codex/lock.json) and the
[runtime class](../images/runtime-classes/codex-node/runtime-class.json).
The supplied runtime currently targets Linux/amd64.

## 1. Configure the platform

[deploy/k3s/platform.env](../deploy/k3s/platform.env) contains portable
placeholder defaults. Create an ignored `deploy/k3s/platform.local.env` for your
installation. For example:

```dotenv
A8S_PLATFORM_CONTEXT=my-cluster
A8S_PLATFORM_NAMESPACE=a8s
A8S_PLATFORM_TENANT=default
A8S_PLATFORM_STORAGE_CLASS=local-path
A8S_PLATFORM_REGISTRY_PLAIN_HTTP=false
A8S_PLATFORM_PACKAGE_REPOSITORY=registry.example.com/a8s-package
```

Use your actual context, storage class, and registry. Export the same
`A8S_PLATFORM_*` variables in the shell when using the commands below. Environment
variables override the local file, which overrides the checked-in defaults.
These files are parsed as configuration; the script does not source shell code.

The package repository stores OCI metadata/content packages. It is separate from
the runtime/Agent image repository and must be writable by the publisher.

```bash
kubectl --context "$A8S_PLATFORM_CONTEXT" \
  create namespace "$A8S_PLATFORM_NAMESPACE"
```

The namespace should be dedicated to this installation. The generated template
creates namespace-scoped Pod/Service permissions and a ClusterRole limited to
reading Nodes for stop-evidence checks. The installing identity needs permission
to create those RBAC objects.

## 2. Initialize PostgreSQL explicitly

Have the database administrator provision the database/account and apply both
schemas once to a fresh database:

```bash
psql "$A8S_DATABASE_URL" -v ON_ERROR_STOP=1 \
  -f storage/control_schema.sql \
  -f storage/execution.sql
```

The Execution schema is not an idempotent migration runner. Do not repeat it
against an initialized database. The server checks the schemas at startup and
never runs DDL automatically. a8s does not start a database container.

## 3. Create the platform secrets

Create an operator-local `.env.a8s-server` containing:

```dotenv
A8S_TOKEN=<a random API token of at least 16 characters>
A8S_DATABASE_URL=<the PostgreSQL connection string>
```

Create `.env.a8s-model` containing the model credential:

```dotenv
OPENAI_API_KEY=<the credential for your configured model endpoint>
```

Restrict these files to the operator and create the Secrets in the selected
namespace:

```bash
chmod 600 .env.a8s-server .env.a8s-model
kubectl --context "$A8S_PLATFORM_CONTEXT" -n "$A8S_PLATFORM_NAMESPACE" \
  create secret generic a8s-server --from-env-file=.env.a8s-server
kubectl --context "$A8S_PLATFORM_CONTEXT" -n "$A8S_PLATFORM_NAMESPACE" \
  create secret generic a8s-model --from-env-file=.env.a8s-model
```

These are initial-setup commands. Manage later secret changes through your
normal infrastructure workflow. `COLONY_MODEL_API_KEY` remains supported for
existing integrations; set the provider's `env_key` to the variable you use.
The Guest forwards only the configured model-key allowlist and runtime values
to the unprivileged worker. It does not forward the database connection or API
token.

## 4. Configure model access

Create a local TOML file with the configuration accepted by the pinned upstream
Codex version. This template requires replacing the endpoint and model:

```toml
approval_policy = "never"
sandbox_mode = "danger-full-access"
model_provider = "a8s"
model = "your-codex-compatible-model"

[model_providers.a8s]
name = "Configured model provider"
base_url = "https://api.example.com/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
```

Create the host ConfigMap:

```bash
kubectl --context "$A8S_PLATFORM_CONTEXT" -n "$A8S_PLATFORM_NAMESPACE" \
  create configmap a8s-host-config --from-file=config.toml=./model-config.toml
```

The model configuration belongs to the platform. Agent-owned
`.codex/config.toml` files may declare `mcp_servers` only. The management script
merges those entries into an Agent-specific host ConfigMap.

The template grants the Agent shell access within its unprivileged workspace
identity. The deployment does not provide a hostile multi-tenant sandbox or
configure an egress proxy/allowlist; those are infrastructure responsibilities.

## 5. Publish the built-in runtime

Choose the image repository for this checkout. Local source builds use one
reused `:dev` tag; runtime and content references are fixed by digest at deploy
time.

```bash
make image-publish IMAGE=registry.example.com/a8s:dev
```

This builds the Go binaries, verifies and installs the original Codex archive,
assembles Codex + Node + Guest, verifies the resulting harness binaries, and
pushes the image. It records the resulting digest in the ignored
`images/runtime-classes/codex-node/runtime-class.local.json`.
The checked-in catalog starts without a published image, so a fresh checkout
cannot accidentally depend on a developer's private registry.

The Dockerfile is platform-owned. Agent authors select `runtimeClassRef:
codex-node`; they do not choose or rebuild the runtime image.

For a private registry, configure both node image pulls and registry access from
the publisher/loader. The convenience management script does not yet project
private-registry credentials into the Guest loader or configure imagePullSecrets.
Use a registry reachable without credentials for the initial workflow, or supply
that infrastructure integration separately.

## 6. Create an Agent

Set the example's `agentImage` to your writable development repository, then:

```bash
make -C examples/frontend-assistant create
make -C examples/frontend-assistant status
```

The management script renders
[deploy/k3s/agent-instance.yaml](../deploy/k3s/agent-instance.yaml), creates
its workspace PVC and per-Agent API server, publishes metadata, and registers
the Deployment through the Control API. Control creates the dedicated execution
Pod from the fixed runtime digest. This is the supported convenience path; local
validation manifests are not a second installation path.

Local plans, request keys, and resolved references remain under `tmp/agents/`.
Preserve them when investigating an uncertain create or registry result.

## Direct server configuration

For integrations assembling their own control-plane deployment:

| Variable | Purpose |
| --- | --- |
| `A8S_DATABASE_URL` | PostgreSQL database containing both schemas |
| `A8S_TOKEN` / `A8S_TOKEN_FILE` | API bearer token; the file takes precedence |
| `A8S_TENANT`, `A8S_PROJECT` | Scope served by this process |
| `A8S_KUBERNETES_NAMESPACE` | Namespace for the native provider |
| `A8S_WORKSPACE_DIRECTORY` | Mounted workspace root |
| `A8S_AGENTC_URL` | Agentc/Guest endpoint; supports `{instance}` |
| `A8S_AGENTC_TOKEN` | Optional Agentc token; defaults to the API token |
| `A8S_SANDBOX_LAUNCH` | Platform-generated JSON command/environment specification |
| `A8S_REGISTRY_PLAIN_HTTP` | Explicit HTTP-registry option; defaults to false |
| `A8S_REGISTRY_HOST` | Host associated with optional registry credentials |
| `A8S_REGISTRY_USERNAME`, `A8S_REGISTRY_PASSWORD`, `A8S_REGISTRY_TOKEN` | Registry client credentials |

The convenience script assembles these values. `a8s-server` exposes `/healthz`
and `/readyz`; those checks do not imply every lifecycle capability is supported.
