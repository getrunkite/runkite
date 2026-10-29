# Managed AI SDK runs and an application UI bridge

This example runs Vercel AI SDK in a Runkite runner. The app starts an ordinary
Runkite run and consumes its stream; it does not call the model in its Next/Nuxt
request handler. Runkite owns the thread, run record, dispatch, cancellation and
usage accounting. The SDK still owns the agent loop inside the runner.

`agent.ts` uses the pinned public AI SDK and an offline scripted model. It needs
no provider credentials. `langgraph.json` retains the existing configuration
filename and `graphs` mapping so the control plane can register it without a new
configuration subsystem. Its runner kind is `typescript-ai-sdk`.

## Run the agent

Build the TypeScript runner from this checkout and install this example's pinned
dependencies. Start your self-hosted plane with this example configuration
registered, then start the matching runner:

```sh
cd typescript/runkite-runner
npm ci
npm run build
cd ../..
cd examples/managed_ai_sdk
npm ci
cd ../..
node typescript/runkite-runner/dist/cli.js \
  --framework ai-sdk \
  --config examples/managed_ai_sdk/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

Use your deployment's existing runner token and TLS configuration. This example
does not add a model proxy, web server, vault or second scheduler. Requests to
`POST /threads/{thread_id}/runs` create managed records visible in Admin Threads
and Runs. Direct Agent Protocol callers can use the existing stream and wait
routes without the UI bridge.

## Keep the existing AI SDK frontend

AI SDK's `useChat` expects its UI message stream, while Runkite streams Agent
Protocol events. `bridge.ts` converts the new runner's `custom:ai_sdk` events
using the SDK's public `createUIMessageStreamResponse`. Your app keeps its
authentication, chat ownership and server-side Runkite credential.
Use a server-side key with the read/write permissions needed to create, stream,
wait and cancel runs, plus the matching agent grant when scoped run grants are
enabled. The decision-only `governance:decide` permission is insufficient here.

Inside an already-authenticated server handler, resolve the chat and turn from
your application database before calling the bridge:

```ts
import { createManagedChatResponse } from "./bridge.ts";

// Your application has authenticated the request and checked ownership.
// ownedChat.runkiteThreadId comes from its durable chat-to-thread mapping.
// storedTurn.runkiteRunId is an application-allocated UUID for this operation.
return createManagedChatResponse(request, {
  baseUrl: process.env.RUNKITE_HTTP_URL!,
  apiKey: process.env.RUNKITE_API_KEY!,
  threadId: ownedChat.runkiteThreadId,
  agentId: "managed_ai_sdk",
  runId: storedTurn.runkiteRunId,
  onCancelError: () =>
    yourExistingLogger.warn(
      "Inspect the managed run: cancellation was not confirmed",
    ),
});
```

Create the Runkite thread once with `POST /threads` and persist its ID against the
authenticated application's chat ID. Reuse that thread for later turns and
allocate a distinct run ID for each operation. The bridge uses an application
run ID, or generates a UUID if omitted. Persist and reuse it for HTTP request
retry idempotency. The application's ownership check must happen on every turn.
Do not use the browser body's `id`, `thread_id`, `agent_id`, approval flags or
headers to select a Runkite identity or credential.

The frontend uses the normal AI SDK transport pointed at that authenticated app
route. It receives no Runkite key:

```tsx
import { useChat } from "@ai-sdk/react";
import { DefaultChatTransport } from "ai";

const chat = useChat({
  id: ownedApplicationChatId,
  transport: new DefaultChatTransport({
    api: `/api/chats/${ownedApplicationChatId}/chat`,
  }),
});
```

This is a server helper and route integration example, not a second Next/Nuxt
application. Supply your existing session lookup, durable chat mapping and
frontend message storage. UI history in `useChat` is presentation state. The
bridge sends only the latest text user message; prior browser history cannot
replace the runner's durable model history.

## Completion and cancellation

The bridge creates a run, then joins its existing stream. Joining replays events
so a fast worker cannot lose output between those requests. It validates native
UI chunks with the pinned SDK's public schema and deduplicates replayed event
IDs. Text and ordinary server tool results stream immediately. Native finish is
held until the existing `/wait` route confirms the durable run and the output's
attempt/message identity. A durable error, timeout or interruption overrides an
advisory successful SSE terminal.

Stopping the browser request or cancelling the response body sends an explicit
Runkite cancel request. It does not merely disconnect SSE. Run creation has a
bounded HTTP timeout independent of browser cancellation so a disconnect during
creation can be cancelled as soon as the run is confirmed. If the process dies
or a network failure leaves creation/cancellation unconfirmed, inspect the saved
run ID through your application's existing recovery path. Cancellation remains
cooperative and cannot undo a tool's completed external write.

Raw SSE remains advisory during the documented reclaim race. The bridge ignores
late events from a lower attempt. If a new attempt starts after the UI has
already rendered output, it reports a delivery error instead of mixing two
attempts into one assistant reply. It does not transparently reconnect, retry
the model or replay side effects. Inspect the durable run after a delivery error.

## Initial scope and tests

This bridge accepts new text user turns and ordinary server-executed tools.
Regeneration, file/multimodal input, browser-executed tools, browser approval
responses and automatic stream reconnection are outside this initial example.
If native UI events are unavailable, such as after broker replay eviction or a
run-cache hit, the bridge reports delivery failure even if the durable run
succeeded. It does not invent a UI transcript from an opaque final result.
The runner's managed lifecycle does not automatically govern every local tool;
use actual connector gates for governed external access. Native checkpoint time
travel and approval resume are not claimed.

Keep whole-run caching disabled for these agents. The runner refuses an
`llm_cache` entry in its startup config; changing agent metadata externally can
still create a cache hit without native UI events. Successful turns meter real
provider token usage. Cancellation can settle in the plane before final usage
events arrive, so canceled-run spend is not guaranteed complete. Only previously
committed turns are restored after cancellation.

The runner caps native state at 16 MiB, its per-turn UI receipt at 8 MiB, and
the final output event at 3 MiB. An oversized conversation fails explicitly.

The example bounds browser input to 64 KiB, individual SSE frames and JSON
completion bodies to 1 MiB, and stream event IDs to 20,000. Adapt explicit limits
to your product rather than silently accepting an unbounded request or event.

```sh
cd examples/managed_ai_sdk
npm ci
npm run check
npm test
```

Tests use the real pinned `DefaultChatTransport`, native UI schema and
`readUIMessageStream` with fake Runkite HTTP responses. They cover text/tool
rendering, durable finish gating, new-turn input, fragmented UTF-8/CRLF SSE,
replay deduplication, attempt fencing, failure and cancellation. They need no
running plane, provider SDK, model key or network after dependency installation.

From the repository root, `make test-managed-ai-sdk` builds the runner and a real
Go plane, runs the native SDK and UI checks, then proves dispatch, Admin visibility,
restart continuity, cancellation and crash-after-commit receipt recovery against
SQLite and the in-process queue. CI runs this target. To run the same subprocess
checks against a dedicated Postgres and Redis test pair, supply both
`RUNKITE_TEST_POSTGRES_DSN` and `RUNKITE_TEST_REDIS_URL`. These services receive test
records, so use isolated instances. This single-plane test is not a multi-replica
HA soak.

The updated `Dockerfile.runner-ts` installs this example's dependencies alongside
the existing LangGraph.js example. Select it with
`--framework ai-sdk --config /app/examples/managed_ai_sdk/langgraph.json`.
The npm package and image require a maintainer release before these additions
are available from public registries.
