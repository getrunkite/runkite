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

## Guard an existing tool executor

`createGovernedExecute` wraps a single-result execution function. It does not wrap a whole SDK tool, change its schema, or take over the model loop. Its config accepts:

- `client`: a governance client with `decide`.
- `identify(input, options)`: synchronous application code returning `agentId`, `connector`, `tool`, a required stable `runId`, and optionally `tenantId`. This `runId` identifies an application operation; it does not create a Runkite run. Identity strings must be nonempty, have no surrounding whitespace, and fit the server's 256 UTF-8 byte limit.
- `execute(input, options)`: called only after `allow`.
- Optional `onDenied(input, options, decision)` and `onPending(input, options, decision)`: return the same output type as `execute`. A pending callback must persist the action ID and original arguments in the application's store before returning. Without a matching callback, the guard throws `GovernanceBlockedError`, whose `decision` contains only decision metadata.

The returned `(input, options) => Promise<Output>` calls `decide` once and never polls, retries, resumes, or consumes an approval. A malformed decision, a pending response without an action ID, or a transport/callback error fails closed. It does not retry a partially completed side effect. Repeated invocations still need application idempotency. SDKs may convert a thrown tool error into a model-visible result and continue their loop, so use application callbacks to record blocked operations when required.

Input must be a plain JSON object. Nested objects, arrays and JSON primitives are supported. Before the first await, the guard copies and deeply freezes the input, captures the operation identity and passes that same snapshot to the decision client and matching callback. Plain object copies have a null prototype; use own fields or `Object.hasOwn`, not inherited object methods. This preserves case and own keys such as `__proto__` without creating prototype properties. It rejects data that could change or disappear on the JSON path: accessors, custom prototypes such as `Date`, symbols, nonenumerable properties, functions, `undefined`, `BigInt`, nonfinite numbers, negative zero, unpaired Unicode surrogates, cycles, sparse arrays and extra array properties. Root strings, arrays and null are rejected because tool arguments must remain an object across server parsing.

Framework `options` are passed through separately and are not policy arguments. Put every authorization-relevant target or value in input. Pass `options.abortSignal` to cancel the decision. The guard checks cancellation again before any application callback; the executor must cooperate with cancellation after it starts. AsyncIterable/streaming results are unsupported and rejected. See the runnable [AI SDK example](../../examples/governance_ai_sdk/README.md) for typed `tool.execute` use, pending persistence and later continuation.

Use a server-side API key with `governance:decide`; ordinary `read`, `write` and agent-run permissions do not grant that permission. Keep that key out of browser code. The tenant comes from the authenticated key. This path creates no Runkite thread or run, does not apply run kill switches, break-glass or spend caps, and does not execute the tool. Polling an approved action does not consume it. The application must enforce idempotency itself.

License: BUSL-1.1, with the repository's additional use grant. See `LICENSE`.
