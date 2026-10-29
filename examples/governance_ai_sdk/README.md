# App-owned AI SDK tools with Runkite governance

This runnable server keeps its Vercel AI SDK request loop and application state. Runkite decides whether its `createNote` tool is allowed. The note is a local application object, so the example performs no external write. The model provider is called only when you submit a new turn.

For a pending decision, the server persists and returns the plane's `actionId` immediately. An operator resolves that action in Runkite Admin. A later application request continues the exact stored operation. It never holds an HTTP request open while waiting for operator approval.

This example uses pinned `ai@7.0.155` and `@ai-sdk/openai@4.0.104` public `generateText`, `tool`, `jsonSchema` and `createOpenAI` APIs. It does not use the SDK's chat-user approval flags as a substitute for operator approval.

## Run

Use Node 22.13 or newer and a running self-hosted Runkite plane with a SQL backend. Build the local helper first; neither governance helper was published by this change:

```sh
cd typescript/runkite-governance
npm ci
npm run build
cd ../../examples/governance_ai_sdk
npm ci
```

Set these environment variables in the server process:

```sh
export RUNKITE_HTTP_URL=http://localhost:2026
export RUNKITE_API_KEY=your-server-side-governance-key
export APP_TOKEN=your-local-application-key
export OPENAI_API_KEY=your-model-provider-key
export MODEL=your-provider-model-id
npm start
```

The server binds only to `127.0.0.1:8088`. `PORT` and `APP_STATE_FILE` are optional. Keys stay in the server environment. The sample application token authenticates its local routes; the separate Runkite key needs `governance:decide` and the correct tenant. Do not give a browser either server-side provider key.

Add a matching grant and mandatory HITL rule to your existing Runkite configuration (or its existing Admin overlays). For a default-tenant key:

```json
{
  "policy": {
    "grants": [{
      "tenant_id": "default", "agent_id": "notes-app", "connector": "workspace",
      "tools": { "allow": ["createNote"] }
    }],
    "mandatory_hitl": [{
      "tenant_id": "default", "agent_id": "notes-app", "connector": "workspace",
      "tools": ["createNote"]
    }]
  }
}
```

`workspace` is a logical policy name; it does not require an MCP connector. `notes-app` need not be registered as a runner agent. Merge this policy with your real configuration and use your key's tenant. Removing the HITL rule exercises immediate allow. A missing matching grant exercises deny when policy is configured.

Submit a turn with a stable application idempotency key:

```sh
curl -s http://127.0.0.1:8088/turns \
  -H "Authorization: Bearer $APP_TOKEN" \
  -H 'Idempotency-Key: note-turn-1' \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Create a note titled Release plan with body Ship the reviewed changes."}'
```

If the model calls `createNote`, the response includes `jobs`. A pending job has its application `id`, the Runkite `actionId`, and the original arguments. Approve the action in Runkite Admin, then continue using the application job ID from that response:

```sh
curl -s -X POST "http://127.0.0.1:8088/actions/$JOB_ID/continue" \
  -H "Authorization: Bearer $APP_TOKEN"
curl -s http://127.0.0.1:8088/notes -H "Authorization: Bearer $APP_TOKEN"
```

Before approval, continuation returns HTTP 202 and the stored pending job. Rejected or consumed actions create no note. Approval creates one note. Concurrent or repeated continuations return the same stored note ID. The continuation route accepts no replacement tool arguments. Repeating `/turns` with the same idempotency key returns the existing turn and its jobs instead of calling the model again. Reusing that key for a different prompt fails.

## What the application owns

`state.ts` serializes mutations and atomically replaces one application state file. The note and its execution record change together, which makes continuation idempotent across concurrent requests and process restarts in this single-process example. `npm test` proves that behavior without a model or a plane. `npm run check` validates the SDK integration types.

For a deployed multi-replica app, use its own database transaction and unique operation key in place of this file. For an external side effect, also pass an idempotency key to the downstream API or use the application's existing outbox. Runkite approval alone cannot provide exactly-once external execution. A process failure during a model turn may leave that turn marked `processing`; repeating its key returns any recorded jobs without automatically repeating the model or tool calls. The application must decide how to recover an incomplete model turn.

The plane creates no thread or run for this caller. Admin Threads and Runs remain empty for these turns. Run kill switches, break-glass and spend caps do not apply. Polling an approved action does not consume it; application idempotency is essential. App chat-user consent, if your product needs it, remains a separate app concern. No second policy evaluator, credential vault or scheduler is introduced.
