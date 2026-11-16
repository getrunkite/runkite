# Send Runkite traces to Langfuse

Runkite already exports OpenTelemetry traces. Published v0.9.0 contains the
Go control-plane exporter and Python/TypeScript runner exporters used here.
This deployment recipe and its qualification are current-checkout work; they
add no Langfuse SDK, model proxy, storage service or UI to Runkite.

The boundary is `Runkite process -> OTLP/HTTP -> your Langfuse deployment`.
An external OpenTelemetry Collector can sit between them when your deployment
needs fan-out or attribute mapping. Runkite stays self-hosted, and Langfuse
continues to own its ingestion, storage, access control and UI.

## Prepare the receiver

Run Langfuse separately using its [official self-hosting instructions](https://langfuse.com/self-hosting).
Create a project and use that project's public/secret API-key pair for OTLP
Basic authentication. Load the resulting header through your deployment's
process-secret mechanism. Connector `secret_ref` does not configure tracing.

The full traces URL is
`https://langfuse.example/api/public/otel/v1/traces`; replace the host and any
reverse-proxy prefix with your own deployment. Langfuse accepts HTTP protobuf
and JSON, not gRPC. Runkite defaults to gRPC, so endpoint configuration alone
is insufficient. Keep TLS verification enabled outside a local test fixture.

For Langfuse v4, include `x-langfuse-ingestion-version=4` with the authorization
header. It selects the current ingestion path used by the v2 Observations API.
The [official OTLP guide](https://langfuse.com/integrations/native/opentelemetry)
documents the API-key encoding and ingestion header.

## Configure each process

The examples below require your deployment to inject `LANGFUSE_OTLP_HEADERS`
before startup. Its value is a standard OTLP header list containing
`Authorization=Basic%20<encoded-project-key-pair>,x-langfuse-ingestion-version=4`.
The key pair is `public_key:secret_key` encoded as one unwrapped Base64 value;
percent-encode header values as required by the OTLP environment format.
Keep the real value out of source control, shell history and support tickets.

Use the full signal-specific endpoint so the SDK does not need to construct a
path from a generic endpoint. Apply configuration to both the plane and every
runner process whose spans you want to receive. Setting variables only on the
plane does not configure separate runner containers.

Go control plane:

```sh
: "${LANGFUSE_OTLP_HEADERS:?Load the project OTLP headers from a deployment secret}"
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://langfuse.example/api/public/otel/v1/traces
export OTEL_EXPORTER_OTLP_TRACES_HEADERS="$LANGFUSE_OTLP_HEADERS"
export OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf
export OTEL_SERVICE_NAME=runkite
```

Python runner:

```sh
: "${LANGFUSE_OTLP_HEADERS:?Load the project OTLP headers from a deployment secret}"
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://langfuse.example/api/public/otel/v1/traces
export OTEL_EXPORTER_OTLP_TRACES_HEADERS="$LANGFUSE_OTLP_HEADERS"
export OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf
export OTEL_SERVICE_NAME=runkite-runner-python
```

Install the Python runner's `otel` extra in the runner environment. Without the
OpenTelemetry packages, the optional Python tracing path logs a warning and
remains disabled. Check the worker startup log and receiver before relying on
tracing. Do not install a second tracer provider solely for this recipe.

TypeScript runner:

```sh
: "${LANGFUSE_OTLP_HEADERS:?Load the project OTLP headers from a deployment secret}"
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://langfuse.example/api/public/otel/v1/traces
export OTEL_EXPORTER_OTLP_TRACES_HEADERS="$LANGFUSE_OTLP_HEADERS"
export OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/json
export OTEL_SERVICE_NAME=runkite-runner-typescript
```

The current Go and Python HTTP exporters encode protobuf. The current
TypeScript HTTP exporter encodes JSON. Its HTTP branch does not change to a
protobuf encoder when given `http/protobuf`; use the language-specific settings
above rather than treating all HTTP protocol labels as interchangeable.
Both encodings are supported by Langfuse. Keep HTTP selection separate from
SDK-specific settings such as resource attributes or compression.

## What to expect

The plane starts a `run` span and passes a real W3C `traceparent` in the runner
assignment. The runners open `runkite.run` beneath that context. Existing
LangChain callbacks add `runkite.llm` and `runkite.tool` children with names,
run identity and status metadata. Other adapter instrumentation differs.

Treat these as operational observations. The built-in callbacks do not export
prompt/completion bodies, token counts or costs. A span named `runkite.llm`
does not supply the measurements for a Langfuse generation. Rich GenAI views
need separately instrumented attributes or an explicitly qualified external
mapping. This recipe does not add those attributes or make a model call.

`run.id`, `thread.id` and `graph.id` remain Runkite attributes. A thread is not
automatically a Langfuse session, nor is an authenticated Runkite principal
automatically a Langfuse user. A collector mapping must preserve these meanings
and propagate any required fields to the appropriate spans. It is optional and
outside the baseline recipe.

`LOG_FORMAT=json` changes stdout logs, not OTLP traces. Prometheus metrics also
remain on their existing path. Do not route logs or metrics to the Langfuse
trace endpoint. See Langfuse's [signal support](https://langfuse.com/faq/all/opentelemetry-logs-metrics-span-links).

## Check a deployment

1. Start with one synthetic managed run and distinct service names. Do not use
   customer prompts or credentials as diagnostic attributes.
2. Allow graceful exporter shutdown or batch delivery. An HTTP acceptance
   response proves receipt, not that an asynchronous receiver has persisted the
   observation. Poll the backend's supported read API or inspect its UI.
3. Verify the trace ID, parent span IDs, names and expected run metadata across
   the plane and runner. Check `run.status` against the durable Runkite record.
4. Confirm that ordinary metadata spans have no invented input/output, token or
   cost data. Test rejected authorization separately without logging headers.
5. Only then add collector mapping or additional framework instrumentation.

On November 17, 2026, the actual exporters and production run-span/LangChain
callback helpers passed a local persistence check against unmodified,
self-hosted Langfuse v4.81.0 at
[`f1e38efe36a752c551288292dc6318ce4c11ce1a`](https://github.com/langfuse/langfuse/tree/f1e38efe36a752c551288292dc6318ce4c11ce1a).
The backend persisted seven ordinary `SPAN` observations from three services in
one connected trace, with the expected parent IDs and operational metadata.
Two missing/invalid-auth read probes were denied. No generation or model call
was produced. Empty model strings and zero-valued usage fields returned by the
read API are backend placeholders, not measured model usage.

The [checkout qualifier](../examples/langfuse_otlp/README.md) provides the
prerequisites and commands; its [sanitized receipt](../examples/langfuse_otlp/qualification.json)
records the measured versions and counts. This helper-boundary result does not
dispatch a managed job, start a framework worker, execute a business tool or
qualify a provider, HA deployment or visual UI. Existing API propagation tests
are separate evidence. The [engineering note](internal/langfuse-otlp.md) records
the pinned setup, regression checks and limits. The wire/offline workflow does
not start a Langfuse backend.

## Failure and data boundaries

- For missing traces, first check the full endpoint, HTTP protocol selection,
  project authorization, ingestion header and runner-side configuration.
  A collector receiving a request is not proof of a persisted Langfuse row.
- Span names and metadata can reveal agent, thread and tool identities.
  Exceptions, framework instrumentation and application-added attributes can
  contain sensitive text even though the built-in callbacks omit normal bodies.
  Decide retention, redaction and access policy before exporting production data.
- Langfuse project access does not inherit Runkite tenant permissions. A shared
  plane may emit data from several tenants. Choose an appropriate deployment
  and receiver access boundary instead of assuming tenant-scoped backend views.
- Telemetry is best effort. Process loss, receiver failure or restart can leave
  incomplete traces. The plane's open span handle is process-local, so a later
  completion on another replica does not establish a complete original root
  span. Durable Runkite state remains the operational record.
- A trace, reply or error observation does not prove a business effect occurred
  once, that cancellation rolled it back, or that native tools used a connector
  policy gate.
