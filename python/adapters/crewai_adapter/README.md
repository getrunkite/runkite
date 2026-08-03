# CrewAI Adapter

Runs CrewAI Crews against the Runkite Runner Protocol -- proves the control
plane is genuinely framework-agnostic, not LangGraph-specific. See
[`adapter.py`](adapter.py)'s module docstring for the config/input/output
conventions, and [`../../../examples/crewai_agent`](../../../examples/crewai_agent)
for a working (offline, no API key needed) example.

## Why this has its own venv

CrewAI is a heavy, independent dependency tree (own `pydantic`/`protobuf`
version constraints, many transitive packages) that must never be installed
into the shared `python/.venv` the LangGraph runner and its examples use --
confirmed live during development: a stray `pip install crewai` into the
shared venv downgraded `protobuf` there. Real deployments would run this as
an entirely separate process anyway (possibly a separate container), so an
isolated venv here matches that reality rather than fighting it.

## Setup

```bash
cd python/adapters/crewai_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python crewai grpcio protobuf httpx
```

## Run

```bash
cd examples/crewai_agent
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/crewai_adapter/.venv/bin/python -m crewai_adapter \
  --config langgraph.json --grpc-address localhost:50051
```

## Test

```bash
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  python/adapters/crewai_adapter/.venv/bin/python python/adapters/crewai_adapter/test_adapter.py
```

Or `make test-adapters` from the repo root once the venv exists.
