# Managed Microsoft Agent Framework example

Offline factory for the managed Python runner. No provider credentials. The
runner kind is `python-agent-framework`. Creates ordinary Runkite Threads/Runs
through the control plane.

This is separate from [governance middleware](../governance_agent_framework/README.md),
which only submits tool decisions while the app keeps the loop.

## Run

The optional runner packaging is **unreleased**. The published PyPI `0.5.8`
artifact does not contain this adapter or its CLI. From the repository root,
build and install the local package in a clean Python 3.11+ environment
(shown here with 3.12):

```bash
python3.12 -m venv .venv-agent-framework
.venv-agent-framework/bin/python -m pip install './python[agent-framework]'
.venv-agent-framework/bin/runkite-runner-agent-framework \
  --config examples/managed_agent_framework/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

The extra pins `agent-framework-core==1.23.0`. The installed module alternative
is `.venv-agent-framework/bin/python -m agent_framework_adapter` with the same
arguments; neither command needs `PYTHONPATH`. The regular `runkite-runner` CLI
and stock Python image retain their LangGraph defaults. After a release
containing this change is published, install the profile with
`pip install 'runkite-runner[agent-framework]'` instead of the local package.

Create runs against graph id `managed_agent_framework` with `input.messages`
containing the new user/system text messages for this turn, ending with a user
message. Do not send the full conversation again: the runner loads its native
session history. Non-text content and caller-supplied assistant/tool history are
unsupported. Session history and turn receipts survive runner
restarts via opaque checkpoints. Reclaim of the same `run_id` replays without
another model or tool cycle.

The unreleased [shared UI bridge](../managed_ai_sdk/README.md#use-the-same-frontend-with-a-python-runner)
uses profile `agent-framework` for bridge-created single-user string turns.
The application keeps UI authentication, presentation history and its stored
chat/turn mapping.

Load this example's `langgraph.json` in the control plane as well as the runner.
Its `runner_kind` selects `python-agent-framework` for dispatch. On authenticated
deployments, set `RUNNER_TOKEN` in the worker and the matching
`RUNNER_TOKEN_PYTHON_AGENT_FRAMEWORK` and `RUNNER_TENANTS_PYTHON_AGENT_FRAMEWORK`
on the plane. The worker uses the same credential for gRPC and checkpoint HTTP.

Only successful turns replace durable session history. Cancelled, failed and
unsupported native approval turns preserve the last committed history. Reported
usage from an unfinished turn still reaches the run output. Native approval or
resume requires a different integration; this adapter rejects it explicitly.
Tools that finish before an uncommitted crash can run again on reclaim, so use
downstream idempotency for external effects.

## Tests

From the repository root, `make test-agent-framework` prepares the isolated
environment and runs unit tests. `make test-managed-agent-framework` runs a real
plane with both the shared runner and Microsoft adapter installed from the built
wheel. See the [adapter README](../../python/adapters/agent_framework_adapter/README.md)
for the SQLite and optional Postgres/Redis profiles.
