# Agent-to-Agent (A2A)

> Deep dive. Diagram also on the [root README](../README.md#architecture); protocol path in [architecture.md](architecture.md#agent-protocol-vs-runner-protocol).

An agent calls another agent as a sub-task, mid-execution -- native sub-agent delegation via the same Agent Protocol API. The mechanism is deliberately **not** a new protocol surface -- it's the exact same `POST /threads/{id}/runs` + wait-for-result path any client already uses, just reachable from inside a runner's own process via one new internal route (`POST /internal/a2a/runs`) instead of a public one.

```mermaid
sequenceDiagram
  participant Coord as Coordinator agent
  participant CP as Control plane
  participant Work as Worker agent

  Note over Coord,CP: Parent run already executing on a runner
  Coord->>CP: call_agent / callAgent (internal A2A)
  CP->>CP: depth check · parent/root bookkeeping
  CP->>Work: enqueue child run (Runner Protocol)
  Work-->>CP: result
  CP-->>Coord: child output (wait=True)
```

**Python SDK**: `call_agent` (`python/runkite_runner/a2a.py`) is what a node calls, using the exact `config` LangGraph already passes it -- everything needed (the calling run's own `run_id`, the authenticated user to forward) is already there:

```python
from runkite_runner.a2a import call_agent

async def coordinator_node(state, config: RunnableConfig) -> dict:
    result = await call_agent(config, "worker_agent", {"messages": [...]}, wait=True)
    ...
```

**TypeScript SDK**: `callAgent` (`typescript/runkite-runner/src/a2a.ts`) is a direct port -- same request shape, same `config.configurable.run_id`/`langgraph_auth_user` forwarding. The runner's `executeRun.ts` now builds this via an exported `buildRunConfig`, which sets the same `configurable` keys Python's `build_run_config` does (`assistant_id`/`graph_id`/`langgraph_auth_user`/`user_id`/`user_display_name`, not just `thread_id`/`run_id` as before) -- one deliberate difference: the TypeScript version builds a fresh config object rather than mutating `assignment.config` in place, which Python's does (harmless either way; nothing compares object identity):

```typescript
import { callAgent } from "runkite-runner";

async function coordinatorNode(state: State, config: RunnableConfig) {
  const result = await callAgent(config, "worker_agent", { messages: [...] }, { wait: true });
  ...
```

See `examples/a2a_agent/` for a complete working example (`coordinator_agent` delegates to `worker_agent`).

Three things this adds on top of the shared run-creation/wait path:

- **Parent-bound authority**: `/internal/a2a/runs` requires the active parent run ID and exact assignment generation. The body `parent_run_id` must match that binding. The plane derives tenant and principal from the assignment and checks any frozen parent principal. Omit `on_behalf_of` to inherit. A supplied identity or claim must match the trusted parent; permissions may only narrow its existing grants. An empty list cannot erase restrictions, and null is rejected. Python and TypeScript helpers send the binding and reject mismatched execution context.
- **Recursion limits**: every sub-run's `depth` is enforced against `a2a.max_depth` (default 10) at creation time -- an accidental cycle or runaway delegation chain fails fast with `400`, not a resource leak. Direct children per parent are capped by `a2a.max_breadth` (default 20) so a buggy coordinator cannot fork-bomb the queue in one hop. Configurable:
  ```json
  { "a2a": { "max_depth": 10, "max_breadth": 20 } }
  ```
  `POST /runs/search` accepts `parent_run_id` to list those direct children (same filter the admission check uses).
- **Cost attribution**: every *delegated* run carries `parent_run_id` and `root_run_id` (the top of the chain), persisted and indexed. `RunSearchRequest` exposes `root_run_id` as a client-facing filter (`POST /runs/search`) -- pass the tree's root `run_id` (or any descendant's own `root_run_id` value, which is the same thing) to list every OTHER run in the tree with one query. The root itself is never returned this way (its own `root_run_id` is nil by design; fetch it separately by ID), and this filtered search is subject to the same `maxSearchLimit` (100) clamp as any other client-facing search. `GET /runs/{runID}/cost` (below) is more permissive: pass ANY run's ID in the tree -- root or descendant -- and it resolves to the same root internally before aggregating, no client-side root-finding required.
- **Cancel cascade**: cancelling a run cancels everything it delegated to, directly or transitively (not ancestors or siblings) -- a cancelled parent can't leave orphaned children still executing.
- **Admin visibility**: `GET /admin-api/runs` accepts the same `parent_run_id`/`root_run_id` filters as the client-facing search, and the Admin UI's Run Detail page renders an "Agent-to-agent" panel from them -- a parent link (if this run was delegated) plus a table of direct children (if it delegated to any), with no extra query needed beyond the run's own ID.

**Cost aggregation** (`GET /runs/{runID}/cost`) is deliberately convention-based, not a Runner Protocol change: nothing requires a runner to report token usage today, so this reads whatever a run's own `output` JSON already contains. If it happens to include a top-level `usage` object shaped like most LLM APIs already return it (`prompt_tokens`/`completion_tokens`/`total_tokens`, plus an optional `cost_usd`), it's summed across every run in the tree; a run with no such object just contributes zero. `total_tokens` is filled in as `prompt_tokens + completion_tokens` if a runner reports the two halves but not their sum.

```json
// GET /runs/{any-run-in-the-tree}/cost
{
  "root_run_id": "root-run-id",
  "run_count": 3,
  "usage": {"prompt_tokens": 420, "completion_tokens": 180, "total_tokens": 600, "cost_usd": 0.03},
  "runs": [
    {"run_id": "root-run-id", "agent_id": "coordinator", "depth": 0, "usage": {...}},
    {"run_id": "child-run-id", "agent_id": "worker", "depth": 1, "usage": {...}}
  ]
}
```

**Known limitations, stated plainly**:
- **Concurrency deadlock risk, real and easy to hit**: if the SAME runner process executes both the calling agent and the agent it delegates to, a `wait=True` call blocks that runner's in-flight job slot waiting for a sub-run the same process must dequeue. Default `--concurrency 1` deadlocks on a single hop. **`--concurrency 2` covers one wait-hop**; a deeper wait-chain (A waits on B waits on C) on one process needs roughly `concurrency >= chain_depth + 1`, or another replica of the same `runner_kind`. Horizontally-scaled runner replicas avoid this entirely (any replica can pick up the sub-run).
- **Trusted fleets remain a boundary.** Parent lookup is tenant-scoped and body-selected foreign parents are rejected. A2A rechecks the assignment after reading the body, but queue validation and child persistence are separate operations. This does not provide per-worker isolation within one runner kind or downscope an external bearer token. Use the documented trusted-fleet deployment model.
- **Cost aggregation is best-effort, not authoritative.** It's reading a convention out of existing `output` JSON, not a value the control plane verified or a runner is required to produce -- a misbehaving or silent runner reports zero, not an error.

## Upgrade and troubleshooting

Custom A2A callers must send `X-Runkite-Generation` and exactly one of `X-Runkite-Run-Id` or `X-Runkite-Run-Id-B64`. Use the current parent assignment, including its real generation. Do not substitute zero. Keep the worker execution context when calling the packaged helpers; hand-built configs without a generation fail before HTTP. Existing custom callers that only supplied `parent_run_id` must be updated with this plane release.

An early `run_binding_required`, `run_not_inflight`, `run_generation_mismatch`, `a2a_parent_mismatch` or `a2a_authority_denied` rejection creates no child. If the parent changes after child persistence, the plane attempts canonical cancellation before dispatch and retains the interrupted record when cleanup commits. A cleanup storage failure is returned as an error and requires operator recovery. Reclaimed or terminal attempts must stop delegating. Do not retry with a different parent or broader permissions. An omitted `on_behalf_of` inherits server-owned identity; a narrowed permission list must retain the rights needed for the target agent. Depth, breadth and synchronous concurrency limits still apply.

See [authority and secret safety](internal/action-authority-hardening.md) for design, compatibility and test boundaries.
