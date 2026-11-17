# Managed Google ADK runner

Runs an ordinary public `LlmAgent` through `python-google-adk`. Runkite owns
Threads/Runs, dispatch, cancellation, received usage and durable records. The
SDK retains its model loop in the Python worker.

## Original run records (0.10.0 source contract)

Committed receipts now retain the assignment's original `run_incarnation_id`.
Only the same public run ID and exact original record replay saved output.
A known new record reusing that public ID executes a fresh turn with the owned
conversation history. A legacy receipt without the record proof is ambiguous
for the same public ID and fails closed; a different public run ID can still
restore that legacy conversation normally. No history is imported or relabelled.
Use matching 0.10.0 source plane/runner builds. Back up and drain all writers and
older assignments, then apply Postgres/SQLite migration 23 or MySQL migration 24
before resuming traffic. This source contract does not establish registry
publication; published 0.9.0 packages do not implement it. Until matching
artifacts are published and verified, use the source installation below with a
plane built from the same checkout. Follow the
[coordinated upgrade](../../../docs/ops-runbook.md#assignment-identity-upgrade).

Native SDK turn IDs derive from the public run ID and original record proof so
ID reuse cannot alias native invocations or prior-turn usage. The Runkite
factory context retains the public `run_id`.

## Install

PyPI `0.6.1` first included this profile through the `google-adk` extra.
This checkout contains the 0.10.0 source contract.
To install the matching runner from source, use a clean Python 3.11+
environment from the repository root:

```bash
python3.12 -m venv .venv-google-adk
.venv-google-adk/bin/python -m pip install './python[google-adk]'
.venv-google-adk/bin/runkite-runner-google-adk \
  --config examples/managed_google_adk/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

The extra pins `google-adk==2.13.0`. The installed module alternative is
`python -m google_adk_adapter`; no source `PYTHONPATH` is required. The ordinary
CLI and stock image retain LangGraph defaults; the image does not install this
optional SDK. For the published release, use
`pip install 'runkite-runner[google-adk]'`.

Load the same config in the plane, including `runner_kind: python-google-adk`.
For authentication, use `RUNNER_TOKEN` in the worker and matching
`RUNNER_TOKEN_PYTHON_GOOGLE_ADK` and `RUNNER_TENANTS_PYTHON_GOOGLE_ADK` on the
plane. The same credential binds gRPC and checkpoint HTTP.

## Agent and input

Each graph export is a per-run factory receiving frozen `GoogleAdkRunContext`
with run/thread/tenant/generation/graph identity and cancellation. It returns a
fresh ordinary `google.adk.agents.LlmAgent`. Static instructions stay in that
server-owned factory. The offline example uses a public `BaseLlm` and needs no
provider key; production model configuration remains application-owned.

Send exactly one new user/human text message in `input.messages`, with no other
input fields. Text parts are accepted, but batches, system messages, assistant
history, native resume and multimodal content are unsupported. The session
supplies previous history. The app keeps UI authentication and presentation
history; this is Agent Protocol, not the AI SDK UI stream format.

## Session and recovery

A small run-local implementation of public `BaseSessionService` holds one
native `Session`. Its app/user/session tuple is derived from the managed
graph/tenant/thread identity, with no browser-selected native identity. It
restores native events, tool results and session-local state from opaque CAS
storage and does not access SDK private storage maps or revision markers.

ADK `app:` and `user:` state normally spans sessions. This profile rejects those
scopes rather than silently changing their meaning. `temp:` state is visible
during an invocation and stripped before durable snapshot. Unprefixed JSON
business data remains session-local.

Only a fully drained, validated successful invocation commits state. Native
partial text is reconciled with completed responses so aggregate text is not
appended twice. Errors, pending confirmations, unfinished functions or a final
response marker alone cannot produce success. Successful receipts replay the
same run under the new generation without constructing the agent or repeating
model/tool work. Failed or interrupted turns retain the previous committed
session and report usage already observed. A state I/O failure withholds terminal
completion so the assignment remains recoverable.

External effects before a confirmed state commit still require downstream
idempotency. Cancellation cannot undo them or recover usage not received by the
plane. Native checkpoints contain conversation, tool and state data; apply
database access and retention controls.

## Tools, usage and tracing

A supplied `allowed_tools` list is checked before local bodies; `[]` denies all.
Native confirmation is checked against the actual proposed arguments before
execution. A confirmation-required proposal fails without running the body;
the model proposal may already have consumed tokens. This profile does not add
an approval/resume engine. An unsupported state write can fail after tool code
has run; it never becomes a successful checkpoint.

Ordinary functions gain connector grants, secret injection, connector audit and
HITL only when they use the existing connector helpers. Local allowlisting is
cooperative, not a Python sandbox.

Model callbacks report current-invocation usage before tool effects. Prompt
counts include tool-use prompt tokens; completion includes reasoning/thought
tokens. Per-call cumulative stream usage is reconciled with its final response
instead of counted twice. A consistent known model lets the existing pricebook
estimate USD. Mixed/unknown identities stay unpriced with tokens retained.
Estimates use the operator's pricebook and are not provider invoices.

ADK-owned content capture is disabled with public per-invocation telemetry
configuration. Existing generic-worker OTLP tracing remains available. Explicit
SDK administrator overrides or separately installed auto-instrumentation have
their own controls. The profile adds no hosted telemetry or logging backend.

An explicit factory-owned `Gemini.client` is per-run and closed afterward; do
not share it across runs. SDK-created lazy clients keep SDK ownership. Custom
`BaseLlm` implementations manage their own resources through their generators.

## Initial boundaries and bounds

One ordinary text `LlmAgent` and ordinary local function tools are supported.
Workflows, delegation/subagents, custom agent callbacks, dynamic instructions,
schemas/structured output, code execution, native retry/resume, long-running
tools, memory/artifacts, shared app/user state and live audio are outside this
profile. Unsupported modes fail explicitly. Use existing connector helpers
when a local tool needs Runkite connector policy.

Events are capped at 3 MiB of compact UTF-8 JSON, text receipts at 8 MiB and
native state at 16 MiB. History is not silently truncated. There is no additional
session database, credential vault, policy engine or Agent Engine deployment.

## Tests

```bash
make test-google-adk
make test-python-package
make test-managed-google-adk
```

Tests use the pinned SDK, including the actual Gemini/google-genai HTTP/SSE
codec over mocked transport and native span privacy checks. Installed-wheel
tests use a real plane and an installed CLI outside the source checkout. Set
both `RUNKITE_TEST_POSTGRES_DSN` and `RUNKITE_TEST_REDIS_URL` to dedicated test
services for the Postgres/Redis profile; otherwise they use SQLite/in-process.
These checks use no live provider and do not establish a multi-replica HA soak.
