# Langfuse through existing OTLP exporters

Status: current-checkout deployment and qualification work. The exporter paths
already ship in v0.9.0. No version, package-availability or release change is
introduced by this recipe. Start with the [operator guide](../langfuse.md).

## Existing source and ownership

`internal/tracing/tracing.go:Init` installs the control-plane provider only when
a generic or traces endpoint is configured. `newExporter` chooses the existing
OTLP HTTP or gRPC exporter; its HTTP encoder is protobuf. The `resource.New`
ordering lets environment service/resource settings override the default name.
`Traceparent` uses the standard W3C propagator.

`internal/api/runs.go:createRunCtx` starts `run`, retains its live handle in
`runSpans`, and puts its real trace context in the queue assignment. That handle
is process-local. This work does not turn it into durable distributed span
state or reconstruct an original root span after restart or replica takeover.

`python/runkite_runner/tracing.py:init` soft-loads OpenTelemetry, selects the
protobuf HTTP exporter for its HTTP branch and installs a batch processor.
`run_span` consumes the assignment context and activates `runkite.run`.
The optional dependency is the runner's existing `otel` extra; a missing SDK
produces a warning and a disabled tracer rather than an export guarantee.

`typescript/runkite-runner/src/tracing.ts:initTracing` selects
`@opentelemetry/exporter-trace-otlp-http` for HTTP. That package sends JSON.
`withRunSpan` consumes the assignment context and uses the existing async-local
context manager. The current branch selection treats HTTP-prefixed protocol
labels alike; setting `http/protobuf` does not replace its JSON encoder. The
recipe deliberately uses `http/json` for this runner. Its resource construction
sets `service.name`; it does not claim complete environment-variable parity
with the Go and Python resource SDKs.

At baseline `42ef2c3e52d4daa293e35aecbe2d0717ee06d5c5`, all three
implementation files above match the v0.9.0 tagged source. The recipe reuses
that exporter behavior instead of adding a Langfuse exporter,
second SDK/provider, endpoint proxy, key store or model-loop wrapper.

## Wire and backend contracts

The recipe uses `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` with the full
`/api/public/otel/v1/traces` path and signal-specific headers. Go/Python use
`http/protobuf`; TypeScript uses `http/json`. Header values use the standard
OTLP environment encoding and are injected by the deployment, not a connector
session or `secret_ref` resolver.

The external receiver baseline is Langfuse v4.81.0, commit
[`f1e38efe36a752c551288292dc6318ce4c11ce1a`](https://github.com/langfuse/langfuse/tree/f1e38efe36a752c551288292dc6318ce4c11ce1a).
Read the pinned [OTLP processing path](https://github.com/langfuse/langfuse/blob/f1e38efe36a752c551288292dc6318ce4c11ce1a/web/src/server/otel/processOtelIngestion.ts)
alongside the official [OTLP endpoint and attribute guide](https://langfuse.com/integrations/native/opentelemetry).
The API accepts HTTP protobuf and JSON. The v4 ingestion marker is part of the
qualified header configuration. Authenticated ingestion queues processing;
acceptance alone must not be recorded as persisted trace/observation success.

The native fixture pins the v4 target settings on its own web and worker:
`LANGFUSE_MIGRATION_V4_WRITE_MODE=events_only`,
`LANGFUSE_MIGRATION_V4_NATIVE_OTEL_BEHAVIOUR=direct` and
`LANGFUSE_MIGRATION_V4_ALLOW_PREVIEW_OPT_IN=true`. The pinned
[`worker/src/env.ts`](https://github.com/langfuse/langfuse/blob/f1e38efe36a752c551288292dc6318ce4c11ce1a/worker/src/env.ts)
and [`web/src/env.mjs`](https://github.com/langfuse/langfuse/blob/f1e38efe36a752c551288292dc6318ce4c11ce1a/web/src/env.mjs)
define the corresponding v4 defaults. The fixture sets them explicitly for
reproducibility; it does not bypass a fresh v4.81.0 default. Its persistence
read is `GET /api/public/v2/observations`, filtered by the synthetic trace ID,
with `fields=basic,time,metadata,io,model,usage` and `limit=100`.
Older Langfuse versions and operator-selected legacy/migration modes require
their own read/write compatibility check. These settings are not a production
upgrade or data-migration instruction for an existing Langfuse deployment.

The existing callback sources are
`python/runkite_runner/otel_callbacks.py` and
`typescript/runkite-runner/src/otelCallbacks.ts`. They ignore normal prompt,
completion and tool input/output payloads and record model/tool names, run IDs
and status. Error paths record exception information. Their metadata does not
automatically become model token/cost measurements or a full content trace.
Other adapters can emit different native instrumentation, so the guide does
not promise the same callback shape for every managed SDK.

## Recorded qualification: November 17, 2026

The [checkout instructions](../../examples/langfuse_otlp/README.md) cover the
prerequisites, native qualifier CLI and wire/offline commands. The
[sanitized native receipt](../../examples/langfuse_otlp/qualification.json)
records a passing run against Langfuse 4.81.0 from the source pin above. Runkite
used baseline `42ef2c3e52d4daa293e35aecbe2d0717ee06d5c5` plus this recipe and
qualification patch; the existing exporter implementations were unchanged.

### Actual receiver and persisted result

The local fixture used the six unmodified service images specified by upstream
Compose, pinned to these measured arm64 digests:

```text
postgres@sha256:0d938e975177bef9f1f61f3f9dbcf2e0aa0a139568f66d3415ed8892eb97f6ac
redis@sha256:2e3eb11462c4e3759e6972678dd051d150ff54b1e60714b3c33b701260172a90
cgr.dev/chainguard/minio@sha256:1ec745a06a986762f89e68e855fc189f403941eff8c0a5f94767e92c8e39d3f5
clickhouse/clickhouse-server@sha256:de3e47c64078aa196dae2349e048fc98ba81947f651f5c20e06e46baea8ae40a
docker.langfuse.com/langfuse/langfuse-worker@sha256:2d975cae7cff6f6426bb04eb2361b9230911cc2b88e609c51ce7708bd77584f9
docker.langfuse.com/langfuse/langfuse@sha256:289074f655e86ec8423ab3cc40b6a2428f0f2d5c827230ee48f7e56b23af35f4
```

It used a new synthetic local project with ephemeral project keys, product
telemetry disabled and the explicit v4 modes documented above. The Docker
network was internal with no egress. A host loopback byte-forwarder reached
the owned web container through `docker exec`; web bound to `0.0.0.0` only
inside that isolated network. This was local test wiring, not a product proxy
or a recommendation to add another deployment component. All owned services,
the forwarder and their temporary resources were cleaned up after the pass.

The actual Go exporter created one synthetic `run` root. The production Python
and TypeScript run-span/LangChain callback helpers emitted a `runkite.run`,
`runkite.llm` and `runkite.tool` each, using its W3C parent. The v2 Observations
API persisted exactly seven completed ordinary `SPAN` observations from three
services, in one trace with the expected parent hierarchy, names and operational
metadata. The v4 read API returns flat keys such as
`metadata["resourceAttributes.service.name"]` and
`metadata["attributes.run.id"]`; they are not nested resource/span objects.
Missing-auth and invalid-auth read probes were both denied.

No generation or model call was produced. The API's empty model string, empty
usage/cost details and zero-valued usage placeholders are not measurements. The qualifier
rejects invented GenAI attributes, nonempty input/output and nonzero model
usage/cost, and verifies synthetic run/thread/agent, tenant and correlation
metadata where the production helpers supply it. Public output keeps only
version/count summaries, without headers, project keys or trace/span IDs.

Measured versions were Go 1.26.11 with OTel SDK/HTTP exporter 1.45.0;
Python 3.12.15 with OTel SDK/HTTP exporter 1.45.2; and Node 26.11.0 with
TypeScript OTel SDK 2.10.0 and HTTP exporter 0.221.0. Node 22.19.0 separately
passed the TypeScript wire check. A different backend version, read/write mode
or instrumentation mapping requires its own qualification.

### Wire, offline and regression checks

Real exporters passed bounded loopback capture tests for path, authorization,
the v4 marker, encoding, span completion and W3C identity. Go covered both HTTP
selector cases plus endpoint/receipt guards; Python passed four wire, auth and
cleanup tests, and TypeScript passed two wire cases. The qualifier's 16 offline tests passed. The
[wire workflow](../../.github/workflows/otlp-wire.yml) runs wire/offline checks
only; it does not provision Langfuse. The native result above is a local
six-service backend result, not a hosted CI or publication claim.

Existing Go tracing, API and bridge suites passed. The full TypeScript runner
suite passed 294 tests with two skips because a live end-to-end binary was not
provided. Site tests passed 43/43, and the documentation checker found zero
broken links across 71 HTML pages and 3,831 local references. These are scoped
regression results on this assembled patch, not an absence-of-all-bugs claim.

The native fixture qualifies only the exporter and helper boundary. It does
not start a Runkite plane, dispatch a managed job, run a framework worker or
execute a real model or business tool. Existing Go API traceparent tests cover
dispatch propagation separately; combining those results does not create an
unexecuted end-to-end native-backend qualification. Backend UI behavior,
production load, retention, provider compatibility and HA remain outside this
proof.

## Deliberate limits

The plain operational-span recipe is useful without a Langfuse-specific model
instrumentor. Full prompt capture, cost inference, user/session translation,
scoring, evaluation, prompt management and framework-specific GenAI mapping are
not introduced here. A separately configured collector can transform supported
attributes, but that requires its own tested mapping rather than renaming a run
or thread into another product's identity model.

Stdout logs and Prometheus metrics remain independent. Langfuse project
authorization also remains independent of Runkite tenant authorization.
Exporting through one shared project does not create tenant isolation in the
receiver. Trace/error attributes require their own data-access and retention
review; a no-body callback is not a general-purpose redactor.

Durable run state, approval/action evidence and metering remain Runkite's
records. A sampled, dropped or incomplete trace cannot authorize a tool,
establish sandbox isolation, prove a remote business effect or safely replay an
unknown outcome. Neither this recipe nor a test receipt implies upstream
endorsement or universal Langfuse feature compatibility.
