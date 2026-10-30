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

Not in this first delivery: workflows, .NET, native approval/resume.

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

## Test

```bash
PYTHONPATH=python:python/adapters:examples/managed_agent_framework \
  python/adapters/agent_framework_adapter/.venv/bin/python \
  python/adapters/agent_framework_adapter/test_adapter.py
```
