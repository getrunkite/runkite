# Managed AG-UI text transport

Status: checkout-only example included with v0.10.0 source, including the
additive shared application chunk-stream helpers. This note records an
application example, not a new runtime protocol or an AG-UI compliance claim.

## Caller need and boundary

An application using a native AG-UI HTTP client needs to consume a durable
Runkite-managed agent without executing the model inside its request handler.
The existing AI SDK bridge already implements public run creation/attachment,
checkpoint-backed completion confirmation, attempt fencing, replay validation,
application assistant-ID binding, detach and explicit Stop. A second copy of
those mechanisms would drift.

The implementation extracts only `createManagedChatChunks` and
`attachManagedChatChunks` from the existing bridge. Existing response helpers
continue to serialize the same validated chunks with AI SDK's public encoder.
No lifecycle logic moves into a second engine. The new AG-UI adapter consumes
that stream and uses the official `EventEncoder` for text/event-stream output.
The existing bridge's 116 tests remain the regression gate for this seam.

The host application still owns authentication, CSRF, durable chat/turn mapping,
assistant-ID compare-and-set and user-visible message storage. The route helper
reauthorizes create, attach and Stop separately. Server configuration supplies
the Runkite endpoint, credential, profile, agent, thread and run. Browser AG-UI
IDs must match a stored application mapping before creation; they do not select
backend identities or confer authority. Attach has no creation fallback.

Streaming and Stop still need a dedicated application's tenant read/write
credential. The additive specialist run/observe profile does not grant stream
or cancel access. A Runkite agent scope is not owner-only privacy. This example
therefore does not relax the existing dedicated-tenant recommendation.

## Public package evidence

The source/type declarations in official npm packages `@ag-ui/core`,
`@ag-ui/encoder` and `@ag-ui/client`, all version 1.0.4, were inspected before
implementation. Their lockfile integrity entries pin the actual packages.
Relevant public exports are:

- Core `EventType`, `PROTOCOL_VERSION`, `Event` and `RunAgentInput`. Runtime
  `RunAgentInputSchema` and `EventSchema` are in `@ag-ui/core/schemas`, not in
  the top-level runtime namespace. `PROTOCOL_VERSION` is `1.0`.
- Encoder `EventEncoder({ accept: "text/event-stream" })`, `getContentType()`
  and `encodeBinary(event)`. The adapter does not hand-roll AG-UI SSE framing.
- Client `HttpAgent`, `runAgent`, `abortRun`, event subscribers and `setMessages`.
  The client verifies event ordering and reconstructs text messages. A
  `RUN_FINISHED` with an explicit success outcome invokes its success callback.
- The native reducer appends content when replay reopens an existing assistant
  message ID. Passing a partial response straight back into a client and
  appending full replay would duplicate text. This is why the reconstruction
  helper uses a fresh client with that exact partial assistant removed.

The packed client verifier permits `RUN_ERROR` as the first event and with an
open text message. A failed delivery can therefore end with one error without
inventing a successful text end or terminal success. The adapter maps source
abort to delivery-aborted wording because the shared detach policy may leave
the remote run executing.

## Supported contract

Input is bounded to 64 KiB with fatal UTF-8 decoding, an intake deadline and
request-abort propagation. The official strict run-input schema validates its
top-level structure. The last message must contain plain user text. Only that
new text enters Runkite; earlier browser messages are presentation history,
not a replacement checkpoint. Nonempty tools, context, resume, state or
forwarded properties, parent runs, unsupported protocol versions, multimodal
content and unpaired Unicode surrogates are rejected before dispatch.

The adapter accepts one assistant text part. Start and content events carry the
shared core's verified assistant ID. Start is withheld until the application's
binding callback commits successfully. Text delta boundaries are preserved and
the official encoder handles Unicode. Native AI SDK step markers do not become
AG-UI control events. Tool, reasoning, structured data and unknown native chunks
fail the text transport; no fake tool, interrupt or approval events are emitted.
Unsupported delivery leaves the remote run available for normal inspection.
A native completion without a completed text part is also unsupported. The AI SDK
can validly finish without emitting any text part; mapping that to success left
no assistant message for retained reconstruction. Creation and attachment now
both emit `RUN_ERROR` for that case and never fabricate an empty assistant.

Only the core's verified durable finish becomes `RUN_FINISHED` with
`outcome: { type: "success" }`. Stream EOF, advisory native finish, failed wait,
error/interrupted/timeout durable status, mismatched framework, changed attempt
or failed assistant binding cannot produce success. Error text is fixed and
safe; raw upstream exceptions, metadata and the Runkite key are not forwarded.

Request abort and response-body cancellation detach and release the existing
stream reader. They do not call the cancellation route. Explicit Stop uses the
existing public cancellation helper after ownership is resolved again; failed
cancellation is not returned as a successful 204. Acceptance of a cancel request
does not undo already committed model or tool side effects.

Full retained attachment uses GET and never dispatches. Reconstruction uses a
fresh official client, verifies application thread/run and assistant IDs, and
returns replacement messages only after successful completion. It never mutates
the caller's saved messages on failure. The caller decides when to persist and
replace its UI response. There is no automatic append on reattach.

Retention expiry, deleted runs, incomplete replay and new attempts can prevent
reconstruction. The app must reconcile uncertainty before the first verified
assistant ID is bound. Existing run IDs provide retained-record retry behavior,
not indefinite deduplication after deletion. This example adds no tombstone,
new run-reference protocol, owner identity or backend storage subsystem.

## Verification

Provider-free deterministic checks performed on the source worktree:

- TypeScript runner build and both bridge type checks pass.
- Existing AI SDK bridge: 116 tests pass after the shared-stream extraction.
- AG-UI bridge: 25 tests pass using the actual official client and runtime event
  schema. Coverage includes one-byte Unicode transport, combining characters,
  delayed durable finish, error/timeout/interrupted status, generation mixing,
  duplicate replay, failed assistant binding, failed wait, client abort,
  response-body cancellation, explicit Stop and failed Stop, ownership and
  input validation, media-range whitespace and exclusion precedence, native
  completion without a text part, identity mismatch and replacement-only replay.
- Public documentation link checker reports zero broken local references.

The existing `aiSdk.e2e.test.ts` real-plane fixture gains an opt-in
`RUNKITE_TEST_AG_UI=1` profile. It uses a scripted AI SDK model, a dedicated
read/write app key, a fresh Go plane and the packaged Node runner. The AG-UI
helper creates and completes one text run, reconstructs retained replay with one
model call and no tool effects, detaches and reattaches to a running text run,
then sends explicit authenticated Stop and observes the real runner abort. The
fixture owns local temporary files and child-process cleanup. The dedicated
`ag-ui-example.yml` workflow installs both examples, runs both regression suites
and executes this qualification without provider credentials.

The integrating maintainer executed this profile on 2026-11-16 against the
candidate Go binary and packaged Node runner: **11 tests passed, zero failures,
zero skips**, including both new AG-UI scenarios. The measured profile was
SQLite/in-process. The two scenarios confirmed successful native text and full
replay replacement with one model call and no tool effects, then running
creation/attachment detach without cancellation until authenticated Stop reached
the real runner abort signal. The existing AI SDK real-plane and crash-recovery
scenarios also passed in that run.

The qualification log is retained in the review workspace at
`outputs/runkite-next/research-20261116/ag-ui-native-e2e.log`. This is a local
maintainer fixture, not an external provider, Postgres/Redis, multi-replica or
independent security assessment of AG-UI.
