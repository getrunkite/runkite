# Managed Microsoft Agent Framework example

Offline factory for the managed Python runner. No provider credentials. The
runner kind is `python-agent-framework`. Creates ordinary Runkite Threads/Runs
through the control plane.

This is separate from [governance middleware](../governance_agent_framework/README.md),
which only submits tool decisions while the app keeps the loop.

## Run

```bash
cd python/adapters/agent_framework_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python \
  'agent-framework-core==1.23.0' grpcio protobuf httpx

cd ../../examples/managed_agent_framework
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/agent_framework_adapter/.venv/bin/python -m agent_framework_adapter \
  --config langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

Create runs against graph id `managed_agent_framework` with `input.messages`
containing the new user text. Session history and turn receipts survive runner
restarts via opaque checkpoints. Reclaim of the same `run_id` replays without
another model or tool cycle.

## Tests

See the adapter README. `make test-agent-framework` from the repo root once the
adapter venv exists.
