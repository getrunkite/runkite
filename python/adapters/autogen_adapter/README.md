# AutoGen Adapter

Runs Microsoft AutoGen (`autogen-agentchat`) agents against the Runkite
Runner Protocol -- proves the control plane is genuinely framework-agnostic,
not LangGraph-specific. See [`adapter.py`](adapter.py)'s module docstring for
the config/input/output conventions, and
[`../../../examples/autogen_agent`](../../../examples/autogen_agent) for a
working (offline, no API key needed) example.

## Why this has its own venv

Same reasoning as the CrewAI/LlamaIndex adapters: keeping each framework's
independent dependency tree out of the shared `python/.venv` the LangGraph
runner and its examples use. AutoGen's own dependencies (`autogen-core`,
`autogen-agentchat`) didn't conflict with anything during development, but an
isolated venv is kept anyway for consistency with the other adapters and to
stay protected against future version drift.

## Setup

```bash
cd python/adapters/autogen_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python autogen-agentchat grpcio protobuf httpx
```

## Run

```bash
cd examples/autogen_agent
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/autogen_adapter/.venv/bin/python -m autogen_adapter \
  --config langgraph.json --grpc-address localhost:50051
```

## Test

```bash
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  python/adapters/autogen_adapter/.venv/bin/python python/adapters/autogen_adapter/test_adapter.py
```

Or `make test-adapters` from the repo root once the venv exists.
