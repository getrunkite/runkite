# Managed Pydantic AI runner

Executes public `pydantic_ai.Agent` factories through runner kind
`python-pydantic-ai`. Runkite owns Threads/Runs, dispatch, cancellation and
reported usage. The SDK keeps its model loop and output validation in Python.

## Install and run

This profile is **unreleased** and absent from the published PyPI `0.5.8`
artifact. From the repository root, use a clean Python 3.11+ environment:

```bash
python3.12 -m venv .venv-pydantic-ai
.venv-pydantic-ai/bin/python -m pip install './python[pydantic-ai]'
.venv-pydantic-ai/bin/runkite-runner-pydantic-ai \
  --config examples/managed_pydantic_ai/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

The extra pins `pydantic-ai-slim==2.63.0`. Install the provider packages required
by your application separately. The offline example does not require one.
The installed module alternative is `python -m pydantic_ai_adapter`, with no
repository `PYTHONPATH`. The ordinary CLI and stock image keep LangGraph defaults;
the image does not install this optional SDK.

Load the same config in the plane with `runner_kind: python-pydantic-ai`.
For authentication, set `RUNNER_TOKEN` on the worker and matching
`RUNNER_TOKEN_PYTHON_PYDANTIC_AI` and `RUNNER_TENANTS_PYTHON_PYDANTIC_AI` on
the plane. The same worker credential binds gRPC and checkpoint HTTP.

## Factory and output

Each graph export is a per-run factory receiving frozen `PydanticAIRunContext`
with run, thread, tenant, generation, graph and cancellation identity. Return a
fresh public `Agent`, or exported `PydanticAIAgentDefinition(agent, deps=...)`
when native dependency injection is needed. Dependencies are recreated by the
factory rather than serialized into a checkpoint.

Send only this turn's new user/system text in `input.messages`, ending with a
nonempty user message. The input object contains only `messages`; assistant and
tool history belong to the runner. Native text streams as assistant messages.
Typed final output is serialized as validated JSON under `output.result`, with
ordinary managed metadata and usage alongside it. Structured JSON is not
fabricated as assistant text. The application owns UI authentication and
presentation history; this profile emits Agent Protocol, not an AI SDK UI stream.

The runner drains native execution and commits only the completed result. An
early final-result announcement or partial JSON never completes a durable run.
The SDK owns schema validation and retries, whose observed usage remains billed.
Ordinary local tool allowlisting is separate from synthetic output-schema tools.

## Conversation, usage and recovery

Successful turns persist the public native `Conversation` with CAS through
`/internal/checkpoints/{thread}/adapter-state`. Reads validate framework, graph,
ETag and native state before execution. The checkpoint retains conversation
metadata and cumulative native usage; Runkite receives only this run's new usage.

A successful receipt for the same `run_id` replays its result and text under the
current generation before constructing the agent. It does not repeat model or
tool work. Errors and interruption retain the previous committed conversation
and report usage already observed. The retained native usage describes that
committed conversation; the plane also accounts for observed failed/cancelled
work. State I/O failures withhold completion and leave the assignment recoverable.
Native SDK usage limits apply to that restored conversation, alongside the
plane's run admission and recorded-spend limits.

Consistent known model identity lets the existing pricebook estimate USD.
Unknown or mixed-model aggregates remain unpriced with token accounting intact.
SDK cost estimates do not replace the operator's Runkite pricebook. These are
estimates, not provider invoices. Cancellation cannot recover usage that was not
received by the plane or undo external effects. Effects before a successful
state commit require downstream idempotency.

Events are capped at 3 MiB of compact UTF-8 JSON, text receipts at 8 MiB and
native state at 16 MiB. History is not silently truncated. Native checkpoints
can contain conversation, reasoning and tool data; apply database access and
retention controls accordingly.

## Initial boundaries

- Text input, ordinary local function tools and final JSON-serializable output.
- A supplied `allowed_tools` list gates local execution; `[]` denies all local
  tools while allowing SDK schema-output validation. This is cooperation, not a
  Python sandbox.
- Connector grants, secret injection, audit and HITL require existing Runkite
  connector helpers. An arbitrary Python function does not gain them by dispatch.
- Native deferred approval/resume, output functions, multimodal input, unfinished
  turn restoration and external workflow engines are unsupported. Operator
  approval alone does not resume an SDK turn.
- Structured results initially use plain output types and the SDK's default
  schema-tool mode. Explicit `NativeOutput`, `PromptedOutput` and `ToolOutput`
  markers are outside this profile. Models whose prepared output mode is neither
  text nor tool-schema fail explicitly before the model request.
- Native/provider tools, remote MCP or custom toolsets, deferred loading,
  fallback model wrappers and whole-run SDK caches are outside this profile.
- No extra durable workflow service or upstream runtime is embedded in the Go
  plane. Native instrumentation is disabled; existing generic-worker OTLP run
  tracing remains the exporter path.

## Tests

```bash
make test-pydantic-ai
make test-python-package
make test-managed-pydantic-ai
```

Unit tests use the pinned SDK, including native provider clients over mocked
HTTP/SSE. Installed-wheel tests run a real plane and a fresh consumer outside
the checkout, with explicit test-only provider dependencies. Set both
`RUNKITE_TEST_POSTGRES_DSN` and `RUNKITE_TEST_REDIS_URL` to dedicated disposable
services for the Postgres/Redis profile; otherwise tests use SQLite and the
in-process queue. This is not a multi-replica HA soak or a live provider test.
