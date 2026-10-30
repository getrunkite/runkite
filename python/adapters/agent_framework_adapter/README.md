# Managed Microsoft Agent Framework adapter

Runs local Microsoft Agent Framework `Agent` factories through Runkite dispatch
so they appear in Admin Threads/Runs. This is a managed runner, not the
decision-only [governance middleware](../../../examples/governance_agent_framework/README.md).

Native session history and committed-turn receipts use run-bound opaque
checkpoints with CAS (If-Match / If-None-Match), matching the managed AI SDK
reclaim safeguards.

## Scope

- Agent factories, text streaming, ordinary local tools
- Durable `AgentSession` across runner restarts
- Cancellation, usage metering, tenant isolation via the generic worker
- CAS receipt recovery for the same `run_id`
- Offline example under [`examples/managed_agent_framework`](../../../examples/managed_agent_framework)

Not in this first delivery: workflows, .NET, native approval/resume, non-text
input or caller-supplied assistant/tool history. Native approval requests and
resume commands fail explicitly rather than becoming successful runs. Native
background continuation and replacement text updates are also unsupported.

## Setup

**Unreleased packaging:** this adapter is included in the source-built
`runkite-runner` wheel and sdist, with an optional `agent-framework` extra pinned
to `agent-framework-core==1.23.0`. The published PyPI `0.5.8` artifact does not
contain this adapter, extra or CLI. From the repository root, install the local
package into a clean environment:

```bash
python3.12 -m venv .venv-agent-framework
.venv-agent-framework/bin/python -m pip install './python[agent-framework]'
```

This builds and installs the runner wheel, including `agent_framework_adapter`.
No `PYTHONPATH` is needed to run it. Python 3.11+ is supported; this example uses
3.12. After a release containing this change is published, the equivalent PyPI
install will be `pip install 'runkite-runner[agent-framework]'`.

## Run

Register an agent with runner kind `python-agent-framework`, start the plane,
then:

```bash
.venv-agent-framework/bin/runkite-runner-agent-framework \
  --config examples/managed_agent_framework/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026 \
  --runner-kind python-agent-framework
```

The installed module alternative is
`.venv-agent-framework/bin/python -m agent_framework_adapter` with the same
arguments. The ordinary `runkite-runner` CLI still starts LangGraph. The stock
Python runner image does not install the Microsoft extra and keeps its LangGraph
entrypoint; build your own image or use the environment above for this profile.

`graphs` entries must export a factory `(AgentFrameworkRunContext) -> Agent`.
The plane must load the same config with `runner_kind: python-agent-framework`.
Use `RUNNER_TOKEN` for both gRPC and run-bound checkpoint HTTP authentication;
the plane's kind token and tenant allowlist must match.

Send only this turn's new user/system text messages, ending with a user message.
The native session supplies prior history. Cancelled and failed turns leave the
last committed session intact and retain any usage observed before termination.
Receipt replay skips the factory, model and tools, and reports the replacement
attempt's generation. It does not make effects before a state commit exactly
once. Give external writes their own idempotency keys.

Observed usage includes a model only when the contributing requests identify
one consistent model. The plane applies its configured pricebook to estimate
USD and uses those recorded estimates in its existing spend limits. Unknown or
mixed model identities remain unpriced; token totals are still recorded and a
configured pricebook can raise the existing `usage_unpriced` alert. Attribution
survives cancellation, provider errors and committed-turn replay. Dollar
estimates are not provider invoices, and cancellation cannot recover usage that
the plane has not received.

Attribution uses the SDK's public per-provider chat middleware. Custom clients
without that boundary keep token accounting and remain unpriced. A single
model name on a final response cannot identify the models behind cumulative
usage across tool rounds.
Existing receipts without a model remain replayable and unpriced; this change
does not infer historical model identities or reprice stored usage.

State reads fail closed on missing CAS metadata, a different framework or graph,
or an invalid native session/receipt. Output events have a 3 MiB safety limit
below the default gRPC receive cap. The text receipt is capped at 8 MiB and
native state at 16 MiB. Bounds are checked before a success receipt is saved so
a committed turn remains deliverable over the Runner Protocol. History is not
silently truncated; use a new thread when a conversation reaches these limits.
Existing partial checkpoints from the earlier preview, or checkpoints containing
native approval requests, are rejected rather than silently resumed. Keep that
thread for inspection and start a new one; this adapter does not migrate
unfinished native work into a successful receipt.

## Test

```bash
make test-agent-framework
make test-managed-agent-framework
```

`make test-agent-framework` prepares an isolated test environment and runs the
adapter's unit tests. `make test-managed-agent-framework` builds a real plane
and installs the runner wheel, including this adapter, into a clean environment.
It runs authenticated dispatch, streaming, Admin visibility,
restart, tenant isolation, cancellation usage and crash-after-commit recovery.
It also checks fictional pricebook estimates, provider-error usage and a later
run-create rejection after a configured USD cap is reached.
Unit tests include the actual Microsoft OpenAI provider clients over mocked
HTTP/SSE. That test environment installs `agent-framework-openai==1.17.0` with
the pinned core; the runtime extra remains core-only so applications choose
their own provider package.
The worker loads both the shared core and Microsoft adapter from that installed
wheel, with no `PYTHONPATH` injection.

By default the live test uses SQLite and the in-process queue. Set both
`RUNKITE_TEST_POSTGRES_DSN` and `RUNKITE_TEST_REDIS_URL` to dedicated disposable
services to exercise Postgres and Redis. This single-plane test is not a
multi-replica HA soak. It uses an offline native model and no provider secrets.
