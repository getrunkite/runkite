# Built-in fetch and the TypeScript TLS dispatcher

Status: correction to the untagged 0.8.0 candidate, following the first hosted
identity qualification on the merged eight-RFE milestone. This extends the
qualification for [RFE #49](https://github.com/getrunkite/runkite/issues/49) and
[RFE #47](https://github.com/getrunkite/runkite/issues/47).

## Caller and observed failure

A TypeScript worker using a private server CA or client certificate could
connect to gRPC but fail its HTTP status preflight on Node 22. Strict ownership
correctly refused execution, then the assignment was reclaimed. This was an
availability failure, not permission to execute without a successful preflight.

The first [hosted manual run](https://github.com/getrunkite/runkite/actions/runs/43627959919)
failed the packaged TypeScript case on Node 22.19.0. It did not reach the Python
external-runtime HA stage. The previous local Node 26 result remains valid for
its stated version, but did not establish Node 22 HTTP compatibility.

A direct reproduction against the merged implementation found Node 22.19.0's
bundled Undici 6.21.2 throwing `UND_ERR_INVALID_ARG`, with
`invalid onRequestStart method`. The runner's installed Undici 8 expects newer
dispatcher callbacks than that built-in fetch supplies. Tests with a mocked
fetch do not exercise this callback boundary.

## Reuse the dependency's public compatibility boundary

`typescript/runkite-runner/src/tls.ts:httpDispatcher` wraps the configured
`Agent` or `HTTPSOnlyAgent` in Undici's exported `Dispatcher1Wrapper`. The
dependency owns translation between the legacy and modern callback contracts,
including response bodies, completion and errors. Runkite does not implement
its own HTTP stack, import private library internals or replace global fetch.

The wrapper forwards close and destroy operations. Its compatibility transport
uses HTTP/1.1, preserving streaming response bodies and SSE. It keeps the
existing CA, client certificate/key, timeout, redirect and cancellation settings.
`HTTPSOnlyAgent` still rejects plaintext client-certificate requests before
headers or bodies can be sent. Certificate trust and server-name validation
remain enabled. Other SDK clients and their global dispatchers are unchanged.

The shared helper covers status, checkpoints, store, vector and A2A proxy calls.
No caller needs a replacement model loop or an application-level fetch patch.

## Regression boundary

Focused tests exercise the actual legacy connect/header/data/end/error
translation, modern callback pass-through, and plaintext refusal before the
underlying TLS agent is invoked. The packaged strict-mTLS fixture uses real
certificate-bound HTTP and gRPC calls, including two durable turns and rejection
of a same-URI peer key. Its report includes the Node and bundled Undici versions.

Ordinary CI runs that packaged fixture on Node 22.19.0 and Node 26.11.0, rather
than leaving this transport boundary exclusively to optional local tests. The
manual external-runtime workflow remains a separate bounded qualification.
Record actual outcomes before calling either workflow qualified. Broader
production soak and independent assessment remain separate gates.

The corrected exact npm archive passed all four real fixture cases locally
under both Node 22.19.0 and Node 26.11.0. The Node 22 report records bundled
Undici 6.21.2. Reports are retained as
`outputs/runkite-next/node22-mtls-fixed.json` and
`outputs/runkite-next/node26-mtls-fixed.json`; the archive manifest is in
`outputs/runkite-next/node22-fix-artifact-final/`. These task-local artifacts
are separate from the repository. No model provider was called. The initial
hosted failure remains a failure until the follow-up commit is qualified there.
