# Runner Protocol v0

**Status**: Draft
**Version**: 0.1.0

The Runner Protocol defines how a control plane dispatches agent execution jobs to runners and how runners stream results back. It is transport-agnostic, language-agnostic, and framework-agnostic. Any process that implements this protocol is a valid runner.

This protocol is the boundary between infrastructure (the control plane) and agent execution (the runner). The control plane never imports an agent framework. The runner never implements HTTP endpoints or SSE streaming. Each side does one thing well.

---

## Table of Contents

1. [Concepts](#1-concepts)
2. [Transport Contract](#2-transport-contract)
3. [RunAssignment](#3-runassignment)
4. [RunEvent](#4-runevent)
5. [Execution Lifecycle](#5-execution-lifecycle)
6. [Checkpoint API](#6-checkpoint-api)
7. [Store API](#7-store-api)
8. [Connector Session API](#8-connector-session-api)
9. [Interrupts and HITL](#9-interrupts-and-hitl)
10. [Cancellation](#10-cancellation)
11. [Runner Authentication](#11-runner-authentication)
12. [Observability](#12-observability)
13. [Error Handling](#13-error-handling)
14. [Conformance Requirements](#14-conformance-requirements)
15. [Versioning](#15-versioning)

---

## 1. Concepts

### Control Plane

The server process (Go) that implements the Agent Protocol HTTP/SSE/WebSocket API. It owns metadata (agents, threads, runs), the job queue, event fan-out, auth, and platform features. It dispatches work to runners and relays events to clients.

### Runner

A process that executes agent turns. It receives a `RunAssignment`, executes the agent (LangGraph, CrewAI, LlamaIndex, raw code -- anything), and emits `RunEvent` messages back. A runner is identified by its `runner_kind` (e.g., `python-langgraph`, `typescript-langgraphjs`, `python-crewai`).

### Run

A single invocation of an agent on a thread. A run has a lifecycle: `pending` -> `running` -> `success` | `error` | `interrupted`.

### Transport

The mechanism by which the control plane delivers `RunAssignment` to a runner and the runner delivers `RunEvent` back. The protocol defines the message shapes; the transport defines how they move. Multiple transports are supported; all must pass the same conformance suite.

---

## 2. Transport Contract

### 2.1 Supported Transports

| Transport | Direction | Description |
|-----------|-----------|-------------|
| **Pull/Queue** | Runner pulls jobs from a shared queue (Redis BLPOP, NATS subscribe, Kafka consume). Events published to a shared event bus (Redis Pub/Sub, NATS publish). | Production default. Runner initiates all connections. No inbound networking required on the runner. |
| **gRPC Long-Poll** | Runner calls the control plane's gRPC endpoint. Control plane holds the connection until a job is available (Temporal-style). Events streamed back on the same gRPC connection or via the event bus. | Dev/cloud default. Runner-initiated, no service discovery. |

Both transports carry the same `RunAssignment` and `RunEvent` JSON payloads. The transport is a delivery mechanism, not a data format.

### 2.2 Transport Conformance Requirements

Every transport implementation MUST satisfy these guarantees. The conformance test suite validates all of them.

**Job Delivery:**

- **At-least-once delivery**: every enqueued `RunAssignment` is delivered to exactly one runner. If the runner crashes before acknowledging, the job is re-delivered (via lease expiry + reaper).
- **FIFO ordering**: jobs enqueued in order A, B, C are dequeued in the same order by a single consumer. With multiple consumers, each job is delivered to exactly one consumer (no duplicates under normal operation).
- **No loss on transport failure**: if the transport (Redis, NATS) restarts, in-flight jobs are either re-delivered or persisted. The control plane's lease/reaper mechanism is the ultimate recovery path.

**Event Delivery:**

- **Ordered per run**: events for a given `run_id` are delivered in the order they were published (by `seq` number). Events across different runs have no ordering guarantee.
- **Fan-out**: multiple control plane subscribers to the same `run_id` each receive every event. This enables multiple SSE clients on the same run.
- **Replay**: the transport (or a replay buffer in the control plane) stores events for a configurable TTL. A subscriber can request replay from a given `seq` number to recover missed events after a disconnect.

**Cancellation:**

- **Cancel propagation**: when the control plane cancels a run, the cancel signal reaches the runner within the transport's delivery latency. For queue transports, this is via a side-channel (Redis Pub/Sub, NATS subject). For gRPC, this is via the gRPC cancellation mechanism.
- **Cancel before dequeue**: if a job is cancelled before any runner dequeues it, the job is removed or poisoned. A runner that dequeues a poisoned job MUST check the run status against the control plane before executing.

---

## 3. RunAssignment

A `RunAssignment` is a JSON object sent from the control plane to a runner. It contains everything the runner needs to execute one agent turn.

### 3.1 Schema

```json
{
  "run_id": "string (UUID)",
  "thread_id": "string (UUID)",
  "runner_kind": "string",
  "graph_id": "string",
  "input": "any (JSON value -- object, array, string, number, boolean, or null)",
  "config": {
    "configurable": "object (key-value pairs passed to the agent)",
    "tags": ["array of strings (optional)"],
    "recursion_limit": "integer (optional)",
    "metadata": "object (optional)"
  },
  "user": {
    "identity": "string",
    "display_name": "string (optional)",
    "is_authenticated": "boolean",
    "permissions": ["array of strings"],
    "additional_fields": "any provider-specific fields flattened onto the same object"
  },
  "checkpoint_ref": "string or null (opaque checkpoint id for time-travel; null = latest)",
  "resume_command": "object or null",
  "stream_modes": ["array of strings"],
  "connector_needs": ["array of strings"],
  "trace_context": {
    "traceparent": "string (W3C Trace Context format)",
    "tracestate": "string",
    "correlation_id": "string (any non-empty identifier)"
  }
}
```

### 3.2 Field Definitions

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `run_id` | string (UUID) | Yes | Unique identifier for this run. Assigned by the control plane. |
| `thread_id` | string (UUID) | Yes | The thread this run executes on. The runner uses this to load/save checkpoints. |
| `runner_kind` | string | Yes | Identifies which runner should handle this job (e.g., `python-langgraph`). Queue channels/subjects are scoped by this value. |
| `graph_id` | string | Yes | Identifies which agent/graph to execute. Maps to a graph definition in the runner's config. |
| `input` | any JSON | No | The input to the agent. May be null for resume-only runs. The shape is defined by the agent, not by this protocol. |
| `config` | object | No | Agent configuration. `config.configurable` carries key-value pairs the agent can read. The runner MUST inject `thread_id` and `run_id` into `configurable` if the agent framework expects them (e.g., LangGraph). |
| `user` | object | No | Authenticated identity from the control plane's auth layer (flat wire shape: `identity`, `display_name`, `is_authenticated`, `permissions`, plus provider Extra fields). The runner SHOULD make this available to the agent (e.g. LangGraph `langgraph_auth_user` / `runtime.user`). Absent when no auth provider is configured. |
| `checkpoint_ref` | string or null | No | Opaque checkpoint identity for resume-by-id (time-travel). Null/absent means resume from the thread's latest checkpoint (or a fresh run). LangGraph runners MUST map a non-empty value to the framework's checkpoint-id config key (`configurable.checkpoint_id`). Runners whose framework has no resume-by-id MUST fail the run with an error event -- never silently ignore a non-null value. |
| `resume_command` | object or null | No | If non-null, this run is resuming from an interrupt (HITL). Contains the client's response to the interrupt. The runner MUST pass this to the agent framework's resume mechanism (e.g., LangGraph `Command(resume=...)`). |
| `stream_modes` | array of strings | No | Which event types the control plane wants. Well-known values: `values`, `updates`, `messages`, `custom`. Default: `["values"]`. Runners SHOULD treat unknown mode strings as pass-through (forward compatibility -- the control plane may map client-requested modes like `events` or `debug` before dispatch, or new modes may be added in future spec versions). The runner SHOULD only emit data events matching these modes (optimization, not a hard requirement -- the control plane filters regardless). **`lifecycle` and `end`/`error` events are always emitted regardless of `stream_modes`** -- they are control events, not data events. |
| `connector_needs` | array of strings | No | Pre-warm hint. The control plane MAY call GetSession for these connectors at dispatch (e.g. warm OAuth caches) but MUST NOT embed sessions or credentials in the assignment. The runner MAY request sessions for connectors not in this list on-demand via the Connector Session API. This is a hint, NOT an allow-list. |
| `tenant_id` | string | No | Tenant that authenticated the originating request. Runners MUST scope direct-mode `store_items`/`vector_items` SQL (and proxy `X-Runkite-Tenant-Id` on `/internal/*`) to this value. Absent on older control planes -- runners fall back to `"default"`. LangGraph runners MUST also encode this into the checkpointer key: `configurable.thread_id` is the bare `thread_id` when tenant is `"default"`/absent, otherwise `"{tenant_id}:{thread_id}"` (logical thread remains the top-level `thread_id` field; avoid `:` inside tenant ids -- the encoding is a single colon split). Proxy-mode: after kind-token auth the control plane accepts the runner-supplied `X-Runkite-Tenant-Id`, optionally constrained by `RUNNER_TENANTS_<kind>` when configured; see docs/auth.md. |
| `trace_context` | object | No | W3C Trace Context for cross-process observability. The runner SHOULD set this as the active trace context before executing the agent, so that all spans (LLM calls, tool invocations, etc.) are children of this trace. |

### 3.3 Rules

- `run_id` and `thread_id` are always present and are UUIDs.
- `input` can be any valid JSON value. The protocol does not constrain its shape. Null means "no input" (valid for resume-only runs).
- The control plane sets `thread_id` and `run_id` in `config.configurable` before dispatching the RunAssignment. The runner MUST verify they are present and MUST NOT replace them with a *different logical* run/thread. If they are missing (e.g., due to a bug), the runner MUST inject them from the top-level `run_id` and `thread_id` fields. LangGraph runners MAY encode `tenant_id` into `configurable.thread_id` as the checkpointer key (`"{tenant_id}:{thread_id}"` for non-default tenants) -- that is the same logical thread, not a substitution.
- Unknown fields in RunAssignment MUST be ignored by the runner (forward compatibility).

---

## 4. RunEvent

A `RunEvent` is a JSON object published by the runner back to the control plane. It represents one event in the agent's execution stream.

### 4.1 Schema

```json
{
  "event_id": "string",
  "seq": "integer (>= 1)",
  "method": "string",
  "namespace": ["array of strings"],
  "data": "any (JSON value)",
  "ts": "integer (milliseconds since epoch)"
}
```

### 4.2 Field Definitions

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `event_id` | string | Yes | Unique identifier for this event within the run. Format: `{run_id}_evt_{seq}`. Used for deduplication on replay. |
| `seq` | integer | Yes | Monotonically increasing sequence number within the run. Starts at 1. Used for ordering and replay. |
| `method` | string | Yes | The event type. See Method Values below. |
| `namespace` | array of strings | Yes | Hierarchical path identifying the position in the agent tree. Empty array `[]` for root-level events. For subgraph events, contains the path (e.g., `["sub_agent", "tool_node"]`). |
| `data` | any JSON | Yes | The event payload. Shape depends on `method`. |
| `ts` | integer | Yes | Timestamp in milliseconds since Unix epoch. Set by the runner at the time the event is created. |

### 4.3 Method Values

| Method | Description | `data` shape |
|--------|-------------|-------------|
| `values` | Full state snapshot after a node executes. | The agent's current state as a JSON object. |
| `updates` | Incremental update from a single node. | `{ "node_name": { ...node_output... } }` |
| `messages` | Chat message(s) produced by the agent. | Message object or array of message objects (role, content, id, metadata). |
| `lifecycle` | Execution lifecycle event. | `{ "event": "running" | "completed" | "failed" | "started" | "interrupted", ...optional_fields }` |
| `input.requested` | The agent needs human input (HITL interrupt). | `{ "interrupt_id": "string", "value": any, "description": "string (optional)" }` |
| `custom` | User-defined custom event (no name). | `{ "payload": any }` |
| `custom:{name}` | Named custom event from a user stream channel. | `{ "name": "string", "payload": any }` |
| `end` | Terminal event. The run is finished. | `{ "status": "success" | "error" | "interrupted" }` |
| `error` | Error event. The run failed. | `{ "message": "string", "type": "string (optional)", "stacktrace": "string (optional)" }` |
| `tool_auth` | Control-plane connector policy denial / pending HITL (not emitted by runners). | `{ "stage": "tool.call" \| "connector.session", "effect": "deny" \| "pending", "connector": "string", "tool": "string", "reason": "string", "reason_code": "string", "rule_id": "string", "generation": number, "action_id"?: "string" }` |

### 4.4 Rules

- `seq` MUST be monotonically increasing within a run. The runner MUST NOT reuse or skip sequence numbers.
- `event_id` MUST be unique within a run. The recommended format is `{run_id}_evt_{seq}`. Control-plane `tool_auth` events use `{run_id}_tool_auth_{hex}` so they never collide with runner IDs; their `seq` is best-effort (`max(replay)+1`) and may interleave with concurrent runner publishes — clients MUST dedupe by `event_id`.
- Every run MUST end with either an `end` event (status=success or status=interrupted) or an `error` event. A run with no terminal event is considered crashed.
- The `lifecycle` method with `event: "running"` SHOULD be the first event emitted after the runner begins execution.
- The `lifecycle` method with `event: "interrupted"` MUST be emitted before an `input.requested` event (it signals the run is paused; the `input.requested` provides the details of what's needed).
- `namespace` for root-level events is `[]`. For subgraph events, it contains the subgraph hierarchy path. The control plane uses this for namespace filtering.
- Unknown `method` values MUST be forwarded by the control plane without interpretation (forward compatibility).
- `data` can be any valid JSON value. The protocol does not constrain its shape beyond the method-specific conventions above.

---

## 5. Execution Lifecycle

### 5.1 Normal Execution

```
Control Plane                    Runner
     |                             |
     |-- RunAssignment ----------->|
     |                             | Load graph, prepare execution
     |                             |
     |<-- RunEvent(lifecycle,      |
     |    event=running) ----------|
     |                             |
     |<-- RunEvent(values/updates/ |
     |    messages) ---------------|  (0 or more events per node)
     |                             |
     |<-- RunEvent(end,            |
     |    status=success) ---------|
     |                             |
```

### 5.2 States

| State | Description | Transitions to |
|-------|-------------|----------------|
| `pending` | Run created, not yet picked up by a runner. | `running` (runner dequeues) or `interrupted` (cancelled before pickup) |
| `running` | Runner is executing the agent. | `success`, `error`, `interrupted` |
| `success` | Agent completed normally. Terminal. | -- |
| `error` | Agent threw an exception or max retries exceeded. Terminal. | -- |
| `interrupted` | Agent was cancelled or hit an interrupt (HITL). | `running` (on resume) or stays terminal if not resumed |

### 5.3 Runner Responsibilities

1. **Dequeue or receive** the `RunAssignment`.
2. **Check run status** against the control plane before executing (guard against cancel-after-dequeue race).
3. **Set trace context** from `trace_context` before any agent code runs.
4. **Load the agent** identified by `graph_id`.
5. **`checkpoint_ref`**: if non-null, resume from that specific past checkpoint (LangGraph: set `configurable.checkpoint_id`). If null/absent, resume from the thread's latest checkpoint via the framework checkpointer.
6. **If `resume_command` is non-null**, pass the resume payload to the agent framework.
7. **Execute the agent** and emit `RunEvent` messages for each event.
8. **On completion**, emit an `end` event with the appropriate status.
9. **On error**, emit an `error` event with the exception details.
10. **On cancel signal**, stop execution cooperatively and emit an `end` event with `status=interrupted`.
11. **Update run status** on the control plane after the terminal event.

---

## 6. Checkpoint API

Checkpoints store the agent's execution state so it can be resumed later (e.g., after an interrupt, crash recovery, or time-travel debugging).

### 6.1 Dual Mode

The runner accesses checkpoints in one of two modes:

**Direct mode** (default for Python runner on Postgres): The runner holds a direct database connection and reads/writes checkpoints using the agent framework's native checkpoint driver (e.g., LangGraph's `AsyncPostgresSaver`). Zero network overhead. The control plane and runner share the same `DATABASE_URL`.

**Proxy mode** (default for non-Python runners, or when the runner cannot hold DB credentials): The runner reads/writes checkpoints via HTTP calls to the control plane. The control plane stores checkpoint data as opaque blobs.

### 6.2 Proxy Mode API

Base URL: the control plane's internal API root.

**Save checkpoint:**
```
PUT /internal/checkpoints/{thread_id}/{checkpoint_id}
Content-Type: application/octet-stream
Body: <opaque checkpoint bytes>

Response: 204 No Content
```

**Load checkpoint:**
```
GET /internal/checkpoints/{thread_id}/{checkpoint_id}

Response: 200 OK
Content-Type: application/octet-stream
Body: <opaque checkpoint bytes>

Response: 404 Not Found (if no checkpoint exists)
```

**List checkpoints for a thread:**
```
GET /internal/checkpoints/{thread_id}

Response: 200 OK
Content-Type: application/json
Body: [
  { "checkpoint_id": "string", "created_at": "ISO 8601 timestamp" },
  ...
]
```

### 6.3 Rules

- In direct mode, the runner uses its framework's native checkpoint driver. The control plane does not participate in checkpoint read/write during execution.
- In proxy mode, checkpoint data is opaque bytes. The control plane stores and returns them without parsing.
- The control plane stores checkpoint identity (`thread_id` + `checkpoint_id`) for its own bookkeeping and forwards a client-chosen past checkpoint via `RunAssignment.checkpoint_ref` for time-travel resume. HITL resume uses `resume_command` (optionally combined with `checkpoint_ref`); without `checkpoint_ref`, HITL resumes from the thread's latest checkpoint.
- The runner MUST write a checkpoint after each node step if the agent framework supports it. This enables crash recovery and HITL resume at the correct position.
- Direct mode requires the runner to have database credentials (`DATABASE_URL`). Proxy mode does not.

---

## 7. Store API

The store is a key-value storage system shared between the control plane and all runners. The control plane owns the store schema and serves the `/store/*` Agent Protocol endpoints. Runners access the store during execution.

### 7.1 Dual Mode

Same pattern as checkpoints:

**Direct mode**: Runner holds a direct database connection to the control plane's `store_items` table. The runner SDK implements the agent framework's store interface (e.g., LangGraph `BaseStore`) as a thin driver over these tables. Zero network overhead.

**Proxy mode**: Runner accesses the store via the control plane's internal HTTP API.

### 7.2 Proxy Mode API

**Put item:**
```
PUT /internal/store/items
Content-Type: application/json
Body: {
  "namespace": ["array", "of", "strings"],
  "key": "string",
  "value": { ... any JSON object ... }
}

Response: 204 No Content
```

**Get item:**
```
GET /internal/store/items?namespace=array,of,strings&key=mykey

Response: 200 OK
Content-Type: application/json
Body: {
  "namespace": ["array", "of", "strings"],
  "key": "mykey",
  "value": { ... },
  "created_at": "ISO 8601",
  "updated_at": "ISO 8601"
}

Response: 404 Not Found
```

**Delete item:**
```
DELETE /internal/store/items
Content-Type: application/json
Body: {
  "namespace": ["array", "of", "strings"],
  "key": "mykey"
}

Response: 204 No Content
```

**Search items:**
```
POST /internal/store/items/search
Content-Type: application/json
Body: {
  "namespace_prefix": ["array", "of", "strings"],
  "filter": { "key": "value" },
  "limit": 10,
  "offset": 0
}

Response: 200 OK
Content-Type: application/json
Body: {
  "items": [ ... ]
}
```

**List namespaces:**
```
POST /internal/store/namespaces
Content-Type: application/json
Body: {
  "prefix": ["optional"],
  "max_depth": 3,
  "limit": 100,
  "offset": 0
}

Response: 200 OK
Content-Type: application/json
Body: [["namespace", "path"], ...]
```

### 7.3 Rules

- In direct mode, the runner's store driver writes to the SAME tables the control plane uses. There is only one set of store tables. The control plane owns the schema (DDL/migrations); the runner never creates or modifies store tables.
- In proxy mode, all store operations go through the control plane's internal HTTP API. The control plane enforces any tenant isolation or access control.
- `namespace` is always an array of strings. An empty array is a valid root namespace.
- `value` is always a JSON object (not a primitive). This matches the Agent Protocol Store spec.

---

## 8. Connector Session API

The connector registry is a control plane feature that manages authenticated sessions for external services (Salesforce, Snowflake, MCP servers, etc.). Runners in any language can request pre-authenticated sessions without implementing OAuth flows themselves.

### 8.1 API

**Request a session:**
```
POST /internal/connectors/{connector_name}/session
Content-Type: application/json
Body: {
  "user_context": {
    "user_id": "string",
    "sso_token": "string (optional)",
    "additional_fields": "any"
  }
}

Response: 200 OK
Content-Type: application/json
Body (non-MCP connector): {
  "credentials": {
    "access_token": "string",
    "instance_url": "string (optional)",
    "refresh_token": "string (optional)"
  },
  "expires_at": "ISO 8601 (api_key/bearer: 1h advisory remint hint; OAuth: IdP expires_in)"
}

Body (MCP connector — proxy-only, no raw credentials): {
  "session_token": "opaque capability token",
  "expires_at": "ISO 8601 capability expiry (15m absolute)",
  "mcp": {
    "url": "/internal/connectors/{connector_name}/mcp",
    "tools": ["optional preview"]
  }
}

MCP proxy calls also require:
```
POST /internal/connectors/{connector_name}/mcp
X-Runkite-Connector-Session: <session_token from GetSession>
X-Runkite-Run-Id / X-Runkite-Generation: (run-binding)
```

Response: 401 Unauthorized (user not authorized / missing session token)
Response: 404 Not Found (connector not configured)
Response: 502 Bad Gateway (upstream auth provider failed)
```

### 8.2 Rules

- `connector_needs` in RunAssignment is a pre-warm hint. The control plane MAY call GetSession for listed connectors (e.g. to warm OAuth caches) but MUST NOT embed sessions or credentials in the assignment payload. Runners mint/fetch at use time via GetSession.
- For non-MCP `api_key`/`bearer`, `expires_at` is a 1h advisory remint hint (does not revoke the underlying secret). OAuth uses the IdP's `expires_in`.
- Session credentials / tokens are scoped to the in-flight run (run-binding). MCP `session_token` is additionally bound to `(run_id, generation, connector)`.
- Downstream OAuth client-credentials tokens may be cached server-side. Runner-facing MCP capability tokens are absolute-TTL and not sliding.
- Tool allow/deny is enforced on the control-plane MCP proxy, not by trusting the runner to filter.
- Secrets (client_id, client_secret, private_key) and raw MCP URLs are NEVER returned to the runner. For MCP connectors, raw access tokens are also omitted.
- If the connector's upstream auth provider fails, the control plane returns 502 with an error message from the connector's configured error taxonomy.

---

## 9. Interrupts and HITL

Human-in-the-loop (HITL) allows an agent to pause execution and wait for human input before continuing.

### 9.1 Flow

```
Runner                         Control Plane                Client
  |                                 |                          |
  |-- RunEvent(lifecycle,           |                          |
  |   event=interrupted) --------->|                          |
  |                                 |                          |
  |-- RunEvent(input.requested,     |                          |
  |   interrupt_id=X,               |                          |
  |   value=...) ----------------->|-- SSE: input.requested -->|
  |                                 |                          |
  | (runner stops, saves checkpoint)|                          |
  |                                 |                          |
  |                                 |<-- input.respond --------|
  |                                 |   (interrupt_id=X,       |
  |                                 |    response=...)         |
  |                                 |                          |
  |<-- new RunAssignment -----------|                          |
  |   (resume_command={             |                          |
  |     interrupt_id: X,            |                          |
  |     response: ...})             |                          |
  |                                 |                          |
  | (runner loads checkpoint,       |                          |
  |  resumes with response)         |                          |
  |                                 |                          |
  |-- RunEvent(values/updates) --->|-- SSE: events ---------->|
  |-- RunEvent(end, success) ----->|-- SSE: end -------------->|
```

### 9.2 Runner Rules

1. When the agent framework signals an interrupt, the runner MUST:
   a. Emit a `lifecycle` event with `event: "interrupted"`.
   b. Emit one `input.requested` event per interrupt, each with a unique `interrupt_id`.
   c. Save a checkpoint at the current execution state.
   d. Emit an `end` event with `status: "interrupted"`.
   e. Stop execution. Do NOT wait for the response in-process.

2. When the runner receives a new `RunAssignment` with `resume_command` set:
   a. Load the referenced checkpoint when `checkpoint_ref` is set; otherwise load the thread's latest checkpoint (framework checkpointer / proxy store).
   b. Pass `resume_command` to the agent framework's resume mechanism.
   c. Continue emitting events from where execution was interrupted.

3. Multiple interrupts in a single run (sequential approval steps) are supported. Each interrupt gets its own `interrupt_id` and `input.requested` event.

4. If the same `interrupt_id` appears in multiple event sources (e.g., in both a `values` event's `__interrupt__` key and an `updates` event's `__interrupt__` node), the runner SHOULD emit only one `input.requested` event per unique `interrupt_id` (deduplication).

---

## 10. Cancellation

### 10.1 Cancel Signaling

The control plane signals cancellation via a transport-specific side-channel:

- **Queue transport**: publish to a cancel topic/channel (e.g., Redis Pub/Sub `cancel:{run_id}`, NATS `cancel.{run_id}`). The runner subscribes to this channel for each active run.
- **gRPC transport**: cancel the gRPC context or send a `CancelRun` message on the bidirectional stream.

### 10.2 Runner Rules

1. The runner MUST subscribe to the cancel channel for each run it is executing.
2. On cancel signal, the runner MUST:
   a. Cooperatively stop the agent's execution (e.g., raise `asyncio.CancelledError` in LangGraph, set a flag checked between tool calls).
   b. Emit an `end` event with `status: "interrupted"`.
   c. Update the run status on the control plane to `interrupted`.
3. The runner SHOULD attempt to save a checkpoint before stopping, to enable future resume. This is best-effort -- if the agent cannot be cleanly interrupted, the last good checkpoint is used.

### 10.3 Cancel-After-Dequeue Race

If a run is cancelled after the runner has dequeued the `RunAssignment` but before execution begins:

1. The runner MUST check the run's current status against the control plane (via `GET /internal/runs/{run_id}/status`) before starting execution.
2. If the status is `interrupted` or any terminal state, the runner MUST NOT execute the run. It should discard the assignment.
3. The transport SHOULD remove or poison the job in the queue when cancel is received. This is a best-effort optimization -- the pre-execution status check is the authoritative guard.

---

## 11. Runner Authentication

### 11.1 Local Mode (Default)

No authentication. The runner is trusted implicitly. Suitable for local development where the control plane and runner are on the same machine.

### 11.2 Production Mode

Each runner authenticates with a shared token scoped to its `runner_kind`. The token is passed in every request from the runner to the control plane's internal APIs and in the queue connection credentials.

**Token configuration** (control plane side):
```env
RUNNER_TOKEN_python_langgraph=<secret>
RUNNER_TOKEN_typescript_langgraphjs=<secret>
```

**Token usage** (runner side):
- For internal HTTP APIs: `Authorization: Bearer <token>` header.
- For queue transport: token is part of the connection credentials or used to authenticate the channel subscription.

### 11.3 Rules

- In local mode, the control plane accepts all internal API requests without authentication.
- In production mode, a runner with `runner_kind=python-langgraph` can only authenticate with the token for `python_langgraph`. A leaked token cannot impersonate a different `runner_kind`.
- Queue channels/subjects are scoped per `runner_kind`. A runner only sees jobs for its own `runner_kind`.
- Token rotation is NOT supported in v0. For environments requiring rotation, use an external secrets manager and restart runners.

---

## 12. Observability

### 12.1 Trace Context Propagation

The control plane injects W3C Trace Context into every `RunAssignment.trace_context`:

```json
{
  "traceparent": "00-{trace_id}-{parent_span_id}-{trace_flags}",
  "tracestate": "",
  "correlation_id": "{request_uuid}"
}
```

The runner MUST:
1. Parse `traceparent` and set it as the active trace context before executing the agent.
2. Create a child span for the run execution.
3. All agent-internal spans (LLM calls, tool invocations, store operations) SHOULD be children of this span.

This ensures a single client request produces one connected trace: client -> control plane -> queue -> runner -> agent.

### 12.2 Structured Logging

The runner SHOULD include the following fields in structured log output:
- `run_id`
- `thread_id`
- `graph_id`
- `runner_kind`
- `correlation_id` (from `trace_context`)

---

## 13. Error Handling

### 13.1 Agent Errors

If the agent throws an exception during execution:

1. The runner MUST emit an `error` event:
```json
{
  "event_id": "{run_id}_evt_{seq}",
  "seq": 42,
  "method": "error",
  "namespace": [],
  "data": {
    "message": "Tool 'search_api' failed: connection timeout",
    "type": "ToolExecutionError",
    "stacktrace": "optional, for debug builds only"
  },
  "ts": 1720000000000
}
```

2. The runner MUST update the run status to `error` on the control plane.
3. The runner process MUST NOT crash. It MUST remain available for the next `RunAssignment`.

### 13.2 Runner Errors

If the runner itself fails (not the agent -- e.g., cannot connect to the checkpoint store, transport error):

1. The runner SHOULD log the error with `run_id` and `correlation_id`.
2. If possible, emit an `error` event. If the transport is unavailable, the lease/reaper mechanism in the control plane will detect the crash and re-enqueue the job.
3. The runner process MAY crash if the error is unrecoverable. The control plane's lease expiry will handle recovery.

### 13.3 Retry Behavior

The runner does not implement retry logic. Retry is managed by the control plane:

1. If a runner crashes (lease expires, no terminal event), the control plane's reaper re-enqueues the job.
2. Each re-enqueue increments a `_retry_count` in the job metadata.
3. After `max_retries` (default: 3), the control plane marks the run as permanently failed.
4. The runner receives the `_retry_count` in the RunAssignment (in `config.configurable._retry_count` or a metadata field) but does not act on it -- it is informational.

---

## 14. Conformance Requirements

A runner implementation is considered conformant if it passes the following:

### 14.1 Required (MUST)

1. **Accepts RunAssignment**: parses all required fields without error.
2. **Emits valid RunEvents**: all events pass JSON schema validation against `schemas/run_event.json`.
3. **Monotonic seq**: `seq` values are strictly increasing within a run.
4. **Terminal event**: every run ends with an `end` or `error` event.
5. **Lifecycle events**: emits `lifecycle(event=running)` at start and appropriate terminal lifecycle before the `end` event.
6. **Cancel handling**: responds to cancel signal by stopping execution and emitting `end(status=interrupted)`.
7. **Interrupt handling**: on HITL interrupt, emits `lifecycle(event=interrupted)` + `input.requested` + `end(status=interrupted)`. On resume, loads checkpoint and continues.
8. **Status update**: reports final run status to the control plane after the terminal event.

### 14.2 Recommended (SHOULD)

1. **Trace context**: sets `trace_context` as the active OTel context.
2. **Stream mode filtering**: only emits events matching `stream_modes`.
3. **Pre-execution status check**: checks run status before executing a dequeued assignment (cancel-after-dequeue guard).
4. **Checkpoint on interrupt**: saves a checkpoint before emitting the interrupt's terminal event.
5. **Structured logging**: includes `run_id`, `thread_id`, `correlation_id` in log output.

### 14.3 Validation

Conformance is validated by running the runner against the golden test fixtures in `runner-protocol/examples/`. Each fixture defines:
- A `RunAssignment` input
- The expected sequence of `RunEvent` outputs (method, data structure, ordering)
- Terminal event and status

The runner is tested with a mock agent that produces deterministic output for each fixture.

Gates in this repo:
- `make test-protocol-fixtures` — schema + lifecycle invariants on the JSON examples (Go)
- `make test-protocol-execute` — live `execute_run` with a scripted mock graph, diffed against `expected_events` (Python; also wired into `make test-python`)

---

## 15. Versioning

### 15.1 Protocol Version

The Runner Protocol is versioned independently of the Agent Protocol spec and the control plane software.

- **v0.x**: breaking changes are allowed between minor versions. Runners SHOULD pin to a specific minor version.
- **v1.0+**: no breaking changes without a major version bump. Runners can rely on backward compatibility within a major version.

### 15.2 Version Negotiation

The control plane includes a `protocol_version` field in internal API responses:

```json
GET /internal/info
{
  "protocol_version": "0.1.0",
  "control_plane_version": "1.0.0",
  "supported_transports": ["queue", "grpc"]
}
```

The runner SHOULD check `protocol_version` on startup and warn if it does not match its expected version.

### 15.3 Forward Compatibility

- **RunAssignment**: unknown fields MUST be ignored by the runner.
- **RunEvent**: unknown `method` values MUST be forwarded by the control plane without interpretation.
- **Internal APIs**: unknown response fields MUST be ignored by the runner.

This allows the control plane and runner to be upgraded independently, as long as they share the same major version.
