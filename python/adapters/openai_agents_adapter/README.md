# Managed OpenAI Agents SDK runner

Runs public Python `agents.Agent` factories through Runkite dispatch. The
runner kind is `python-openai-agents`; executions appear in Admin Threads/Runs.
The SDK runs in this Python worker. The Go plane uses the existing Runner
Protocol and HTTP checkpoint surface.

## Install and run

This profile is **unreleased**. The published PyPI `0.5.8` artifact does not
include it. From the repository root, install the source package into a clean
Python 3.11+ environment (shown with 3.12):

```bash
python3.12 -m venv .venv-openai-agents
.venv-openai-agents/bin/python -m pip install './python[openai-agents]'
.venv-openai-agents/bin/runkite-runner-openai-agents \
  --config examples/managed_openai_agents/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

The extra pins `openai-agents==0.24.0`. The installed module alternative is
`python -m openai_agents_adapter` with the same arguments. Neither entrypoint
needs repository `PYTHONPATH`. Once a release contains this profile, the public
install will be `pip install 'runkite-runner[openai-agents]'`. The ordinary
`runkite-runner` command and stock Python image retain their LangGraph defaults.
The stock image does not install this optional SDK.

Load the same config in the plane, including
`runner_kind: python-openai-agents`. Each graph export is a per-run factory
returning a fresh `agents.Agent`. An explicit `Model` returned by that factory
is owned by this run and closed afterward; do not share it across jobs. Model
name strings retain the SDK's normal provider-client ownership. The example uses
an offline public SDK `Model` implementation and a local tool, with no provider
credentials.

For authenticated deployments, use `RUNNER_TOKEN` on the worker and matching
`RUNNER_TOKEN_PYTHON_OPENAI_AGENTS` and `RUNNER_TENANTS_PYTHON_OPENAI_AGENTS`
on the plane. The worker uses one credential for gRPC and run-bound checkpoint
HTTP. Opaque state is required; there is no single-shot fallback on read failure.

## Execution and durability

Send only this turn's new user/system text through `input.messages`, ending with
a nonempty user message. The input object contains only `messages`. Prior history
belongs to the runner. Native input items, function-call results and reasoning
items are preserved in successful-turn checkpoints. UI output contains ordinary
user/assistant text; reasoning items are not rendered as assistant text.

State and successful-turn receipts use conditional writes through
`/internal/checkpoints/{thread}/adapter-state`. Reads validate the HTTP framework,
ETag, graph, native history and receipt before agent execution. The same `run_id`
can replay a committed receipt in a replacement generation without rerunning its
factory, model or tools. State commits before terminal success.

Failed or interrupted turns retain the previous committed history. Reported
usage is published during execution and retained on errors/cancellation when
the plane received it before terminal completion. Cancellation is cooperative
and cannot undo completed external effects. A crash before state commit can
repeat work; use downstream idempotency for external writes.

Output events are capped at 3 MiB of compact UTF-8 JSON below the default gRPC
receive limit. Text receipts are capped at 8 MiB and native state at 16 MiB.
Deterministic schema or size failures stop before a success checkpoint is sent.
History is not silently truncated. Native checkpoints can contain conversation, tool and
reasoning data, so apply your normal database access and retention controls.

## Initial boundaries

- New text turns, a single agent and ordinary local `FunctionTool` execution.
- Native function names are checked against a supplied `allowed_tools` list
  before their bodies run. This is cooperative enforcement, not a sandbox.
- Local tools acquire connector grants, secret injection and connector audit
  only when they use Runkite's existing connector helpers. An arbitrary Python
  function does not automatically acquire connector policy or HITL.
  A pending connector decision blocks that operation; operator approval alone
  does not resume this SDK turn.
- Native approval/resume, unfinished-turn restoration, handoffs,
  agents-as-tools, MCP/hosted tools, deferred loading, structured final outputs
  and non-text inputs are outside this profile. Unsupported modes fail
  explicitly instead of committing a successful receipt.
- Native SDK trace export is disabled by this profile. The generic worker's
  existing OTLP run span remains available through `OTEL_EXPORTER_OTLP_*`.
- Usage carries a model ID when completed requests identify one consistent
  model. The plane can apply its configured pricebook. Unknown or mixed model
  identities remain unpriced rather than attributing all tokens to one model;
  token accounting still applies. Dollar estimates are not provider invoices.
- The native SDK's default turn limit applies. Limit and guardrail failures
  retain observed usage and the previous committed history. A native model
  refusal follows the SDK's error semantics and is not an empty successful turn.
- This profile uses the in-process Agents SDK, not OpenAI-hosted Agents API
  sessions or an additional session service.

## Tests

From the repository root:

```bash
make test-openai-agents
make test-python-package
make test-managed-openai-agents
```

Unit tests use the actual pinned SDK with an offline model. The installed-wheel
test creates a fresh consumer environment outside the checkout and exercises a
real plane and installed CLI. Set both `RUNKITE_TEST_POSTGRES_DSN` and
`RUNKITE_TEST_REDIS_URL` to dedicated test services for the Postgres/Redis profile;
otherwise it uses SQLite and the in-process queue. These checks do not establish
a multi-replica HA soak or validate an external model provider.
