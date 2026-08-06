# Runners

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

## Install (published packages)

Preview **0.1.0** (see [`VERSION`](../VERSION)):

```bash
# Python — LangGraph runner (+ importable SDK)
pip install runkite-runner==0.1.0
runkite-runner --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026

# TypeScript — LangGraph.js runner
npm install -g runkite-runner@0.1.0
runkite-runner --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

From a clone (dev), `PYTHONPATH=python python -m runkite_runner` and
`npx tsx src/cli.ts` still work. Framework adapters under
`python/adapters/` are not on PyPI yet — use `PYTHONPATH` as below.

Two runner SDKs today, both implementing the exact same Runner Protocol against the exact same Go control plane -- proof that the protocol is actually language-agnostic, not just designed to look that way on paper:

| | Python (`python/runkite_runner/`) | TypeScript (`typescript/runkite-runner/`) |
|---|---|---|
| Framework | LangGraph | LangGraph.js |
| `runner_kind` | `python-langgraph` (default) | `typescript-langgraphjs` |
| gRPC client | `grpcio` | `@grpc/grpc-js` + `@grpc/proto-loader` (dynamic proto loading, no codegen step) |
| Checkpoint direct mode | `AsyncPostgresSaver` | `PostgresSaver` (`@langchain/langgraph-checkpoint-postgres`) |
| Store dual mode | `RunkiteStore` (`BaseStore`) | `RunkiteStore` (`BaseStore`, same `batch()`-only abstract surface) |
| Dynamic graph loading | `importlib` | `tsx` (esbuild-based, no build step for agent code) |
| Custom routes (in-runner) | any ASGI app via `uvicorn` | any `(req, res) => void` handler -- covers plain `node:http` and Express directly; Koa needs `app.callback()` exported instead of `app` itself |

A run is routed to whichever runner declared the target agent: `langgraph.json`'s top-level `runner_kind` is stashed into that agent's metadata at bootstrap, and `createRun` reads it back to set `RunAssignment.runner_kind` -- not hardcoded, so a single control plane can serve a Python runner and a TypeScript runner side by side, each only ever receiving jobs for the agents its own config declared. Falls back to `python-langgraph` when an agent predates this field or its lookup fails, so every existing deployment keeps working unchanged.

### Concurrency (both runners)

By default a runner process (Python's `worker.py` and every `generic_worker`-based adapter, or the TypeScript runner) handles exactly one job at a time -- `--concurrency N` / `RUNKITE_CONCURRENCY=N` (default `1`, fully backward compatible) lets it dispatch up to `N` jobs concurrently instead, via a semaphore-bounded dispatcher (one `asyncio.Task` per job in Python; one un-awaited promise per job, tracked by a small hand-rolled `Semaphore` class, in TypeScript -- Node has no `asyncio.Semaphore` equivalent built in). The control plane needs zero changes for this: `GetJob`/`StreamEvents`/`WatchCancels` were already safe for multiple concurrent calls from one runner connection, and the job queue's dequeue is already atomic across concurrent callers. Setting `N` also sizes each runner's direct-mode Postgres connection pool (Python: `psycopg_pool.AsyncConnectionPool`, replacing a single shared connection behind a lock; TypeScript: `pg.Pool`'s own `max`, floored at node-postgres's default of 10 so a low `N` can't shrink it below that) for both the store and the checkpointer, so concurrent jobs' store/checkpoint I/O don't serialize on one connection either.

```bash
python -m runkite_runner --config langgraph.json --grpc-address localhost:50051 --concurrency 10

# TypeScript
npx tsx src/cli.ts --config langgraph.json --grpc-address localhost:50051 --concurrency 10
```

**What this actually helps, and what it doesn't**: genuinely effective for agents whose wall-clock time is dominated by *waiting* (slow LLM API calls, tool calls, external HTTP requests) -- many concurrent jobs can overlap productively since each spends most of its time not touching the CPU at all (proven for Python: 20 concurrent runs against the same static graph, zero cross-contamination, combined wall time ~14x faster than the sequential sum -- see `bench/REPORT.md`'s finding 1d; proven for TypeScript: 5 concurrent runs against `slow_agent_ts`'s 3-step, 2s-per-step graph all created and completed within the same 6-second window, not staggered 6s apart as concurrency=1 would produce). It does **not** let one process exceed one CPU core's worth of throughput for a CPU-bound or near-zero-compute agent (`asyncio` only overlaps I/O waiting, not CPU work, and Python's GIL means one process uses one core at a time; Node's single-threaded event loop has the same ceiling for CPU-bound work) -- confirmed via direct measurement in `bench/REPORT.md` for Python. For that case, run multiple runner processes (replicas) of the same `runner_kind` against the same control plane instead -- already fully supported, zero config changes, since the queue's dispatch is fair across any runner of that kind.

**CrewAI/AutoGen-specific caveat**: a shared `Crew` (CrewAI) or `AssistantAgent` (AutoGen) instance is not safe for concurrent invocation -- confirmed by reading each framework's own source: CrewAI's `kickoff`/`akickoff` write results onto shared instance attributes like `self.usage_metrics`; AutoGen's `AssistantAgent.run()` appends to a shared, mutable `model_context` (conversation history). Both adapters serialize concurrent calls on the same `graph_id` via a per-graph lock. AutoGen's adapter additionally `clear()`s that context before each run so sequential jobs on the same long-lived agent do not leak history across unrelated threads (LlamaIndex avoids the equivalent by reconstructing `chat_history` per call). CrewAI/AutoGen runs sharing a `graph_id` don't get real parallelism from `--concurrency`, only correctness -- LangGraph, LlamaIndex, and plain LangChain runs do get real parallelism.

```bash
cd typescript/runkite-runner
npm install
npx tsx src/cli.ts --config ../../examples/echo_agent_ts/langgraph.json --grpc-address localhost:50051
```

Same environment variables as the Python runner (`POSTGRES_DSN` for direct-mode checkpoint/store, `RUNKITE_GRPC_URL`/`RUNKITE_HTTP_URL`, `RUNNER_TOKEN`). `npm run build` compiles to `dist/` for production; `npx tsx` runs directly from TypeScript source for local development, matching the Python runner's zero-build-step DX.

Dockerized via `Dockerfile.runner-ts` (same zero-build-step pattern as `Dockerfile.runner`'s Python image -- `tsx` loads a user's `graph.ts` directly, no separate `tsc` compile). `docker-compose.yml`/`docker-compose.dev.yml` (the Python-focused stack) are unchanged; a standalone `docker-compose.ts.yml` demonstrates the TS runner instead, fully self-contained (SQLite + in-memory transport, no postgres/redis needed):

```bash
docker compose -f docker-compose.ts.yml up --build
```

Kept separate rather than merged into the main compose file because a single control-plane instance's `LANGGRAPH_CONFIG` binds to one `runner_kind` at a time (see `internal/config/loader.go`'s `RunnerKind` field) -- mixing Python and TS agents in one deployment means auto-discovering multiple `langgraph.json` files (leaving `LANGGRAPH_CONFIG` unset), not something this demo needed to prove the image itself works.

Live-verified end to end against a real control plane: the TypeScript runner dynamically loads a `.ts` graph, executes it with direct-mode Postgres checkpointing and store attached, streams events back, a cancel mid-execution correctly propagates through a real `WatchCancels` gRPC stream, and a full interrupt -> human approval -> `Command(resume)` -> completion round trip persists correctly across two separate runs on the same thread -- the exact same three validation gates already verified for the Python runner (VG-001/002/003), now proven independently in a second language. Happy-path TS × backend combinations are also regression-guarded by `make test-matrix` / the nightly Matrix workflow; cancel and HITL for TypeScript remain manual (matrix's cancel/HITL scenarios are LangGraph-Python example agents today).

### Framework Adapters

Four more Python runners (`python/adapters/{crewai_adapter,llamaindex_adapter,autogen_adapter,langchain_adapter}/`), each proving the control plane never assumed LangGraph -- built on a new shared, framework-agnostic loop (`runkite_runner.generic_worker`, extracted from but not replacing `worker.py`'s LangGraph-specific one) that handles only the gRPC polling/streaming/status-reporting mechanics. Each adapter is a thin translation layer implementing just two methods (`load_config`, `execute`) -- a small framework-adapter shim:

| | CrewAI | LlamaIndex | AutoGen | Plain LangChain |
|---|---|---|---|---|
| `runner_kind` | `python-crewai` | `python-llamaindex` | `python-autogen` | `python-langchain` |
| Loads | a `Crew` (`./crew.py:crew`) | a chat engine/agent (`./chat_engine.py:chat_engine`) | an `AssistantAgent` (`./agent.py:agent`) | any `Runnable` (`./chain.py:chain`) |
| Executes via | `crew.akickoff(inputs={"input": ...})` | `engine.achat(text, chat_history=...)` | `agent.run(task=...)` | `runnable.ainvoke({"input": ...})` |
| Venv | isolated (`python/adapters/crewai_adapter/.venv`) | isolated (`python/adapters/llamaindex_adapter/.venv`) | isolated (`python/adapters/autogen_adapter/.venv`) | shared `python/.venv` (`langchain-core` already a dependency) |
| Example | `examples/crewai_agent/` | `examples/llamaindex_agent/` | `examples/autogen_agent/` | `examples/langchain_agent/` |

All four examples are offline and deterministic (a hand-written fake LLM/model-client subclass returning a fixed response) -- no API key needed, same convention as `examples/vector_agent`'s fake embeddings. Input/output convention matches the LangGraph runner: extract the last human message's text from `RunAssignment.input.messages`, invoke the framework, append the reply as `{"role": "ai", "content": ...}` -- so client code built against one `runner_kind` doesn't need to change to talk to another.

Cancellation is wired into all four via `generic_worker.run_cancellable` -- each adapter races its single framework call (`akickoff`/`achat`/`run`/`ainvoke`) against `cancel_event`, calling `.cancel()` on the underlying task and reporting `interrupted` (not `error`) if the cancel wins, the same outcome a cancelled LangGraph run reports. Live-verified against a real gRPC `WatchCancels` signal, not just a unit-test mock.

**Why CrewAI, LlamaIndex, and AutoGen get their own isolated venv** (plain LangChain doesn't need one): confirmed live during development -- installing `crewai` into the shared `python/.venv` silently downgraded `protobuf`, a dependency the production LangGraph runner's generated gRPC stubs are version-sensitive about (see the Runner Protocol section's protobuf note). AutoGen's own dependencies didn't conflict during development, but an isolated venv is kept anyway for consistency and future-proofing. Real deployments would run each framework's runner as a genuinely separate process anyway (arguably a separate container), so an isolated venv here matches that reality rather than fighting it. Setup:

```bash
cd python/adapters/crewai_adapter        # or llamaindex_adapter / autogen_adapter
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python crewai grpcio protobuf httpx   # or: llama-index-core ... / autogen-agentchat ... (+ httpx)

cd ../../../examples/crewai_agent
PYTHONPATH=<repo>/python:<repo>/python/adapters \
  <repo>/python/adapters/crewai_adapter/.venv/bin/python -m crewai_adapter \
  --config langgraph.json --grpc-address localhost:50051
```

Live-verified end to end for all four: a real control plane, a real runner process for each framework, a real `POST /threads/{id}/runs` request through to a real thread-values response containing that framework's actual output. `make test-adapters` runs the CrewAI/LlamaIndex/AutoGen unit tests once their venvs exist (`test_generic_worker.py`/`test_langchain_adapter.py` cover the shared loop and plain LangChain respectively, and run in CI/`make test-python` alongside the rest of the Python suite -- CrewAI/LlamaIndex/AutoGen's isolated-venv tests run in a dedicated CI step instead, for the same shared-venv-isolation reason above). Plain LangChain additionally has automated, PR-CI-gated end-to-end coverage (`test/e2e/adapters/`, part of `make test-e2e`) -- a real control plane dispatching to a real `langchain_adapter` runner subprocess over real gRPC, including cancellation via a real `WatchCancels` signal. Happy-path coverage for CrewAI/LlamaIndex/AutoGen (and LangChain/LangGraph/TS) across every state+transport cell is the nightly `make test-matrix` / Matrix workflow. CrewAI/LlamaIndex/AutoGen don't have the equivalent PR-CI e2e tier yet (their isolated venvs make it more setup than LangChain's shared one) -- live-verified manually during development, same as before, just not CI-gated.
