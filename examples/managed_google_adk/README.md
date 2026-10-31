# Managed Google ADK example

An offline public ADK `LlmAgent` dispatched as `python-google-adk`. Runkite
creates Threads/Runs, streams its text and records usage while the SDK executes
inside the Python worker. The example requires no provider key.

## Run

This profile is **v0.6.1** and included in published PyPI `0.6.1`. From
repository root:

```bash
python3.12 -m venv .venv-google-adk
.venv-google-adk/bin/python -m pip install './python[google-adk]'
.venv-google-adk/bin/runkite-runner-google-adk \
  --config examples/managed_google_adk/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

The extra pins `google-adk==2.13.0`. `python -m google_adk_adapter` is the
installed module alternative; no source `PYTHONPATH` is required. Load the
same config in the plane so `runner_kind: python-google-adk` selects the worker.
The stock image does not install this SDK, and the default Python command
remains LangGraph.

Use graph `managed_google_adk`, sending exactly one new user text message through
`input.messages`. Do not resend the conversation, send system messages or supply
native session identifiers. The server-owned factory provides static instructions.
Native session events, tool results and unprefixed session state survive successful
turns and worker restart through Runkite's existing opaque CAS API.

For authenticated deployments, the worker uses `RUNNER_TOKEN` for both gRPC and
checkpoint HTTP. Configure matching `RUNNER_TOKEN_PYTHON_GOOGLE_ADK` and
`RUNNER_TENANTS_PYTHON_GOOGLE_ADK` on the plane.

## Boundaries

The initial profile supports one ordinary text agent and local function tools.
Shared `app:`/`user:` state is rejected; `temp:` state remains invocation-local.
Native workflows/delegation, confirmation/resume, memory, artifacts, structured
output and live audio are unsupported. Native model proposals can spend tokens
before a confirmation requirement is rejected; the local tool body stays blocked.

Only successful turns replace durable history. Receipt recovery of the same run
replays without another factory/model/tool cycle. Failed or cancelled turns retain
the previous committed session and report usage already observed. Effects before
state commit still need downstream idempotency. Local tools need existing
connector helpers for connector grants, secret injection and HITL. The app retains
UI authentication and presentation history. The v0.6.0
[shared UI bridge](../managed_ai_sdk/README.md#use-the-same-frontend-with-a-python-runner)
uses profile `google-adk` to present bridge-created single-user turns.

## Tests

`make test-google-adk` exercises the actual SDK with offline models and mocked
provider HTTP/SSE. `make test-managed-google-adk` runs a real plane and a fresh
installed runner wheel. See the
[adapter contract](../../python/adapters/google_adk_adapter/README.md) for token
mapping, privacy, resources, bounds and backend-test scope.
