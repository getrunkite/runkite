# Managed AG-UI text bridge

**Checkout-only example included with v0.10.0 source.** The shared application
chunk-stream helpers are checkout assets too; use matching v0.10.0 plane and
runner packages for the managed execution beneath this route.

This application-side example connects the official AG-UI HTTP client to an
ordinary Runkite managed run. It uses `@ag-ui/core`, `@ag-ui/encoder` and
`@ag-ui/client` version **1.0.4**, protocol **1.0**. The official encoder writes
SSE; the official client reconstructs the response in the tests. No Go runtime,
model call, scheduler, credential broker or application database is embedded.

The supported slice is one plain user text turn and one assistant text response.
A successful response contains `RUN_STARTED`, `TEXT_MESSAGE_START`,
`TEXT_MESSAGE_CONTENT`, `TEXT_MESSAGE_END` and `RUN_FINISHED` with an explicit
success outcome. Failure uses `RUN_ERROR`. A text-end event alone is not success.
The source must provide one completed text part. A native completion with no text
part ends delivery with `RUN_ERROR`, even when the managed run succeeded; the
adapter does not invent an assistant message that retained replay cannot rebuild.
The shared bridge withholds successful finish until the public Runkite wait
response confirms durable completion, the selected framework and the current
attempt.

## Install and verify

Use Node 22.19 or newer. From the repository root:

```sh
npm ci --prefix typescript/runkite-runner
npm run build --prefix typescript/runkite-runner
npm ci --prefix examples/managed_ai_sdk
npm ci --prefix examples/managed_ag_ui
npm run check --prefix examples/managed_ai_sdk
npm test --prefix examples/managed_ai_sdk
npm run check --prefix examples/managed_ag_ui
npm test --prefix examples/managed_ag_ui
```

The sibling example supplies the pinned AI SDK used by the shared lifecycle
engine. Its agent configuration and runner setup are described in
[the managed AI SDK example](../managed_ai_sdk/README.md). Select `ai-sdk` for a
text-only native AI SDK agent, or select one of the shared Python text profiles
in trusted server configuration. The fixture qualification uses an offline
scripted model, not a provider key.

To run the real Go plane, packaged Node runner and native AG-UI client fixture:

```sh
go build -o /tmp/runkite-ag-ui ./cmd
cd typescript/runkite-runner
RUNKITE_TEST_BINARY=/tmp/runkite-ag-ui RUNKITE_TEST_AG_UI=1 \
  node --import tsx --test src/aiSdk.e2e.test.ts
```

This command owns and cleans up its temporary SQLite database and local child
processes. The optional Postgres/Redis profile in that fixture remains available
through its existing environment variables.

## Integrate existing application routes

The application authenticates every create, attach and Stop operation. Store
its chat-to-Runkite-thread mapping and a distinct turn-to-Runkite-run mapping
before creation can dispatch. Allocate the run UUID on the server and reuse it
for that operation. Store the application AG-UI thread/run IDs separately from
the Runkite IDs. They are protocol identifiers, not authorization tokens.

```ts
import { createManagedAGUIRoutes } from "./routes.ts";

const routes = createManagedAGUIRoutes({
  baseUrl: process.env.RUNKITE_HTTP_URL!,
  apiKey: process.env.RUNKITE_API_KEY!,
  async resolveTurn(request, operation) {
    const session = await requireApplicationSession(request);
    // Check this user's chat AND turn ownership for every operation. On create,
    // allocate and commit the new turn/run mapping before returning it.
    const turn = await resolveOwnedTurn(session.userId, request, operation);
    if (!turn) return new Response(null, { status: 404 });
    return {
      threadId: turn.runkiteThreadId,
      runId: turn.runkiteRunId,
      agentId: "my_text_agent",
      profile: "ai-sdk",
      aguiThreadId: turn.aguiThreadId,
      aguiRunId: turn.aguiRunId,
      expectedMessageId: turn.assistantMessageId,
      async onMessageStart(messageId, signal) {
        await bindAssistantMessageId(turn.id, messageId, signal);
      },
    };
  },
});

// Mount these behind the application's existing authentication and CSRF rules.
export const POST = routes.create; // /api/chats/:chat/turns/:turn/agui
// Mount routes.attach as GET on a separate retained-replay route.
// Mount routes.stop as POST on a separate explicit Stop route.
```

The lookup and database functions above belong to the host application. They are
not supplied by this example. Resolve the URL's application chat/turn under the
authenticated owner. If lookup needs the body, read only a bounded clone. Never
use a browser-supplied backend run, thread, agent, credential or ownership claim.
The bridge caps its own request body at 64 KiB. Wrong methods return 405;
unsupported input returns a safe 400 response. The host app translates setup
failures into its usual safe HTTP errors.

`bindAssistantMessageId` must atomically bind the first verified assistant ID to
this exact turn/run, accept an already-equal ID and reject a different saved ID.
It must honor the supplied signal and the database's normal deadline. The
existing bridge awaits the binding before exposing the assistant start. A
callback failure stops delivery and leaves the managed execution available for
inspection. A timeout cannot undo a database commit already started.

Keep the Runkite key on the server. Streaming and explicit cancellation require
the existing tenant read/write permissions, plus the applicable agent run grant
when configured. The upcoming `agents:<id>:observe` permission permits only exact
public run status and wait access, so run/observe credentials alone are
insufficient for this adapter. Use a dedicated application tenant and apply
ownership in your application; Runkite's agent-scoped observation is not a
per-user privacy boundary.

The native client sends ordinary `RunAgentInput` to the authenticated app route:

```ts
import { HttpAgent } from "@ag-ui/client";

const agent = new HttpAgent({
  url: ownedTurn.createUrl,
  threadId: ownedTurn.aguiThreadId,
  initialMessages: [{ id: newUserMessageId, role: "user", content: text }],
  // Supply the application's session/CSRF transport through headers or fetch.
});
await agent.runAgent({ runId: ownedTurn.aguiRunId });
```

Only the latest plain user text enters the managed run. Prior browser messages
are UI history and cannot replace the runner's durable checkpoint. The native
body's AG-UI IDs must equal the stored mapping. The adapter rejects tools,
context, resume, parent runs, nonempty state/forwarded properties, multimodal
content and unknown top-level fields. It does not implement frontend tools,
interrupts, native approvals, reasoning, arbitrary data events, subagent events,
structured output or the AG-UI protobuf transport. An unsupported native runner
event ends delivery with `RUN_ERROR`; it is not converted into a fake tool or
approval event. The example does not qualify a particular web widget or claim
CopilotKit integration.

## Disconnect, replay and Stop

Every operation uses the existing shared managed-run lifecycle. Browser abort
or response cancellation **detaches delivery**. It does not call Runkite cancel.
The app must keep the turn mapping and inspect/rejoin that run instead of
silently dispatching a new one. A `RUNKITE_DELIVERY_ABORTED` error describes the
delivery, not a promise that remote execution stopped. Stop is a separate
authenticated `POST` to `routes.stop`; accepted cancellation does not undo any
side effect already committed by the agent.

Attach is read-only public stream replay. It never creates another thread or
run. Retained replay starts at the beginning of the current attempt, so appending
it to an existing partial response would duplicate text. Use a fresh client and
replace the response only after the replay confirms the exact stored application
and assistant identities and durable success:

```ts
import { reconstructManagedAGUI } from "./replay.ts";

const replacement = await reconstructManagedAGUI({
  attachUrl: ownedTurn.attachUrl,
  threadId: ownedTurn.aguiThreadId,
  runId: ownedTurn.aguiRunId,
  assistantMessageId: ownedTurn.assistantMessageId,
  messages: savedMessages,
  // Supply the application's session/CSRF headers or fetch here too.
});
// Commit only now. On failure keep the saved partial response and inspect status.
agent.setMessages(replacement);
```

Do not concatenate this replay with the original partial text. Stream retention,
run deletion and attempt replacement can make a full reconstruction unavailable.
A conflicting or missing retained attempt fails delivery; the helper does not
create a replacement execution. Application run IDs give normal retained-record
retry behavior, not deduplication forever after a run is deleted. There is also
an uncertainty window before the app receives and binds the first verified
assistant ID. Reconcile the stored operation through the public API and the
application's own retention policy before deciding to submit another operation.

The source packages inspected for this example are the
[official core package](https://www.npmjs.com/package/@ag-ui/core/v/1.0.4),
[official encoder package](https://www.npmjs.com/package/@ag-ui/encoder/v/1.0.4)
and [official client package](https://www.npmjs.com/package/@ag-ui/client/v/1.0.4).
The lockfile records exact package integrities. See
[the engineering note](../../docs/internal/managed-ag-ui-bridge.md) for the
implementation boundary and qualification scope.
