# Runners

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

The v0.6.0 heartbeat update bounds each RPC to one second by default. A
hung request cannot block later renewals. Stopping a TypeScript loop cancels its
active call and suppresses late superseded callbacks. The normal heartbeat cadence
and reclaim age remain independent; partitions can still cause reclaim. Connector
helpers now use one operation deadline and explicit safe-renewal reasons, and
credential-bearing HTTP clients refuse redirects. See
[Connectors](connectors.md#deadlines-cancellation-and-retry-safety).

## Identity and native checkpoint guard (v0.6.0)

The pre-execution status lookup now uses the assignment tenant and encodes the
run ID as one URL component. Empty tenant still selects `default`; nonempty
identities are preserved exactly. Raw printable ASCII headers remain compatible.
Unicode and legacy header-unsafe identities use `X-Runkite-Run-Id-B64` or
`X-Runkite-Tenant-Id-B64` with unpadded base64url UTF-8, omitting the corresponding
raw header. Upgrade the plane before runners that send these representations.
The status lookup remains advisory; this does not change its failure policy.

**Direct Postgres LangGraph now permits only the default tenant with thread IDs
without `:`.** Every named tenant is refused before graph factory, state access
or execution, even if its new identity is delimiter-safe: a legacy default thread
such as `acme:target` could otherwise be read by named tenant `acme` / `target`.
There is no trusted ownership marker on those old saver rows. Named LangGraph
tenants must use HTTP proxy checkpoints and explicitly resolve prior native
history. Safe default direct keys remain reachable without renaming.

Fresh shared memory still permits named tenants under its delimiter guard:
default-tenant thread IDs cannot contain `:`, named tenant IDs cannot contain `:`,
and tenant IDs cannot have outer whitespace or ASCII controls. Named-tenant memory
thread IDs may contain colons. Proxy checkpoints support all tenants without
these native saver restrictions. Managed AI SDK, Microsoft, OpenAI Agents,
Pydantic AI and Google ADK already use opaque HTTP state and are unaffected.

Default cron uses `cron:<name>` threads and must use proxy checkpoints too.
Unset runner `POSTGRES_DSN` and set `RUNKITE_HTTP_URL`. Switching modes imports no
native history and does not repair previously mixed data. Resolve ownership and
continuity explicitly before continuing; no blank-namespace or memory fallback
is automatic. See [upgrade guidance](ops-runbook.md#follow-up-identity-and-completion-upgrade)
and [engineering rationale](internal/atomic-results-and-identities.md).

The bridge also verifies the actual dequeued assignment's kind before advancing
its durable attempt or delivering input. Authenticated cross-kind misroutes are
rejected and generation-Nacked; trusted no-token local behavior is unchanged.
For NATS, choose kinds with distinct derived durable names: `.` maps to `_`, so
`a.b` and `a_b` conflict. An existing durable with a different filter now produces
an error instead of being retargeted. Reconcile queued work before a consistent
kind rename; no automatic consumer/history migration occurs.

In-process return/reclaim also keeps the original lease when its ready buffer is
full or the caller is already cancelled. These paths fail without blocking or
advancing a generation for an unqueued assignment, allowing normal later retry.
Memory still does not survive process exit.

## Install (published packages)


```bash
# Python — LangGraph runner (+ importable SDK)
pip install runkite-runner
runkite-runner --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026

# TypeScript — LangGraph.js runner
npm install -g runkite-runner
runkite-runner --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

Pin with `==` / `@` when you need a fixed release. From a clone (dev),
`pip install -e python/` then `python -m runkite_runner` works the same way
(`PYTHONPATH=python python -m runkite_runner` also works once dependencies
are installed some other way, e.g. `uv pip install -e python/`); TypeScript's
`npx tsx src/cli.ts` needs `npm ci` in `typescript/runkite-runner/` first.
CrewAI, LlamaIndex, AutoGen and plain LangChain adapters under `python/adapters/`
remain checkout-only. Use `PYTHONPATH` as below after installing each adapter's
own dependencies. The Microsoft adapter has an v0.6.0 optional install
profile in the same Python runner package, described below.

The plane does not proxy model HTTP and does not pick models. Put an LLM gateway in front of the runner if you want per-call model routing.

The published Python and TypeScript runner runtimes implement the same Runner
Protocol. The install commands above describe their LangGraph defaults. The
managed AI SDK backend described below ships in `runkite-runner` from `v0.5.8`
(`--framework ai-sdk`, optional peer `ai@7.0.155`).

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

### Dispatch generations and completion delivery

Every run-specific RPC and bound `/internal/*` request must echo the exact
`generation` from `GetJob`. A new run starts at generation `1`; reclaim increments
it. Zero is an ordinary legacy value, never a wildcard for a positive attempt.
An older runner that omits generation can act only on an actual generation-zero
assignment and stops matching after reclaim. Upgrade custom runners to preserve
this field. Both shipped runners already propagate it.

The runner waits for event-stream acknowledgement before reporting completion.
If stream delivery fails or its acknowledgement is lost, completion stays
unconfirmed and the lease remains recoverable. The runner must not invent an
agent `error` result solely because delivery failed. ReportStatus delivery can
be retried with the same attempt and result, without rerunning the agent. A
confirmed terminal stream can be recovered by the plane if the final status
report was interrupted, including at the reclaim generation ceiling.

At that ceiling, the plane retains the assignment at its current generation and
does not re-enqueue it. It retries durable completion and finalization before
acknowledging the lease. A matching delivered terminal is recovered first;
otherwise the plane attempts an `error` result. A same-attempt late ReportStatus
can race that fallback, and the first durable terminal transaction wins. A
terminal arriving after the recovery check is not guaranteed to win. NATS keeps
the exhausted delivery leased with `+WPI`; any native redelivery still passes the
existing generation fence and completion recovery before execution. See
[Runner Protocol](../runner-protocol/PROTOCOL.md).

### Live streaming during reclaim (v0.6.0)

Run SSE, thread SSE and WebSocket streams confirm terminal events through the
existing durable completion fence. A delayed terminal from a superseded attempt
does not end the stream while its replacement runs. If the broker subscription
closes early, the stream checks retained replay and durable state for the current
result. Normal delivery still uses the existing push subscription. Sequence
cursors apply to the initial attempt; a replacement can restart its sequence
without having its events hidden by the previous attempt's cursor.

Event IDs preserve the attempt boundary. Raw nonterminal events remain advisory
because generation checking and broker publication are separate operations.
Clients cannot undo old text already displayed, so they must detect attempt
changes and avoid merging output across generations. The managed AI SDK bridge
keeps that safeguard. Retained history and request cancellation still bound live
recovery; `/wait` and persisted output remain authoritative. This adds no new
external-effect idempotency or exactly-once guarantee.

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

### Managed AI SDK TypeScript runner

The optional `typescript-ai-sdk` backend runs native Vercel AI SDK agents as
Runkite-managed runs. It uses a per-run factory returning public SDK settings,
the existing Runner Protocol worker, and the plane's opaque checkpoint API.
Use `--framework ai-sdk` and install the tested `ai@7.0.155` alongside the runner.
See the [managed AI SDK example](../examples/managed_ai_sdk/README.md).

These runs appear in Admin Threads and Runs. Admission, queueing, cancel and
usage follow the existing managed-run path. Native text and local JSON-result
tools are supported. The runner checks `allowed_tools` before local execution,
but arbitrary tools do not automatically acquire connector grants or HITL.
Native approval resume, binary/client/provider tools, streaming tool outputs,
`checkpoint_ref`, whole-run caching and partial-turn resume are unsupported.
AI SDK UI clients need the example's application-side stream bridge.

History stays in its native message format, with framework/agent validation,
run/tenant/generation binding and conditional writes. State must commit before
terminal success. Reclaim can replay an already-committed turn without repeating
tools. Earlier crashes remain subject to downstream side-effect idempotency.

### Managed Microsoft Agent Framework Python runner (v0.6.0)

The `python-agent-framework` adapter executes a per-run factory
returning a public `agent_framework.Agent`. Load a config declaring
`runner_kind: python-agent-framework` in both the plane and runner. Source-built
`runkite-runner` wheels and sdists include the adapter; the `agent-framework`
extra pins `agent-framework-core==1.23.0`. This packaging ships in published
PyPI `0.6.0`. From the repository root, or with
`pip install 'runkite-runner[agent-framework]'`:

```bash
python3.12 -m venv .venv-agent-framework
.venv-agent-framework/bin/python -m pip install './python[agent-framework]'
.venv-agent-framework/bin/runkite-runner-agent-framework \
  --config examples/managed_agent_framework/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

The installed module alternative is `python -m agent_framework_adapter`; no
`PYTHONPATH` is needed. Python 3.11+ remains supported. The ordinary
`runkite-runner` command still starts LangGraph, and its stock Python image
installs `[otel]` without the Microsoft SDK. Use your own environment or image
with this extra and entrypoint. Setup and the offline factory are in the
[managed Microsoft example](../examples/managed_agent_framework/README.md).

These executions create ordinary Threads/Runs and use managed dispatch, cancel
and usage. `RUNNER_TOKEN` authenticates both gRPC and run-bound checkpoint HTTP.
Native session history and successful-turn receipts use conditional opaque
writes. The runner validates framework, graph, session and CAS metadata before
model or tool execution. Reclaim replays an already committed turn using the
current generation without repeating the factory, model or tools.

Input is this turn's new user/system text messages, ending with a user message.
The session supplies previous history. Failed or interrupted turns retain the
last committed history and report usage already observed. Workflows, .NET,
non-text input, caller-supplied assistant/tool history, time travel and native
approval/resume are unsupported. Native approval/resume fails explicitly.

The v0.6.0 Microsoft profile enforces the dispatched `allowed_tools` snapshot
at the native function invocation boundary, before a local tool body runs. An
omitted graph entry is unrestricted, `[]` denies all local tools, and a nonempty
list permits only those names. A configured restriction fails before model
execution if a custom client lacks the supported native invocation boundary.
Denials emit `tool_auth` with `reason_code: tool_not_allowed`. This cooperative
check does not sandbox agent code or add connector grants or HITL. Local tools
need the existing connector helpers for connector policy. External effects before
a successful state commit require downstream idempotency.

Usage identifies a model only when all contributing requests have one consistent
identity. The configured pricebook can then estimate USD for the plane's existing
spend accounting and limits. Unknown or mixed model totals remain unpriced with
token accounting intact. Cancellation, errors and receipt replay preserve the
observed attribution; estimates are not provider invoices.

API cancellation retains usage already delivered to the plane by the current
attempt. Usage still buffered in the runner or provider when cancellation
commits is unavailable. Cancellation does not undo completed external effects.

### Managed OpenAI Agents SDK Python runner (v0.6.0)

The optional `python-openai-agents` profile executes per-run factories returning
public `agents.Agent` objects through the existing generic worker. Its native
model loop stays in the Python runner. The plane owns Threads/Runs, dispatch,
cancel and usage through the existing Runner Protocol. No hosted Agents API
session service is used.

Source-built `runkite-runner` wheels and sdists include the runtime adapter and
`runkite-runner-openai-agents` CLI. Install `./python[openai-agents]` from a source
checkout in a clean environment; its optional SDK is pinned to
`openai-agents==0.24.0`. This profile ships in published PyPI `0.6.0` via
`pip install 'runkite-runner[openai-agents]'`. The stock Python image
continues to use LangGraph and does not install this optional SDK.

Load `runner_kind: python-openai-agents` in the plane's agent config. Send only
new user/system text for each turn, ending with a user message. Native
successful-turn history and receipts use run/tenant/generation-bound opaque
state with conditional writes. The runner
validates native history before execution and replays a committed run without
repeating its model or tools. Failures and interruption retain the previous
committed history; external effects before commit still need idempotency.

The first profile supports one text agent and local function tools. A supplied
`allowed_tools` list is checked before local function bodies run. Connector
grants/HITL require the existing connector helpers. Native approval/resume,
handoffs, agents-as-tools, hosted/MCP tools, structured final outputs, non-text
input and unfinished-turn restoration are unsupported. Native reasoning remains
in checkpoints rather than assistant text. Native SDK trace export is disabled;
generic-worker OTLP run spans use the existing exporter variables. See the
[offline example](../examples/managed_openai_agents/README.md) and
[adapter contract](../python/adapters/openai_agents_adapter/README.md).

### Managed Pydantic AI Python runner (v0.6.0)

`python-pydantic-ai` executes native per-run agent factories through the generic
worker. The model loop stays in Python; Runkite owns dispatch, Threads/Runs,
cancel and reported usage. Text can stream while typed final results are
validated by the SDK and stored as JSON. The runner waits for native execution
and validation to finish before committing success.

The source-built `pydantic-ai` extra pins `pydantic-ai-slim==2.63.0` and includes
the `pydantic_ai_adapter` module and `runkite-runner-pydantic-ai` command.
Applications install their chosen providers separately. The ordinary CLI and
stock image keep their LangGraph defaults. This profile ships in published
PyPI `0.6.0`; follow the
[offline example](../examples/managed_pydantic_ai/README.md).

Native conversations retain message/tool history and cumulative SDK usage.
Runkite receives only new usage for the current run. Successful turns commit
through opaque CAS writes; a committed receipt recovers a reclaimed run without
another factory/model/tool cycle. Errors or interruption keep the previous
committed conversation. Effects before commit still need downstream idempotency.

The initial scope is text input, ordinary local tools and validated JSON output.
Local tool allowlisting is distinct from synthetic SDK output-schema tools.
Connector policy and HITL require the existing connector helpers. Native deferred
approval/resume, output functions, multimodal input and additional workflow
engines are unsupported. See the
[adapter contract](../python/adapters/pydantic_ai_adapter/README.md).

### Managed Google ADK Python runner (v0.6.0)

`python-google-adk` runs fresh ordinary `LlmAgent` factories through the generic
worker. It uses public ADK `Runner.run_async`, model/tool plugins and a run-local
`BaseSessionService`. Runkite's opaque CAS API remains the durable store; there
is no second session database or hosted Agent Engine deployment.

The source-built `google-adk` extra pins `google-adk==2.13.0`, includes
`google_adk_adapter` and provides `runkite-runner-google-adk`. Published PyPI
`0.6.0` includes this profile; the stock image does not install its optional SDK.
The default Python command remains LangGraph. See the
[offline example](../examples/managed_google_adk/README.md).

Input is exactly one new user text message. The session retains native event and
local tool history plus session-local state across successful turns. Temporary
state stays invocation-local. Cross-session `app:`/`user:` state is rejected.
The app retains UI authentication and presentation history.

The runner deduplicates native partial/aggregate text, observes model usage before
tool effects and commits only a settled successful invocation. Receipts replay
under the replacement generation without another factory/model/tool cycle.
Errors and interruption retain the previous committed session and report usage
already observed. External effects before commit still need idempotency.

The first slice excludes workflows, delegation, native confirmation/resume,
long-running tools, memory/artifact services, structured output and live audio.
Local allowlisting is cooperative; connector grants and HITL require the existing
connector helpers. Content capture on ADK-owned telemetry is disabled for managed
invocations using public run configuration. See the
[adapter boundaries](../python/adapters/google_adk_adapter/README.md).

### Framework Adapters

Opaque-state hardening (from v0.5.8) stops execution when configured state
cannot be loaded and withholds completion when a required save fails. A rejected
`403 run_not_inflight` write is not treated as saved state. Invalid versioned
message blobs fail before agent execution. This replaces warning-and-continue
behavior; it does not add full framework HITL resume.

The older Python generic opaque client still uses unconditional PUT. The managed
AI SDK and Microsoft paths use CAS and committed-turn receipts. Do not infer
those stronger recovery properties for every existing adapter. None of these
paths guarantees exactly once external effects before state commit.

**Opaque multi-turn (proxy):** when an adapter sets `checkpoint_framework` and `RUNKITE_HTTP_URL` (or the worker http address) is set, `generic_worker` loads `GET /internal/checkpoints/{thread}/adapter-state` before execute and `PUT`s that same id after success/interrupted — so clients can send only the new turn. This is **message continuity**, not LangGraph channel-state HITL: LlamaIndex restores native chat history; CrewAI / LangChain typically fold prior turns into the next prompt; AutoGen seeds model context. Not full CrewAI crew-memory restore. `checkpoint_ref` stays LangGraph-only (adapters reject it). Without an HTTP base URL, adapters stay single-shot. Same `/internal/checkpoints` surface as LangGraph proxy mode; adapters must not use `.../latest` on shared threads. See Runner Protocol §6.5 and `docs/limitations.md`.

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
