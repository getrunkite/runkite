# LlamaIndex Adapter

Runs LlamaIndex chat engines/agents against the Runkite Runner Protocol.
See [`adapter.py`](adapter.py)'s module docstring for the config/input/output
conventions (including why chat history is reconstructed per-call rather
than relying on the engine's own internal memory), and
[`../../../examples/llamaindex_agent`](../../../examples/llamaindex_agent)
for a working (offline, no API key needed) example.

## Why this has its own venv

Same reasoning as `crewai_adapter`'s README -- an independent, heavy
dependency tree that must never be installed into the shared
`python/.venv`. `llama-index-core` (not the full `llama-index` meta-package)
is used specifically to avoid pulling in a cloud LLM SDK by default.

## Setup

```bash
cd python/adapters/llamaindex_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python llama-index-core grpcio protobuf httpx \
  opentelemetry-api opentelemetry-sdk
```

## Run

```bash
cd examples/llamaindex_agent
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/llamaindex_adapter/.venv/bin/python -m llamaindex_adapter \
  --config langgraph.json --grpc-address localhost:50051
```

## Test

```bash
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  python/adapters/llamaindex_adapter/.venv/bin/python python/adapters/llamaindex_adapter/test_adapter.py
```

Or `make test-adapters` from the repo root once the venv exists.
