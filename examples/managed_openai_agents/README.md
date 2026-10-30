# Managed OpenAI Agents SDK example

An offline public `agents.Agent` factory for `python-openai-agents`. Runkite
creates real Threads/Runs, dispatches this factory to a Python runner and records
its streamed answer and reported usage. No provider key is required.

## Run

The optional install profile is **unreleased** and absent from the published
PyPI `0.5.8` artifact. From the repository root:

```bash
python3.12 -m venv .venv-openai-agents
.venv-openai-agents/bin/python -m pip install './python[openai-agents]'
.venv-openai-agents/bin/runkite-runner-openai-agents \
  --config examples/managed_openai_agents/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

Load this config in the plane too. Its `runner_kind: python-openai-agents`
selects the correct worker. The installed module alternative is
`.venv-openai-agents/bin/python -m openai_agents_adapter`; no source `PYTHONPATH`
is needed. The extra pins `openai-agents==0.24.0`.

Create runs for graph `managed_openai_agents`, sending new user/system text
through `input.messages` and ending with a nonempty user message. The input
object contains only `messages`. Send only the new turn, without resending the whole
conversation. Native history and committed-turn receipts survive worker
restarts through the plane's HTTP opaque checkpoint API. Reclaim of the same
committed run replays its receipt without another model/tool cycle.

For authentication, set `RUNNER_TOKEN` in the worker and matching
`RUNNER_TOKEN_PYTHON_OPENAI_AGENTS` and `RUNNER_TENANTS_PYTHON_OPENAI_AGENTS`
on the plane. Both gRPC and checkpoint HTTP use that worker identity.

## Scope

The example demonstrates text and an ordinary local function tool. It uses the
native SDK loop inside the runner. Replace its offline model with your provider
model in your own factory. Native hosted tools, approvals/resume, handoffs,
agents-as-tools, structured final output and non-text input are unsupported in
this profile. The application still owns UI authentication and presentation
history; this example does not implement the AI SDK UI stream protocol.

Only successful turns update native history. Interrupted or failed turns retain
the previous committed checkpoint and report usage already observed. External
effects before a state commit still need downstream idempotency. Local tools do
not automatically gain connector grants or HITL. Native SDK trace export is
disabled; existing Runkite OTLP run tracing remains available.

After a release includes this change, install with
`pip install 'runkite-runner[openai-agents]'`. The stock Python runner image
continues to use its LangGraph defaults and does not install this extra.

## Tests

`make test-openai-agents` exercises the pinned SDK without a provider.
`make test-managed-openai-agents` runs a real plane and a fresh installed-wheel
worker. See the [adapter README](../../python/adapters/openai_agents_adapter/README.md)
for persistence, recovery and backend-test boundaries.
