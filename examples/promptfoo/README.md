# promptfoo managed-run provider

Checkout-only example included with v0.10.0 source. This is an application-side custom API provider, not a new Runkite runtime or permission boundary. It creates ordinary governed runs through public REST endpoints and observes their durable results. No Admin credential, simulation header, embedding provider, remote grader or direct model SDK is used by the provider.

The example pins promptfoo **0.125.1** and Node **22.22 or newer**. Compatibility is limited to the tested custom class loader, evaluator scheduler and CLI path. Requalify before changing the pin. The package pins narrowly scoped transitive resolutions, including upstream's Nunjucks watcher selection; see the [engineering note](../../docs/internal/promptfoo-managed-provider.md#evaluation-dependency-audit) for source evidence and compatibility limits.

From this directory:

```sh
npm ci
export RUNKITE_URL=https://your-runkite.example
export RUNKITE_AGENT_ID=your-agent
export RUNKITE_API_KEY=your-dedicated-tenant-key
export PROMPTFOO_DISABLE_TELEMETRY=1
export PROMPTFOO_DISABLE_UPDATE=1
npm exec -- promptfoo eval --config promptfooconfig.yaml --no-cache
```

Use an existing agent that accepts `input.messages` with one user message and returns final assistant text. The sample assertion expects `ready`; replace it with assertions appropriate to your agent. Live evaluation can incur the configured agent's normal model and tool costs. Give this dedicated caller only the permissions needed for public run creation and observation. The compatibility example uses tenant `read` and `write`, which are broader than a single specialist. Agent capability descriptions are advisory and do not grant authority or prove a runner is live.

An additional source-only qualification profile uses exactly
`agents:pfoo-fixture:run` and `agents:pfoo-fixture:observe`, without `read`,
`write` or Admin authority. It requires the matching integrated checkout with
scoped REST observation and SQL run references. Published v0.9.0 does not include
those permissions. Observation remains agent-wide, not requester-private. The
profile grants no listing, thread-history endpoint, stream, cancel or delete access.

HTTPS is required. `allowInsecureLoopback: true` permits only literal `http://127.0.0.1` or `http://[::1]` for a local fixture. Redirects are rejected. Do not put credentials in URLs, provider IDs, config templates or test variables. Runkite prompts and output remain sensitive: promptfoo stores evaluation artifacts independently from the provider's receipts.

Each intentional `callApi` creates a fresh run and thread ID. Repeats, a new CLI invocation, promptfoo `--retry-errors`, red-team strategies and explicit caller retries are new work. For the qualified ordinary evaluator, `handlesOwnRetries = true` and `config.maxRetries = 0` prevent the scheduler from replaying a whole call. This is not an exactly-once guarantee for arbitrary orchestration outside that evaluator.

Before dispatch, the provider writes a private immutable receipt containing destination, agent, run/thread IDs and a hash of the input. It fsyncs the file and containing directory. It sends **at most one POST** for that call, then uses only GET and GET/wait. A lost create response followed by 404 stays unknown. It never creates a replacement run merely because delivery or retention is uncertain.

Keep the receipt path returned in `response.metadata.runkite.receipt`. Recovery is explicitly read-only:

```sh
node recover.mjs /absolute/private/receipt.json
```

Use the same endpoint, tenant credential and agent. For a custom profile set `RUNKITE_PROMPTFOO_PROFILE`; for explicit JSON output set `RUNKITE_PROMPTFOO_OUTPUT_MODE=json`. To recover other custom provider settings, construct the same class and call `observeReceipt(path, { abortSignal })`. Recovery never calls the create endpoint. A missing retained run, mismatched agent/thread/input, changed reference or unavailable destination remains an error requiring investigation.

The receipt directory defaults to `.runkite-promptfoo-receipts` in the working directory. Set `RUNKITE_RECEIPT_DIR` to a canonical absolute path on a private local filesystem. The directory must be owned by the current user with mode 0700, and files use mode 0600. Symlinks, existing replacement files, unsupported hard links and failed directory fsync fail closed. Receipts contain neither credentials nor raw prompts/output. They do not defend against another process already running as the same user, filesystem rollback or manual receipt deletion.

When a server advertises `Runkite-Run-Reference`, observation saves it and sends `Runkite-Expected-Run-Reference` on later reads. Every later successful response must carry the exact saved reference. On published Runkite 0.9.0 the reference is absent: recovery checks retained run ID, thread, agent and input, but cannot prove the row was never deleted and recreated. Even a newer server's first reference acquisition cannot prove pre-acquisition history. Retain run rows and receipts throughout evaluation and recovery. This is an identity precondition, not an ETag or authorization token.

Text mode accepts only one unambiguous final assistant string. It rejects tool requests, reasoning channels, truncated finishes, content blocks and conflicting `result` fields. For a graph with structured output use `outputMode: json` and assertions against its values. Managed error/interrupted/timeout states become provider errors. Only consistent measured token counts and an explicit nonnegative `cost_usd` are mapped; missing or unmetered usage is omitted. These values do not establish invoice reconciliation.

The total observation deadline defaults to 60 seconds (maximum 5 minutes), each request to 10 seconds (maximum 30 seconds), and each JSON response to 1 MiB. Native `options.abortSignal` is honored. Timeout or cancellation detaches observation. It does not prove the managed worker stopped, and the example never automatically sends a cancellation request. The saved handle remains available for read-only recovery.

Validation:

```sh
npm test
# Starts the local Go fixture and invokes the real promptfoo CLI:
npm run qualify
# From the repository root, with local loopback networking available:
RUNKITE_PROMPTFOO_QUALIFY=1 CGO_ENABLED=0 go test ./internal/api -run '^TestNativePromptfooManagedRuns$' -count=1 -v
# From this example directory, only after integrating scoped run observation:
npm run qualify:scoped
# Equivalent from the repository root:
RUNKITE_PROMPTFOO_QUALIFY=1 RUNKITE_PROMPTFOO_SCOPED_SOURCE=1 CGO_ENABLED=0 go test ./internal/api -run '^TestNativePromptfooScopedManagedRuns$' -count=1 -v
```

The broad baseline remains the published-compatibility gate. The separate scoped
profile passed separately on the integrated source on 2026-11-16 using promptfoo
0.125.1 and Node 26.11.0. It repeats the same four
CLI evaluations with narrow credentials, requires four saved-reference waits,
and checks that unrelated public and Admin operations receive 403. An explicit
request to run it against an older checkout must fail rather than broaden the
credential. Reports identify `tenant-read-write` or `scoped-run-observe`; keep
separate report files when running both profiles.
The workflow requires both native gates on the integrated checkout and validates
both synthetic receipt schemas before uploading them. Each profile must create
four fresh threads inline through `if_not_exists: create`; the fixture pre-seeds
no threads and permits no separate caller thread-creation request.

The native interface test uses the installed promptfoo loader and scheduler with an in-memory HTTP fixture. The opt-in Go qualification uses the actual CLI, authenticated Runkite API, in-process queue and durable SQLite completion with a scripted worker. Its local equality assertions need no model credentials. It does not qualify any real LLM, remote provider, native framework runner, promptfoo UI/server, FTP, proxy, filesystem MCP, red-team strategy or arbitrary external retry wrapper.

The Go fixture lives in [internal/api/promptfoo_qualification_test.go](../../internal/api/promptfoo_qualification_test.go) and remains part of the root test suite. This Node-only directory has a minimal `go.mod` boundary so root `go list`, `go test` and linters skip promptfoo's installed third-party Go templates under `node_modules`. The boundary adds no Go dependencies. Run the qualification from the repository root or use `npm run qualify` here.
