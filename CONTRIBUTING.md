# Contributing to a8s

The API and runtime behavior are still evolving. Read the
[architecture](docs/architecture.md) and
[project status](docs/project-status.md) before changing lifecycle or
workspace ownership behavior.

## Build and check

Use Go 1.26 or later:

```bash
make build
make check
buf lint
```

`make check` runs `go vet` and `go test` over the source packages. Most packages
currently have no checked-in unit tests; this command alone does not validate
Kubernetes, PostgreSQL, registry, or model integrations.

The TypeScript SDK requires Node 22 or later and pnpm:

```bash
pnpm --dir sdk/ts install --frozen-lockfile
pnpm --dir sdk/ts check
pnpm --dir sdk/ts build
```

Manage Python tooling with uv. The management script declares its dependencies
in its inline script metadata:

```bash
bash scripts/agent.sh --help
```

## Generated contracts

Protocol definitions live in `api/proto`, `agentc/contract/proto`, and
`execution/proto`. Commit generated Go and TypeScript bindings with their source
changes. With Buf and the TypeScript dependencies installed:

```bash
make gen
pnpm --dir sdk/ts generate
pnpm --dir sdk/ts check
```

Buf's Go plugins are pinned in `buf.gen.yaml`. TypeScript generation uses the
pinned local generator from the SDK dependencies.

## Runtime images

```bash
make image-publish IMAGE=registry.example.com/a8s:dev
```

Replace the registry with one you control. Reuse the `:dev` tag for local source
builds and deploy fixed digests. The runtime installer verifies the complete
upstream archive and the locked Codex binaries. Do not replace them with a fork
or bundle a second harness in an Agent content image.

See the [operator guide](docs/operator-guide.md) for installation.
Building or publishing an image does not deploy it to Kubernetes.

## Review boundaries

Keep changes focused on one behavior. Use public package contracts and inject
infrastructure clients and credentials at the application boundary. Preserve
request IDs, operation IDs, original bindings, and unknown-outcome semantics.

A missing Pod or an expired request is not evidence that an old writer has
stopped. Do not add automatic redispatch or ownership handoff without the
corresponding execution and storage evidence.

Add focused tests for meaningful invariants and failures. Manual smoke scripts
under `scripts/guest-*` require explicitly selected existing validation resources;
they may start real model requests and some stop or restart their workspace.
They are not part of the default check command.

Keep credentials, local cluster manifests, generated plans, build outputs, and
runtime data out of Git. `deploy/k3s/platform.local.env` and
`images/runtime-classes/*/runtime-class.local.json` hold operator-local settings.
