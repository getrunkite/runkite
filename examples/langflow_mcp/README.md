# Langflow MCP Tools calling a Runkite specialist

Unreleased checkout recipe. The incoming MCP transport and stable `run_id`
contract exist in published Runkite v0.9.0. The scoped permissions described
below require the newer source feature and are not in published v0.9.0.

Langflow keeps its flow, conversation, application authentication and agent loop.
Its MCP Tools component calls `https://<your-plane>/mcp`. Only the selected
Runkite specialist creates a managed Runkite thread/run. This adds no Langflow
server, frontend, workflow engine or model dependency to Runkite.

## 1. Prepare the self-hosted plane

Register a working agent and start its existing Runkite runner. Begin with the
provider-free [echo graph](../echo_agent/graph.py), registered as `echo_agent`.
Keep the plane behind your existing HTTPS endpoint. `/mcp` uses Streamable HTTP;
it is not Langflow's own `/api/v1/mcp/.../streamable` server endpoint or a legacy
`/sse` endpoint. Multi-replica installations need the documented
[MCP session affinity](../../docs/mcp-server.md).

Choose one authority profile deliberately:

- **Published v0.9.0:** use a dedicated tenant and trusted server-side caller key
  with `read` and `write`. Write admits `/mcp`; read permits exact REST recovery.
  Do not use Admin. An agent run grant alone does not admit the published HTTP
  MCP route. This key has broader tenant authority than one specialist.
- **Unreleased scoped source:** `agents:echo_agent:run` admits the MCP transport,
  filtered discovery and that specialist's invocation. Add the separate
  `agents:echo_agent:observe` only if the application's recovery code needs exact
  run GET/wait. Observe alone does not invoke; run alone does not grant reads.
  Neither grant provides search, thread history, store access, streaming, cost,
  cancel or delete. Scoped observation requires SQL durable run references.

The MCP-only source profile needs only:

```json
{"permissions": ["agents:echo_agent:run"]}
```

The native scoped probe uses that run-only key. Add observation authority only
to separate application-owned REST recovery code that actually needs it.

Merge that entry into your existing authenticated configuration only after
deploying source that implements the scoped contract. Keep a dedicated tenant
and one trusted caller: agent-scoped observation is not requester-private run
ownership. Selecting one tool in Langflow does not narrow a broad server key.

## 2. Register the remote server in Langflow

The [official MCP client guide](https://docs.langflow.org/mcp-client) describes
this UI path. The native client qualification below pins LFX 1.13.2; the visual
editor and credential-variable UI still require your deployment's validation.

1. In Langflow Settings, create a Credential global variable named
   `RUNKITE_AUTHORIZATION` containing the complete `Bearer <caller-key>` value.
2. Open Settings > MCP Servers > Add MCP Server. Choose HTTP/SSE, name it
   `runkite-specialist`, and set URL to `https://<your-plane>/mcp`.
3. Set header `Authorization` to the global variable name
   `RUNKITE_AUTHORIZATION`. Keep TLS verification enabled and save.
4. Drag that server from the MCP sidebar into the flow. Select `echo_agent` in
   its Tool field. For an agent flow, enable Tool Mode and connect Toolset to
   the Agent's Tools port.

Keep the origin, server credential and specialist under trusted configuration.
Do not forward an arbitrary end-user Authorization header to the plane or let a
flow request choose the destination carrying this credential. For a private
Runkite host, use Langflow's operator-controlled SSRF host allowlist. The local
qualifier allowlists only its loopback fixture and keeps protection enabled.

For a manual smoke without a model, leave Tool Mode off, select `echo_agent`,
and supply these fields before running the component:

```json
{
  "message": "Hello from Langflow",
  "thread_id": "langflow-smoke-conversation",
  "run_id": "langflow-smoke-operation-001"
}
```

Leave `input` unset when using `message`. A supplied `input` object overrides
the message, including an empty object. Expect the echo text and one managed
run in Runkite. A repeated identical operation while retained should return
that run's result. Use a fresh saved run ID for an intentional new turn.
Tool-field binding and Table rendering are not covered by the headless proof.

## 3. Own retry identity in the application

Before dispatch, persist the exact agent, input/message, optional thread ID and
unique run ID as one application operation. Reuse them after a lost response;
never invent a new ID because delivery timed out. Persist IDs in application
code. A model remembering an ID in its prompt is not enforcement. The stock
MCP Tools component does not implement a durable Runkite operation-receipt store.
Unattended Agent Tool Mode needs an application-owned invocation wrapper that
injects and enforces these values before allowing retries or side effects.

The pinned native client can retry a failed session once using the same tool
arguments. Supplying a stable `run_id` is therefore relevant even when the
application did not explicitly click Retry. Reusing an ID with changed input,
agent or explicitly different thread is a tool error. A thread ID alone does
not deduplicate. Deleting/pruning the run ends retry protection; no caller
receipt can resurrect that server-side protection.

For durable background handles or bounded HTTP delivery, use the existing
REST create/get/wait API from application code. This recipe does not turn a
synchronous MCP call into a background job API.

With the unreleased scoped observation feature, save the first
`Runkite-Run-Reference` from an exact REST GET/wait response, or
`_meta["runkite/run_reference"]` from a completed raw MCP result, alongside the
intent. Supply it as `Runkite-Expected-Run-Reference` on later GET/wait requests.
Scoped wait requires it. Langflow's displayed text is not guaranteed to retain
raw result metadata, so preserve it in application code. Missing retention,
reference mismatch or an incompatible backend stops recovery; it never
authorizes a new operation. A first reference acquired only after delivery was
lost cannot prove the ID was not deleted/recreated before that lookup.

## 4. Failure, approval and cancellation boundaries

Runkite run errors, interruptions and timeouts are MCP tool errors. The pinned
Langflow conversion raises those as tool failures rather than success data.
Transport failure means delivery is unknown, not that work failed or stopped.

Connector HITL remains the existing Runkite Review/Admin flow. Langflow's MCP
connection does not approve a pending action, skip required review, resume an
unsupported adapter or execute a held tool itself. An approval does not imply
the tool's business effect completed. Continue only through the existing
adapter/connector contract with the original operation arguments.

Stopping a Langflow request, closing an MCP session or HTTP DELETE `/mcp` does
not prove cancellation of the managed worker. Explicit cancellation uses the
existing Runkite operator/API path and separate authority. The narrow source
caller profile has no cancel grant. Do not report rollback from a disconnected
tool request or automatically replay unknown effects.

Reconnect after credential/permission or schema changes. The current server
session captures its tool catalog, while Langflow also has session/tool caches.
This recipe uses the generic Runkite envelope. Typed nested agent-input
discovery is a separate source feature, not established by this qualification.

## Provider-free native qualification

On November 16, 2026, the [recorded native proof](qualification.json) passed
against the v0.9.0 baseline (`a068c6b8`), using LFX 1.13.2, MCP 1.30.0,
LangChain Core 1.8.1 and Pydantic 2.13.5. It negotiated MCP `2025-11-25`
over Streamable HTTP and produced exactly two managed runs and two worker
assignments for its two intentional operations. The reconnect and failed-run
retries created no additional assignments. This receipt uses the published
read/write credential profile; it does not qualify the new scoped profile.

Install [requirements.txt](requirements.txt) in a separate Python 3.12 client
environment. Use another existing environment for Runkite's LangGraph runner:

```sh
python3.12 -m venv /tmp/runkite-langflow-client
/tmp/runkite-langflow-client/bin/python -m pip install \
  -r examples/langflow_mcp/requirements.txt
python3.12 examples/langflow_mcp/qualify.py \
  --python /tmp/runkite-langflow-client/bin/python \
  --runner-python /absolute/path/to/runner-environment/bin/python \
  --output /tmp/runkite-langflow-qualification.json
```

Use prepared Go/module caches; the qualifier runs with `GOPROXY=off` and never
downloads dependencies or calls a provider. It creates only ephemeral loopback
HTTP/gRPC listeners, a SQLite plane fixture and a real Python worker. Add
`--scoped-source` only when testing a checkout with the new scoped MCP grants.
Existing receipt/log files are never overwritten.

The dedicated `langflow-mcp.yml` workflow runs both baseline and run-only
`--scoped-source` profiles on the integrated checkout, with separate clean
client and runner environments and distinct receipts. The scoped gate requires
the source permission implementation; it has no compatibility fallback.
Strict receipt validation precedes an allowlisted upload of the two synthetic
JSON files. Process logs, intent files and environment state are not uploaded.
The checked-in receipt above remains baseline evidence; the scoped profile must
pass against the integrated tree before claiming that qualification.

The unmodified installed `lfx.base.mcp.util.update_tools` path performs header
handling, discovery, schema conversion and `StructuredTool.ainvoke` over its
native `MCPStreamableHttpClient`. The proof checks a real managed result,
reconnection with the same persisted operation, conflict rejection, terminal
tool errors, error retries and invalid credentials. Server-side row and runner
assignment counts detect accidental redispatch. Hash checks require the native
client and MCP Tools component files to match the pinned public source.

This is native headless client qualification. It does not test the Langflow
web server, project database, visual editor, browser credential-variable
resolution, agent/model loop, real provider or production reverse proxy. See
the [engineering boundary](../../docs/internal/langflow-mcp-boundary.md) for
the exact evidence and outstanding deployment checks.
