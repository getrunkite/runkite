# Managed Pydantic AI example

A native Pydantic AI factory dispatched by Runkite as `python-pydantic-ai`.
The SDK executes in the Python runner while the plane owns Threads/Runs,
streaming, cancellation, reported usage and committed-turn recovery.

## Run

This profile is **unreleased** and absent from the published PyPI `0.5.8`
artifact. From repository root:

```bash
python3.12 -m venv .venv-pydantic-ai
.venv-pydantic-ai/bin/python -m pip install './python[pydantic-ai]'
.venv-pydantic-ai/bin/runkite-runner-pydantic-ai \
  --config examples/managed_pydantic_ai/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

The source extra pins `pydantic-ai-slim==2.63.0`; model-provider packages are
application choices. The offline example uses the SDK's public function model
and needs no provider credential. The installed module alternative is
`python -m pydantic_ai_adapter`, with no source `PYTHONPATH`.

Load the same config in the plane. Its `runner_kind: python-pydantic-ai`
selects this worker. Send only the new user/system text turn through
`input.messages`, ending with a nonempty user message. Do not resend the whole
conversation or supply native assistant/tool history.

For authentication, the worker uses `RUNNER_TOKEN` for both gRPC and checkpoint
HTTP. The plane uses matching `RUNNER_TOKEN_PYTHON_PYDANTIC_AI` and
`RUNNER_TENANTS_PYTHON_PYDANTIC_AI` settings.

## Results and durability

Native text streams normally. A structured agent's validated final JSON is
stored under the durable run's `output.result`; partial JSON does not mark a run
successful. The SDK retains its native schema validation and retry behavior.

The config exports `managed_pydantic_ai` for text and
`managed_pydantic_ai_typed` for the `Summary` model. The typed example stores
`{"count":7,"note":"validated"}` under `output.result`. Select that graph as
the run's agent; the request still sends only the new text turn.

Successful native conversations and receipts survive worker restart through
the plane's opaque CAS state. Reclaim after a successful commit replays the
same run without another factory/model/tool cycle. Usage reported for a new turn
excludes earlier conversation turns. Cancelled or failed turns retain the last
committed conversation and report usage already observed.

The app still owns UI authentication and presentation history. This example
does not implement the AI SDK UI stream protocol. Local tools need existing
connector helpers for connector grants/HITL. Native deferred approvals, output
functions, multimodal input and external workflow engines are unsupported.
External side effects before state commit still need downstream idempotency.

## Tests

`make test-pydantic-ai` exercises the actual pinned SDK without a live model.
`make test-managed-pydantic-ai` runs a real plane with an installed runner wheel.
See the [adapter contract](../../python/adapters/pydantic_ai_adapter/README.md)
for provider dependencies, output bounds and backend-test scope.
