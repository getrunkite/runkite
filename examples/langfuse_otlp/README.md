# Qualify existing Runkite exporters against local Langfuse

This checkout qualifier exercises the real Go exporter and the production
Python/TypeScript run-span and LangChain callback helpers. It expects seven
ordinary `SPAN` observations in one trace: one Go root and a run, LLM and tool
span from each runner language. It makes no model or business-tool call and
does not dispatch a managed run or start a framework worker.

Use the [deployment guide](../../docs/langfuse.md) for configuration and the
[engineering note](../../docs/internal/langfuse-otlp.md) for source pins, measured
results and limits. Existing exporters ship in v0.9.0; this qualifier is a
checkout asset included with v0.10.0 source.

## Prepare local prerequisites

From the repository root, use the installed Go toolchain required by `go.mod`,
Node 22.19.0 or newer, npm and a Python 3.12 environment. Prepare module/build
caches before invoking the qualifier:

```sh
export GOCACHE="$(go env GOCACHE)"
export GOMODCACHE="$(go env GOMODCACHE)"
go mod download
go test -mod=readonly ./internal/tracing -run '^$' -count=1
npm ci --prefix typescript/runkite-runner
npm run build --prefix typescript/runkite-runner
python3.12 -m venv /tmp/runkite-langfuse-otel
/tmp/runkite-langfuse-otel/bin/python -m pip install \
  opentelemetry-sdk==1.45.2 opentelemetry-exporter-otlp==1.45.2 \
  langchain-core==1.7.3 protobuf==7.36.2
```

The Python pins match the wire workflow's OTLP and callback prerequisites.
An existing runner environment containing its `otel` extra and LangChain Core
can be supplied instead; the native receipt records the versions actually used.
The emitter imports the matching checkout's runner source.

Explicit cache paths are important because child processes use a temporary
home and `GOPROXY=off`, `GOTOOLCHAIN=local`, `GOWORK=off` and read-only modules.
They do not download missing toolchains, dependencies or Node packages. Keep
`go` and `node` available on `PATH` and choose an already prepared Python
executable.

## Prepare a fresh local backend

Start unmodified Langfuse **4.81.0** from its
[pinned official source](https://github.com/langfuse/langfuse/tree/f1e38efe36a752c551288292dc6318ce4c11ce1a),
following its self-hosting instructions. Use a disposable local org/project
and ephemeral project API keys, disable product telemetry, and expose its web
endpoint only on a literal `127.0.0.1` port. Do not point this test at a hosted
or customer project. The qualifier creates observations and does not provision
or tear down the backend.

The native fixture uses the v4 defaults explicitly:

```text
LANGFUSE_MIGRATION_V4_WRITE_MODE=events_only
LANGFUSE_MIGRATION_V4_NATIVE_OTEL_BEHAVIOUR=direct
LANGFUSE_MIGRATION_V4_ALLOW_PREVIEW_OPT_IN=true
```

Keep web and worker read/write settings consistent. These are qualification
settings for a fresh pinned deployment, not migration instructions for existing
Langfuse data. The read check uses `/api/public/v2/observations`.

Have the local fixture's secret mechanism inject
`OTEL_EXPORTER_OTLP_TRACES_HEADERS`. It must contain exactly project Basic
authorization and `x-langfuse-ingestion-version=4`, with OTLP percent-encoded
header values. Do not paste actual key values into a command, this file, a
receipt or support output. The qualifier supplies each child's full trace
endpoint and the actual encoding: protobuf for Go/Python, JSON for TypeScript.

## Run native persistence qualification

Choose a new absolute output file in an existing directory and pass the ID of
the fresh local project. For a fixture listening on port 3210:

```sh
python3.12 examples/langfuse_otlp/qualify.py \
  --url http://127.0.0.1:3210 \
  --python /tmp/runkite-langfuse-otel/bin/python \
  --project-id runkite-otel-fixture \
  --output /tmp/runkite-langfuse-result.json \
  --expected-version 4.81.0
```

The expected version defaults to 4.81.0; other versions are rejected. The URL
must have only the root path on a literal HTTP `127.0.0.1` port. Redirects,
proxy use and ambient credentials are excluded. The qualifier has a bounded
deadline, refuses to overwrite an output file and writes a private sanitized
summary only after persistence and missing/invalid-auth checks pass.

The summary contains SDK/backend versions and counts, not project keys or
trace/span IDs. HTTP acceptance alone is insufficient: all seven observations
must be readable with matching trace, parent, service and operational metadata,
without invented generation, input/output, model-usage or cost data. A successful
helper-boundary proof does not qualify dispatch, framework execution, production
HA, a visual UI or a real provider.

The pinned v4 observation API returns flat dotted metadata keys, so checks use literal keys such as `resourceAttributes.service.name` and `attributes.run.id`.

## Repeat wire and offline checks

From the repository root:

```sh
go test ./internal/tracing -count=1 -v
PYTHONPATH=python /tmp/runkite-langfuse-otel/bin/python python/tests/test_tracing_otlp.py
python3.12 -m unittest discover -s examples/langfuse_otlp -p 'test_*.py' -v
cd typescript/runkite-runner
node --import tsx --test src/tracingOtlp.test.ts
cd ../..
```

The [wire workflow](../../.github/workflows/otlp-wire.yml) runs the actual
exporters against bounded capture receivers and runs the qualifier's offline
safety/receipt tests. It does **not** run a Langfuse backend. Keep that CI result
separate from a native persistence receipt, and remove the disposable backend
and its owned data after local qualification.
