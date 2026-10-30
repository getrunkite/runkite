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

```bash
cd python/adapters/agent_framework_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python \
  'agent-framework-core==1.23.0' grpcio protobuf httpx
```

## Run

Register an agent with runner kind `python-agent-framework`, start the plane,
then:

```bash
cd examples/managed_agent_framework
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/agent_framework_adapter/.venv/bin/python -m agent_framework_adapter \
  --config langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026 \
  --runner-kind python-agent-framework
```

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
PYTHONPATH=python:python/adapters:examples/managed_agent_framework \
  python/adapters/agent_framework_adapter/.venv/bin/python \
  -m unittest discover -s python/adapters/agent_framework_adapter -p 'test_*.py'
```

`make test-managed-agent-framework` builds a real plane and a wheel of the shared
runner core, then runs authenticated dispatch, streaming, Admin visibility,
restart, tenant isolation, cancellation usage and crash-after-commit recovery.
The Microsoft adapter itself still loads from this checkout.

By default the live test uses SQLite and the in-process queue. Set both
`RUNKITE_TEST_POSTGRES_DSN` and `RUNKITE_TEST_REDIS_URL` to dedicated disposable
services to exercise Postgres and Redis. This single-plane test is not a
multi-replica HA soak. It uses an offline native model and no provider secrets.
