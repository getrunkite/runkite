# Quickstart

End-to-end walkthrough: build the control plane, run an agent, stream results via SDK.

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | 1.25+ | Build the control plane |
| Python | 3.12+ | Run agents |
| Docker | Any recent | Optional, for Postgres/Redis |
| uv | Latest | Python package manager (recommended) |

## 1. Clone and Build

```bash
git clone https://github.com/getrunkite/runkite
cd runkite
make build
```

This produces a `./runkite` binary in the project root.

## 2. Create a Simple Agent

Create a file `my_agent/graph.py`:

```python
from typing import Annotated, TypedDict
from langgraph.graph import StateGraph, START, END


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


def echo_node(state: State) -> State:
    last_message = state["messages"][-1]
    return {
        "messages": [{"role": "ai", "content": f"Echo: {last_message['content']}"}]
    }


builder = StateGraph(State)
builder.add_node("echo", echo_node)
builder.add_edge(START, "echo")
builder.add_edge("echo", END)

graph = builder.compile()
```

## 3. Create langgraph.json

In the same directory as `my_agent/`, create `langgraph.json`:

```json
{
  "graphs": {
    "echo_agent": "./my_agent/graph.py:graph"
  },
  "dependencies": ["./my_agent"]
}
```

The format is `"<graph_id>": "<path>:<symbol>"`. The path is relative to the JSON file. The symbol is the compiled graph variable in that module.

## 4. Start the Control Plane

```bash
./runkite dev
```

The `dev` command auto-discovers `langgraph.json` in the current directory. Output:

```
  Runkite Control Plane (dev)
  HTTP API:    http://localhost:2026
  gRPC bridge: localhost:50051
  Admin UI:    http://localhost:2026/admin/
  Health:      http://localhost:2026/health
  Metrics:     http://localhost:2026/metrics

INF state store: sqlite path=./runkite.db
INF transport: in-memory
INF registered agent graph_id=echo_agent source=./langgraph.json
INF gRPC bridge listening port=50051
INF control plane starting http_port=2026 grpc_port=50051
```

Verify it's running:
```bash
curl http://localhost:2026/health
# {"status":"ok"}

curl -s http://localhost:2026/agents/echo_agent | python -m json.tool
# {
#   "agent_id": "echo_agent",
#   "name": "echo_agent",
#   "description": "Graph loaded from ./langgraph.json (./my_agent/graph.py:graph)",
#   ...
# }
```

## 5. Start the Python Runner

In a second terminal:

```bash
cd runkite

# Option A: from PyPI
pip install runkite-runner
runkite-runner --config langgraph.json

# Option B: from this clone (editable install, picks up local changes)
pip install -e python/
python -m runkite_runner --config langgraph.json
```

Either installs LangGraph, gRPC, and the other runner dependencies declared
in `python/pyproject.toml` -- `PYTHONPATH=python python -m runkite_runner`
without one of these first will fail with `ModuleNotFoundError` the moment
it tries to import LangGraph.

The runner connects to the control plane's gRPC bridge and waits for jobs.

## 6. Use the SDK

Install the LangGraph SDK (works with Runkite since it implements the same Agent Protocol). There is no separate Runkite client package yet — see [`docs/client-sdk.md`](client-sdk.md) for curl/OpenAPI usage and the deferred first-party SDK plan.

```bash
pip install langgraph-sdk
```

### Streaming example

```python
import asyncio
from langgraph_sdk import get_client


async def main():
    client = get_client(url="http://localhost:2026")

    # Create a thread
    thread = await client.threads.create()
    print(f"Thread: {thread['thread_id']}")

    # Stream a run
    async for event in client.runs.stream(
        thread["thread_id"],
        "echo_agent",
        input={"messages": [{"role": "user", "content": "Hello, Runkite!"}]},
    ):
        if event.event == "values":
            messages = event.data.get("messages", [])
            for msg in messages:
                print(f"  [{msg['role']}] {msg['content']}")


asyncio.run(main())
```

Expected output:
```
Thread: 550e8400-e29b-41d4-a716-446655440000
  [user] Hello, Runkite!
  [ai] Echo: Hello, Runkite!
```

### Wait (non-streaming) example

```python
async def main():
    client = get_client(url="http://localhost:2026")
    thread = await client.threads.create()

    # Wait for completion (blocks until done)
    result = await client.runs.wait(
        thread["thread_id"],
        "echo_agent",
        input={"messages": [{"role": "user", "content": "ping"}]},
    )
    print(result)


asyncio.run(main())
```

### HITL (Human-in-the-Loop) example

Using the `approval_agent` example (which uses `langgraph.types.interrupt()`):

```python
async def main():
    client = get_client(url="http://localhost:2026")
    thread = await client.threads.create()

    # Start the run -- it will interrupt waiting for approval
    run = await client.runs.wait(
        thread["thread_id"],
        "approval_agent",
        input={"messages": [{"role": "user", "content": "send the email"}]},
    )
    # Run status will be "interrupted"
    print(f"Status: {run['status']}")

    # Resume with approval
    resumed = await client.runs.wait(
        thread["thread_id"],
        "approval_agent",
        input=None,
        command={"resume": True},
    )
    print(f"Final: {resumed}")


asyncio.run(main())
```

## 7. Verify with curl

You can also interact directly with the HTTP API:

```bash
# Create a thread
THREAD=$(curl -s -X POST http://localhost:2026/threads | python -c "import sys,json; print(json.load(sys.stdin)['thread_id'])")

# Create a run and wait for result
curl -s -X POST "http://localhost:2026/threads/$THREAD/runs/wait" \
  -H "Content-Type: application/json" \
  -d '{
    "agent_id": "echo_agent",
    "input": {"messages": [{"role": "user", "content": "hello from curl"}]}
  }' | python -m json.tool
```

## Next Steps

### Enable Postgres (persistent state across restarts)

```bash
export POSTGRES_DSN="postgres://user:pass@localhost:5432/runkite?sslmode=disable"
./runkite db upgrade    # create tables
./runkite serve --config langgraph.json
```

### Enable Redis (scalable transport for multiple runners)

```bash
export REDIS_URL="redis://localhost:6379"
./runkite serve --config langgraph.json
```

### Add authentication

In `langgraph.json`:
```json
{
  "auth": {
    "type": "api_key",
    "strict_permissions": true,
    "keys": {
      "sk-my-secret-key-1": {"name": "client-1", "permissions": ["read", "write"]},
      "sk-my-secret-key-2": {"name": "client-2", "permissions": ["read"]}
    }
  }
}
```
`keys` maps each API key string to its identity and permissions -- not a plain array.

Then pass the key in requests:
```bash
curl -H "Authorization: Bearer sk-my-secret-key-1" http://localhost:2026/agents/echo_agent
```

### Add a connector

Create `connectors/github.yaml`:
```yaml
auth:
  type: bearer
  bearer_token: ${GITHUB_TOKEN}
```

Reference it in `langgraph.json`:
```json
{
  "connectors": {
    "github": { "config_ref": "./connectors/github.yaml" }
  }
}
```

Runners can then call `POST /internal/connectors/github/session` to get the token without storing it themselves.

### Run with production infrastructure

For the whole system (control plane + runner + Postgres + Redis, all containerized), use the full-stack compose file instead of building/running manually:

```bash
docker compose up -d --build
curl http://localhost:2026/health
```

To run the binary yourself against your own Postgres/Redis instead:

```bash
export POSTGRES_DSN="postgres://user:pass@localhost:5432/runkite?sslmode=disable"
export REDIS_URL="redis://localhost:6379"
./runkite db upgrade
./runkite serve --config langgraph.json
```

(`docker-compose.test.yml` also starts Postgres + Redis, on non-standard ports 5433/6380 -- that one is meant for running the test suite, not for serving traffic, but works fine for either.)

### Secure runner communication

Set runner tokens for production (prevents unauthorized processes from dequeuing jobs):

```bash
export RUNNER_TOKEN_python_langgraph="your-secret-token"
./runkite serve --config langgraph.json
```

The runner must present the matching token on every gRPC call.
