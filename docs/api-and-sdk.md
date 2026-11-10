# API, CLI, and SDKs

The source contracts are:

- [Control API](../api/proto/a8s/v1/control.proto)
- [Execution API](../execution/proto/a8s/execution/v1/execution.proto)
- [Agentc contract](../agentc/contract/proto/a8s/agentc/v1/agentc.proto)

Go and TypeScript bindings are checked in. The CLI and SDKs use Connect. The
current server accepts a bearer token and serves its configured tenant/project
scope; applications own any broader user/organization authorization model.

## CLI

Build the local binaries with `make binaries`, then inspect the available flags:

```bash
dist/a8s --help
dist/a8s deployment create --help
dist/a8s execution submit --help
```

Connection defaults come from `A8S_ENDPOINT`, `A8S_TENANT`, and `A8S_PROJECT`.
`A8S_TOKEN` supplies the bearer token. Use `--json` for scripts.

| Group | Commands |
| --- | --- |
| `package` | `init`, `validate`, `inspect`, `build`, `publish`, `resolve`, `fetch` |
| `agentc` | `register`, `get` |
| `runtime` | `create`, `get` |
| `deployment` | `create`, `get`, `list`, `update`, `online`, `offline`, `rollback` |
| `operation` | `get`, `wait` |
| `workspace`, `release`, `target` | `get` |
| `execution` | `open-thread`, `submit`, `get`, `lookup`, `cancel`, `respond`, `events` |

Low-level mutation bodies use protobuf JSON through `--file request.json`, or
`--file -` for stdin. Scope and stable request keys must be supplied by the caller.
Lifecycle APIs existing in this table do not imply every runtime can safely
complete them; see [project status](project-status.md).

For the Agentfile workflow, use the example Makefile. The
[getting-started guide](getting-started.md) shows actual conversation commands.

## Go SDK

Use the source module `github.com/jingyugao/a8s/sdk`. The SDK receives connection
settings and caller-owned authentication headers; it does not create credentials.

```go
package main

import (
    "context"
    "log"
    "net/http"
    "os"

    "connectrpc.com/connect"
    v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
    "github.com/jingyugao/a8s/sdk"
)

func main() {
    headers := make(http.Header)
    headers.Set("Authorization", "Bearer "+os.Getenv("A8S_TOKEN"))
    client, err := sdk.New(os.Getenv("A8S_ENDPOINT"), sdk.Options{Headers: headers})
    if err != nil {
        log.Fatal(err)
    }
    response, err := client.Control.GetDeployment(context.Background(),
        connect.NewRequest(&v1.GetDeploymentRequest{
            Scope: &v1.Scope{
                TenantId: os.Getenv("A8S_TENANT"),
                ProjectId: os.Getenv("A8S_PROJECT"),
            },
            Id: "frontend-assistant",
        }))
    if err != nil {
        log.Fatal(err)
    }
    log.Print(response.Msg.Deployment.GetId())
}
```

The client exposes generated `Control` and `Execution` clients. `WaitOperation`
returns the most recently observed operation when a bounded wait expires or the
operation becomes failed, blocked, or unknown. Ending the wait does not cancel
the server operation.

## TypeScript SDK

The TypeScript SDK is source-only at present; it is not a published npm release.
Build it locally:

```bash
pnpm --dir sdk/ts install --frozen-lockfile
pnpm --dir sdk/ts build
```

From the repository root, use its built Node entrypoint:

```js
import { createNodeA8sClient } from "./sdk/ts/dist/node.js";

const client = createNodeA8sClient(process.env.A8S_ENDPOINT, {
  headers: { Authorization: `Bearer ${process.env.A8S_TOKEN}` },
});
const response = await client.control.getDeployment({
  scope: {
    tenantId: process.env.A8S_TENANT,
    projectId: process.env.A8S_PROJECT,
  },
  id: "frontend-assistant",
});
console.log(response.deployment?.id);
```

A consuming workspace can link the package as `@a8s/sdk`, using `@a8s/sdk/node`
for the Node transport helper. Browser applications can supply an appropriate
Connect transport to `createA8sClient`; credentials and transport policy remain
application-owned.

## Request keys and unknown outcomes

Choose a stable key for each intended mutation and persist it before sending.
Reusing it with a different payload is a conflict. Gateway also assigns a request
ID; save that ID and query the original input rather than redispatching it.

A request deadline, connection loss, or missing native receipt can produce
`OutcomeUnknown`. It does not prove rejection or non-execution. The SDK preserves
request/operation identity and structured error metadata and does not
unconditionally retry mutations.

Use `execution lookup` with the original request kind/key or `operation get`
with the recorded operation ID. The result is reconciled against the original
binding, not a newly healthy instance. Correct a confirmed pre-execution
rejection using the same request key; do not invent another key to bypass an
uncertain result.

Native receipt lookup requires positive evidence. A newly submitted input can
briefly be unresolvable before native history is visible. Continue querying its
original ID, with a bounded wait, instead of sending it again.

## Live events and approvals

Execution supports live subscriptions and typed approval responses. Event
streams have no durable replay cursor. A subscription can end when the runtime
parks or a connection fails; query the final input state after reconnecting.

Typed interaction decisions currently support `accept` and `decline`. Do not
assume cancellation acknowledgement proves remote tool processes have exited;
workspace release requires the separate lifecycle/process evidence.
