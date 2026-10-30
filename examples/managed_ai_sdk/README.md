# Managed SDK runs and a shared application UI bridge

This example runs Vercel AI SDK in a Runkite runner. The app starts an ordinary
Runkite run and consumes its stream; it does not call the model in its Next/Nuxt
request handler. Runkite owns the thread, run record, dispatch, cancellation and
usage accounting. The SDK still owns the agent loop inside the runner.

The checkout's shared bridge also accepts the four managed Python profiles:
Microsoft Agent Framework, OpenAI Agents SDK, Pydantic AI and Google ADK. It
translates their ordinary Agent Protocol text events for the same AI SDK UI
transport. Choose the profile in trusted server configuration.

`agent.ts` uses the pinned public AI SDK and an offline scripted model. It needs
no provider credentials. `langgraph.json` retains the existing configuration
filename and `graphs` mapping so the control plane can register it without a new
configuration subsystem. Its runner kind is `typescript-ai-sdk`.

**Unreleased bridge update:** this checkout adds shared Python text/result
profiles, detach, reattach and explicit Stop helpers to the app-side example.
They are separate from the managed runner
that shipped in v0.5.8, and do not change the plane's run API or SDK model loop.

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
  disconnectPolicy: "detach",
  expectedMessageId: storedTurn.assistantMessageId,
  onMessageStart: async (messageId, signal) => {
    await saveAssistantMessageId(
      storedTurn.id,
      storedTurn.runkiteRunId,
      messageId,
      signal,
    );
  },
});
```

Create the Runkite thread once with `POST /threads` and persist its ID against the
authenticated application's chat ID. Reuse that thread for later turns and
allocate a distinct run ID for each operation. The bridge uses an application
run ID, or generates a UUID if omitted. Persist and reuse it for HTTP request
retry idempotency. The application's ownership check must happen on every turn.
Do not use the browser body's `id`, `thread_id`, `agent_id`, approval flags or
headers to select a Runkite identity or credential.

`saveAssistantMessageId` above is your application's database transaction. It
must bind the first assistant ID to that exact owned turn/run, accept an
already-equal ID on retries, and reject a different existing ID instead of
overwriting it. Pass its supplied `AbortSignal` to the store operation and use
the store's normal deadline. The helper bounds the callback by request abort
and `timeoutMs`, then exposes the verified native start only after it succeeds.
A failed callback fails delivery; detach still leaves execution running. A
timeout does not roll back an app database commit that already started, which
is why retries must accept the same saved ID.

The reconnecting example opts into `disconnectPolicy: "detach"`. That mode
requires an explicit run ID persisted by the app before creation. Omitting the
policy retains the existing `"cancel"` behavior, where browser disconnect or
delivery failure triggers best-effort run cancellation and an omitted run ID
can be generated by the helper.

The frontend uses the normal AI SDK transport pointed at that authenticated app
route. It receives no Runkite key:

```tsx
import { useChat } from "@ai-sdk/react";
import { DefaultChatTransport } from "ai";

const chat = useChat({
  id: ownedApplicationChatId,
  transport: new DefaultChatTransport({
    api: `/api/chats/${ownedApplicationChatId}/chat`,
    resumeStreamIsReplay: true,
    prepareReconnectToStreamRequest: () => ({
      api: `/api/chats/${ownedApplicationChatId}/stream`,
    }),
  }),
});
```

This is a server helper and route integration example, not a second Next/Nuxt
application. Supply your existing session lookup, durable chat mapping and
frontend message storage. UI history in `useChat` is presentation state. The
bridge sends only the latest text user message; prior browser history cannot
replace the runner's durable model history.

## Use the same frontend with a Python runner

The unreleased `profile` option selects a checked server-side mapping:

- `ai-sdk` is the default and preserves native AI SDK UI events and tool parts.
- `agent-framework` uses the managed Microsoft Python runner.
- `openai-agents` uses the managed OpenAI Agents SDK Python runner.
- `pydantic-ai` uses the managed Pydantic AI Python runner.
- `google-adk` uses the managed Google ADK Python runner.

Register the chosen agent and start its existing runner using that profile's
example. The bridge creates ordinary managed runs on the application's Runkite
thread; it does not execute the model in your application handler. The Python
modes support assistant text
and Pydantic's final JSON result. Native tool panels, reasoning display,
multimodal parts and native approval/resume are outside this bridge slice.

`routes.ts` supplies Web Request/Response handlers with mandatory application
hooks. Your app must authenticate and check ownership on create, attach and
Stop, create the thread mapping, persist each turn/run ID before creation, and
bind the assistant ID transactionally. The helper does not supply a user database
or choose an operation from browser-provided Runkite identifiers.

For example, create a server module in your app using your existing storage:

```ts
import { createManagedChatRoutes } from "./routes.ts";

export const chatRoutes = createManagedChatRoutes({
  baseUrl: process.env.RUNKITE_HTTP_URL!,
  apiKey: process.env.RUNKITE_API_KEY!,
  resolveTurn: async (request, operation) => {
    // Application hook: authenticate, check ownership and resolve the exact turn.
    // For create, allocate and persist its run ID before returning.
    // Read request data only through a bounded clone so the bridge keeps its body.
    const turn = await requireOwnedTurn(request, operation);
    // Your hook may return an auth error, or 204 when attach has no active turn.
    if (turn instanceof Response) return turn;
    return {
      threadId: turn.runkiteThreadId,
      runId: turn.runkiteRunId,
      agentId: "managed_pydantic_ai",
      profile: "pydantic-ai",
      expectedMessageId: turn.assistantMessageId,
      onMessageStart: (messageId, signal) =>
        saveAssistantMessageId(turn.id, turn.runkiteRunId, messageId, signal),
    };
  },
});
```

`requireOwnedTurn` and `saveAssistantMessageId` are application hooks to
implement using your existing sessions and database. The latter must bind an
unset-or-equal ID and reject an attempt to replace it. `routes.ts` always uses
detach and requires a persisted run ID and an assistant-ID binding callback.
Use direct bridge helpers when you need the existing cancel-on-disconnect mode.
The profile and agent must agree with your registered configuration; the
browser cannot select either through its request body.

### Next App Router routes

In an existing Next app, export these handlers from its route files:

```ts
// app/api/chats/[chatId]/chat/route.ts
import { chatRoutes } from "@/server/managed-chat";
export const runtime = "nodejs";
export const POST = chatRoutes.create;
```

```ts
// app/api/chats/[chatId]/stream/route.ts
import { chatRoutes } from "@/server/managed-chat";
export const runtime = "nodejs";
export const GET = chatRoutes.attach;
```

```ts
// app/api/chats/[chatId]/stop/route.ts
import { chatRoutes } from "@/server/managed-chat";
export const runtime = "nodejs";
export const POST = chatRoutes.stop;
```

Next's [Route Handlers](https://nextjs.org/docs/app/getting-started/route-handlers)
accept these standard Request/Response functions. Keep the helper in server
code and use a Node runtime; its Runkite credential never belongs in a client
component or public environment variable.

### Nuxt / Nitro routes

Use the public H3 `fromWebHandler` adapter in an existing Nuxt app's Node server:

```ts
// server/api/chats/[chatId]/chat.post.ts
import { fromWebHandler } from "h3";
import { chatRoutes } from "~~/server/managed-chat";
export default fromWebHandler(chatRoutes.create);
```

Use the same wrapper around `chatRoutes.attach` in `stream.get.ts` and
`chatRoutes.stop` in `stop.post.ts`. H3 documents the adapter for both its
[v1 handler API](https://v1.h3.dev/guide/event-handler) and
[current handler API](https://h3.dev/guide/basics/handler).
These are route recipes for your existing app, rather than a second Nuxt app
or an embedded frontend in the control plane.

For Pydantic structured output, render the final `data-result` message part as
ordinary JSON. Check its part type rather than the value's truthiness: `null`,
`false` and `0` are valid results. The bridge does not invent assistant text for
a typed-only response. A result part becomes visible only after durable
completion and current-turn validation.
Result data is finite JSON with a maximum nesting depth of 64. Integral numbers
must fit JavaScript's safe range, -9007199254740991 to 9007199254740991; larger
integral values fail explicitly. Use strings for exact identifiers outside that
range. This bridge does not promise arbitrary-precision numeric transport.

Python mode requires the runner's retained `running` lifecycle prefix, contiguous
event identities and a matching durable framework/generation. Reconnect uses
the same full-replay and persisted-message-ID rules below. It attaches to turns
created through this bridge: one new user string message. Broader Agent Protocol
inputs such as system batches or text-part arrays are not accepted for attachment.
The bridge never
fills a missing transcript from a historical assistant or restarts model work.
Its existing example bounds apply, including the 1 MiB response/frame limit;
large retained conversations can fail explicitly rather than being truncated.

## Reattach to an existing run

Your authenticated app's GET stream route resolves the same stored turn and
checks chat ownership again, then calls the attach helper:

```ts
import { attachManagedChatResponse } from "./bridge.ts";

// GET /api/chats/:chatId/stream, after application authentication/ownership.
return attachManagedChatResponse(request, {
  baseUrl: process.env.RUNKITE_HTTP_URL!,
  apiKey: process.env.RUNKITE_API_KEY!,
  threadId: ownedChat.runkiteThreadId,
  runId: storedTurn.runkiteRunId,
  expectedMessageId: storedTurn.assistantMessageId,
  onMessageStart: async (messageId, signal) => {
    await saveAssistantMessageId(
      storedTurn.id,
      storedTurn.runkiteRunId,
      messageId,
      signal,
    );
  },
});
```

Attach defaults to detach. It validates the existing run/thread mapping and
joins that run's stream; it does not create a run, resubmit user input or execute
the model/tools again. If the app has no saved operation to attach to, its route
can return `204`, which the native transport treats as no stream to resume.
The helper does not discover a user's last run or choose one from browser input.

Pass the stored assistant ID as `expectedMessageId` on create retries and
reattach. It fences the UI identity across connections: a new generation cannot
silently append a second assistant to an older partial reply just because its
own replay is internally consistent. The helper rejects a different start ID
before exposing it. Keep the persistence callback on reattach too; it must
tolerate the same start being replayed.

An undefined `expectedMessageId` is appropriate only before any assistant was
displayed for that operation, or for a fresh observer with no partial assistant
to merge. Do not merge unknown partial UI state without its saved identity;
reload the app's authoritative messages first. The helper cannot infer an
earlier browser's assistant ID from a new connection's SSE history.

After restoring the app's saved UI messages, call `chat.resumeStream()` from its
recovery action. The pinned SDK receives a complete replay from the beginning,
not a byte offset. `resumeStreamIsReplay: true` resets the streaming assistant
state before applying that replay, so already-rendered text and tool parts are
not appended twice. Keep the same application chat ID and server-owned mapping
for the run and assistant message. Starting another POST chat request is a new
turn, not reconnection.
Browser `onToolCall` and analytics callbacks may fire again during replay; make
them tolerate repeated events and never execute managed tools from those callbacks.

## Completion, disconnect and Stop

The bridge creates a run, then joins its existing stream. Joining replays events
so a fast worker cannot lose output between those requests. It validates native
UI chunks with the pinned SDK's public schema and deduplicates replayed event
IDs. Text and ordinary server tool results stream immediately. Native finish is
held until the existing `/wait` route confirms the durable run and the output's
attempt/message identity. A durable error, timeout or interruption overrides an
advisory successful SSE terminal.

With detach, a browser disconnect, a cancelled response reader or a delivery
failure only closes that HTTP delivery. The managed run may continue in its
runner, consume model tokens and finish while nobody is watching. The same
stored run can be attached later. With the default cancel policy, those paths
instead send best-effort run cancellation. Run creation is bounded independently
of browser cancellation so its outcome can be checked even if the browser leaves.
If creation's response is lost, use the saved run ID to inspect or attach; a
network failure is not proof that the plane never created it.

In detach mode, the UI's Stop action needs a separate authenticated POST route:

```ts
import { cancelManagedChatRun } from "./bridge.ts";

// POST /api/chats/:chatId/stop, after the app's existing mutation/ownership checks.
await cancelManagedChatRun({
  baseUrl: process.env.RUNKITE_HTTP_URL!,
  apiKey: process.env.RUNKITE_API_KEY!,
  threadId: ownedChat.runkiteThreadId,
  runId: storedTurn.runkiteRunId,
});
return new Response(null, { status: 204 });
```

The UI calls that route to stop the run, and `chat.stop()` to stop local stream
consumption:

```tsx
async function stopManagedChat() {
  const response = await fetch(
    `/api/chats/${ownedApplicationChatId}/stop`,
    { method: "POST", credentials: "same-origin" },
  );
  if (!response.ok)
    throw new Error("Stop was not confirmed; inspect the managed run");
  await chat.stop();
}
```

Wire this function to the Stop button and show its rejection through your app's
existing error UI. If the app's request client supplies mutation or CSRF headers,
use that client with those existing protections for this POST too.

Calling only `chat.stop()` in detach mode disconnects the reader and leaves
execution running. A confirmed cancel HTTP response does not promise that
an external effect was undone; cancellation is cooperative and may race an
already-committed success. Surface a failed Stop request rather than reporting
that execution stopped.

Raw SSE remains advisory during the documented reclaim race. The bridge ignores
late events from a lower attempt. If a new attempt starts after the UI has
already rendered output, it reports a delivery error instead of mixing two
attempts into one assistant reply. Reattach supports a retained complete stream
from the same attempt. It does not transparently cross a reclaim, retry the
model or replay side effects. Inspect the durable run after a delivery error.

## Replay retention and completeness

The bridge requires the native start and contiguous runner-event IDs, including
the non-UI events between native chunks. It rejects a missing prefix or middle
gap rather than confirming a partial transcript merely because a finish and
matching durable generation remain. Duplicate event IDs are still deduplicated.
If full native replay is unavailable, it emits
`Managed chat replay is unavailable; reload saved messages` and withholds UI
finish. This also covers an expired broker history or a run-cache result with
no native UI stream. Detach leaves the run alone on those delivery errors.

The default Redis broker sets a 24-hour expiry for completed streams. That is
not a guarantee of complete replay for 24 hours: Redis eviction, operator
trimming/deletion or this example's replay limits can make history unavailable
earlier. The in-process broker cannot retain events across a control-plane
restart. This is not a permanent UI-message store or a cursor-based replay API.
The app persists its own UI messages and provides the reload path. The bridge
does not invent a UI transcript from an opaque final result or re-run the agent
to rebuild one.

## Initial scope and tests

This bridge accepts new text user turns and ordinary server-executed tools.
Regeneration, file/multimodal input, browser-executed tools and browser approval
responses are outside this initial example. Same-attempt reconnection requires
the app-owned mapping and retained complete replay described above. The helper
does not schedule retries or create a new run after a stream failure.
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
replay deduplication, attempt fencing, detach/reattach, explicit Stop, failure and
cancellation. They need no
running plane, provider SDK, model key or network after dependency installation.
The Python-profile tests also cover current-turn matching, typed-only null/false/
zero results, framework and generation mismatch, omitted replay prefixes,
identity persistence, abort timing, private tool data and bounded JSON/text.

`make test-managed-chat-ui` prepares these dependencies and runs the UI checks.
`make test-managed-chat-bridge` additionally runs each of the four installed
Python workers against a real plane, including native UI text/history, completed
replay, detach/Stop, recovered-generation receipts and Pydantic typed-only JSON.
Set both backend URLs below to use Postgres/Redis. To enable the UI cases on one
existing profile target, use `RUNKITE_TEST_CHAT_BRIDGE=1 make test-managed-google-adk`
(or the corresponding Microsoft/OpenAI/Pydantic target) after UI preparation.
CI enables these checks for all four Python profiles on both backend pairs.
The models are offline fixtures; these tests do not build a full Next/Nuxt app.

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
