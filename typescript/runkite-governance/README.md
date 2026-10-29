# Runkite governance helper

This small TypeScript helper calls a self-hosted Runkite plane for tool decisions. Your application keeps its model loop, conversation storage and tool execution. This is not the Agent Protocol client SDK.

The package is prepared for distribution here. It has not been published by this change. To build and install from a checkout:

```sh
cd typescript/runkite-governance
npm ci
npm run build
npm pack
```

Install the resulting local tarball in your application, then import the package:

```typescript
import { createGovernanceClient } from "runkite-governance";

const governance = createGovernanceClient({
  baseUrl: process.env.RUNKITE_HTTP_URL!,
  apiKey: process.env.RUNKITE_API_KEY,
  timeoutMs: 10_000,
});

const decision = await governance.decide({
  agentId: "notes-app",
  connector: "workspace",
  tool: "createNote",
  args: { title: "Release notes", body: "A draft" },
  runId: "stable-app-turn-and-tool-call-id",
});
```

`allow` means the trusted application may execute the exact arguments it submitted. `deny` means it must not execute them. For `pending`, persist the returned `actionId` and the original arguments in the application's own database. On a later request, call `governance.action(actionId)` and continue only when its status is `approved`. Rejected or consumed actions must not execute. Do not replace the original arguments on continuation.

`resolve(call, { signal, timeoutMs })` is convenient for a bounded wait in a long-lived backend. Its total deadline includes the initial decision and all HTTP/polling. `decide` and `action` also have a client timeout, and accept an AbortSignal. Do not hold a serverless HTTP request open for a lengthy operator approval. See `examples/governance_ai_sdk` for an app-owned continuation and idempotent execution example.

Use a server-side API key with `governance:decide`; ordinary `read`, `write` and agent-run permissions do not grant that permission. Keep that key out of browser code. The tenant comes from the authenticated key. This path creates no Runkite thread or run, does not apply run kill switches, break-glass or spend caps, and does not execute the tool. Polling an approved action does not consume it. The application must enforce idempotency itself.

License: BUSL-1.1, with the repository's additional use grant. See `LICENSE`.
