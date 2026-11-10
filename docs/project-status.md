# Project status

Status recorded on **2026-11-10**. a8s is an experimental implementation. This
page distinguishes implemented interfaces from runtime behavior actually
verified with the current native Codex path.

## Supported path

| Area | Current behavior |
| --- | --- |
| Workload | `AgentDeployment`; one persistent workspace per Deployment |
| Runtime | Built-in `codex-node`, upstream Codex 0.166.0 and Node 22.18.0, Linux/amd64 |
| Content | Agentfile image containing instructions, complete Skills, CLI dependencies, MCP configuration |
| Registry | Fixed OCI content/runtime references and verified metadata packages |
| Control | PostgreSQL resource persistence, immutable Releases, Operations, workspace ownership |
| Execution | Stable request keys, original-binding routing, result lookup, typed interactions, live events |
| Guest | Root supervisor, unprivileged UID worker, workspace lease, process census |
| Idle reclamation | Retain Pod/storage/binding; gracefully stop runtime processes; wake on demand |
| Clients | CLI, Go SDK, and source TypeScript SDK |

## Recorded manual verification

The implementation was exercised in an existing dedicated k3s namespace with
PostgreSQL, an OCI registry, upstream Codex binaries, and a real configured model.
The runs used retained validation Deployments and existing Pods, not temporary
benchmark Pods.

Observed scenarios included:

- Agent content loading, registration, Pod preparation, and actual model/tool execution.
- Concurrent conversations sharing a workspace with independent conversation context.
- UID filesystem boundaries and rejection of stale workspace generations.
- Preservation of active Turns and detached background OS processes.
- Retention of an empty Thread before its first input becomes persistent.
- Graceful idle parking with normal native exit, history sync, and no remaining workspace-UID processes.
- Passive event-stream release and completed-result polling without reawakening the harness.
- Same-binding wake and restoration of the original native conversation's random context value.
- Reuse of the original native dispatch receipt without repeating the tool action.
- Rejection of a reused request key carrying a different payload.

One final small-Agent run used a **10-second idle timeout** and **1-second check
interval** to shorten validation. It observed:

| Measurement | One recorded run |
| --- | --- |
| Worker/native runtime wake | 296 ms |
| Input acceptance after wake | 529 ms |
| Wake through complete short model reply | 3.49 s |
| Suspended execution Pod working set | 6.89 MiB |

These values are environment-specific observations, not service guarantees or
portable benchmarks. They exclude the separate control-plane Pod. Model/network
latency, image cache state, content, and loaded Threads affect results. Pod
requests and limits remain allocated after process reclamation.

The opt-in scripts in `scripts/guest-*` reproduce selected checks against
explicitly prepared validation resources. Local raw reports, credentials,
cluster manifests, and experimental plans are excluded from Git.

## Important limits

**General updates and removal remain guarded.** Control has update, rollback,
and offline state machines, but arbitrary running-instance drain, history/schema
handoff, and durable stop-result reconciliation are not fully verified. The
convenience workflow refuses replacement and active-Agent cleanup. In-place idle
park/wake has a narrower proof boundary and is supported.

**Unknown activity keeps a runtime running.** Persistent MCP processes and empty
Threads without a first input can prevent reclamation. Failed native observation
or an unresolved dispatch journal is never treated as idle.

**There is no infrastructure checkpoint or automatic failover.** A sleeping
workspace retains its existing Pod. Node loss, force deletion, and unknown
storage writers do not authorize takeover. Cross-instance native Thread recovery
and durable event replay are not available.

**The installer is still a development workflow.** It provisions a per-Agent API
server from a source checkout. There is no hosted public image catalog, released
Helm chart, production multi-tenant control-plane installer, or published npm SDK.
The registry convenience path does not yet assemble private-registry credentials
for the Guest loader/node image pulls.

**Security policy belongs to the deployment.** The current UID/process checks do
not constitute hostile multi-tenant isolation. Egress restrictions and network
proxy/allowlist infrastructure are not installed by this workflow.

**Checks have a defined scope.** `make check` runs vet and package test commands;
most packages currently have no permanent unit tests. `buf lint` and TypeScript
checks validate contracts/types. The manual integration scenarios are not a CI
acceptance suite and do not establish production readiness.
