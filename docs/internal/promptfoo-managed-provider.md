# promptfoo managed-run provider engineering note

Status: Unreleased example in current source. Do not attribute this integration to published Runkite 0.9.0 or advertise native runtime/embedding support. Code is under [examples/promptfoo](../../examples/promptfoo/README.md). No core dispatch, permission or API semantics change.

## Pinned caller contract

The qualification targets promptfoo tag [0.125.1](https://github.com/promptfoo/promptfoo/tree/0.125.1), release commit `8569e72`. Primary implementation evidence:

- [Custom provider factory](https://github.com/promptfoo/promptfoo/blob/0.125.1/src/providers/registry.ts): the file-provider factory imports the class and constructs it with provider options. A class retains `handlesOwnRetries` on the actual instance.
- [Provider interface](https://github.com/promptfoo/promptfoo/blob/0.125.1/src/types/providers.ts): `id`, `callApi`, `handlesOwnRetries`, optional caller context and `options.abortSignal`.
- [Scheduler retry budget](https://github.com/promptfoo/promptfoo/blob/0.125.1/src/scheduler/rateLimitRegistry.ts): `maxRetriesOverride: provider.handlesOwnRetries ? 0 : providerMaxRetries`.
- [Scheduler result predicates](https://github.com/promptfoo/promptfoo/blob/0.125.1/src/scheduler/providerWrapper.ts): rate limit detection and `metadata.retryableErrorKind === 'transient_availability'` can retry provider results. The native interface regression deliberately forces that predicate after real provider calls, sets a nonzero retry budget and verifies exactly one call per intentional repeat.
- [Evaluator](https://github.com/promptfoo/promptfoo/blob/0.125.1/src/evaluator.ts) and [response contract](https://github.com/promptfoo/promptfoo/blob/0.125.1/src/contracts/providers.ts): provider errors are errors, terminal assistant content is output, optional usage/cost is attached only when measured.

The provider uses public `POST /threads/{threadID}/runs`, `GET /runs/{runID}` and `GET /runs/{runID}/wait`. It fixes endpoint, agent and credential from configuration. Input is the raw text prompt wrapped once in `messages`; no prompt rewrite, input default application or separate model client is introduced. Existing Runkite admission, tenant authority, queue, attempt, completion and tool governance paths apply normally. Caller orchestration outside the pinned ordinary evaluator can deliberately call the provider again; it receives a new ID and is not mistaken for recovery.

## Delivery and recovery

Fresh IDs represent intentional calls. A private fsynced receipt exists before the single create attempt. All in-call retries and explicit `observeReceipt` recovery are read-only. Unknown delivery, an initial 404 visibility race, missing retention, incompatible identity or a bounded observation failure never authorize another POST. No automatic managed-run cancellation is claimed or sent.

Receipt binding compares endpoint/profile/agent, and retained responses must match run/thread/agent plus the canonical hash of input. A saved `Runkite-Run-Reference` is immutable. Future reads send `Runkite-Expected-Run-Reference` and require the same response reference. First reference acquisition and 0.9.0 compatibility without that header cannot prove prior row continuity. A receipt is neither authority nor a durable server-side deduplication guarantee across deletion/recreation.

Tests cover lost-create read recovery, initial invisible-row failure, no repeat dispatch, independent fresh IDs, unknown input identities, immutable references, missing/changed response references, cancellation/deadlines, response size, strict assistant selection, measured usage and filesystem privacy. The native loader/scheduler test is offline and exercises the actual installed package. The opt-in Go test invokes the real CLI against authenticated public Runkite endpoints and a scripted queue worker with durable completion. It counts requests and fresh run IDs. Local equality assertions make no model calls. This fixture does not attest framework execution, live agent readiness or arbitrary promptfoo features.

Local qualification on 2026-11-16 passed with promptfoo 0.125.1 and Node 26.11.0: four CLI evaluations produced four distinct managed runs, four creates, four reads and four waits. The native scheduler diagnostic also passed two intentional repeats with a nonzero external retry budget and no extra call. The patched dependency audit reported zero findings at every severity. The dedicated [CI workflow](../../.github/workflows/promptfoo-example.yml) repeats unit/native, vet and real-CLI qualification using Node 26.11.0, npm 10.9.3 and the Go version in `go.mod`. It uploads only an allowlisted synthetic summary, with no receipts, evaluation prompts/output or process logs.

The first-party Go fixture is [internal/api/promptfoo_qualification_test.go](../../internal/api/promptfoo_qualification_test.go). Run it from the root with `RUNKITE_PROMPTFOO_QUALIFY=1 CGO_ENABLED=0 go test ./internal/api -run '^TestNativePromptfooManagedRuns$' -count=1 -v`. The Node-only `examples/promptfoo/go.mod` is a module boundary, with no Go dependencies, that keeps root Go tools from discovering installed upstream Go templates. It does not move or suppress the first-party fixture, which remains in the root API test suite and keeps its explicit native-runtime gate.

The baseline retains tenant `read`/`write` authority and does not require run
reference headers, preserving the published REST compatibility check. A separate
`TestNativePromptfooScopedManagedRuns` runs behind both
`RUNKITE_PROMPTFOO_QUALIFY=1` and `RUNKITE_PROMPTFOO_SCOPED_SOURCE=1`. It uses only
`agents:pfoo-fixture:run` and `agents:pfoo-fixture:observe`; no provider behavior
or runtime permission code changes. Both native profiles passed on the integrated
source on 2026-11-16 with promptfoo 0.125.1 and Node 26.11.0. Each produced four
distinct runs and threads with four reference-pinned waits. The scoped profile
also passed all six denial probes. These are separate qualification receipts.

The source profile requires every native result to report acquired reference
evidence and all four waits to send the saved expected reference and receive the
same response header. Six direct real-middleware denial probes cover broad run
and thread lists, stream, cancellation, thread deletion and Admin overview.
The synthetic report records the permission profile, pinned-wait count and
denial-probe count without recording references. This remains agent-wide
observation, not per-requester ownership. Keep baseline and scoped report paths
separate. The dedicated workflow requires both profiles on the integrated
checkout. Its strict 8 KiB receipt validator rejects duplicate or unexpected
keys, wrong field types, invalid numeric constants, and incorrect counts or
profile markers. It uploads both summaries only after both validate. The scoped
native gate must pass on each tested integration before advertising that result.

Both profiles create fresh threads through the existing single run-create
request with `if_not_exists: create`. The fixture seeds no thread and permits no
separate caller `POST /threads`. It verifies that each thread is absent before
the request and that four managed threads exist afterward. This demonstrates
inline thread creation through run authority, not a new thread-write grant or a
claim that every underlying database write shares one transaction.

## Evaluation dependency audit

Keep promptfoo 0.125.1 pinned while qualifying this contract. Its unmodified closure included four vulnerable packages on 2026-11-16, with propagated audit findings through parents. The example pins overrides to the official patched releases:

- `@hono/node-server` 2.1.6 fixes [WebSocket upgrade reset](https://github.com/advisories/GHSA-qv5m-9pm6-hqp3) and [backslash path parsing](https://github.com/advisories/GHSA-jpvv-r467-5p4w). The original promptfoo dependency pinned 2.1.5. This example never starts the promptfoo web server.
- `basic-ftp` 6.2.4 fixes [listing parser CPU growth](https://github.com/advisories/GHSA-c475-qrg2-pj4r), [control character exception](https://github.com/advisories/GHSA-r4jh-29pc-gfqr) and [MLSD inherited handler lookup](https://github.com/advisories/GHSA-5pm8-86m8-9m6r). The path is promptfoo → proxy-agent → pac-proxy-agent → get-uri → basic-ftp. The [6.0 breaking change](https://github.com/patrickjuchli/basic-ftp/blob/v6.2.4/CHANGELOG.md) disables separate FTP transfer hosts by default. The managed provider uses native fetch, fixed HTTPS and no FTP/PAC path.
- `glob` 14.0.2 fixes [post-abort traversal](https://github.com/advisories/GHSA-3g3m-2fjr-6pw4); `minimatch` 11.0.1 fixes [pattern amplification](https://github.com/advisories/GHSA-g3hv-75wg-m249). The original direct and optional filesystem-MCP paths resolved glob 13/minimatch 10. These major overrides preserve the APIs used by the qualified local CLI path; they do not claim compatibility with every upstream plugin, untrusted pattern or filesystem MCP server.

### Watcher dependency removal, November 18, 2026

The reviewed [picomatch advisory](https://github.com/advisories/GHSA-fc5q-39xc-vjpf)
now covers every published version through 4.0.7 and lists no fixed release.
Our old example lock reached picomatch 2.3.2 through Nunjucks 3.2.4's optional
Chokidar 3 watcher, producing six propagated high findings. Upgrading picomatch
within that affected range would not resolve it.

Promptfoo 0.125.1 already selects Chokidar 5.0.0 for Nunjucks in
[its own package overrides](https://github.com/promptfoo/promptfoo/blob/0.125.1/package.json).
Overrides inside a dependency do not propagate to a consuming project's root.
The example now mirrors that resolution with `nunjucks: { chokidar: "5.0.0" }`,
keeping promptfoo and all existing security pins unchanged. The regenerated npm
10.9.3 lock removes the old watcher closure, including every picomatch copy;
it introduces no new package artifacts. This removes affected code rather than
ignoring an advisory or replacing it with an unreviewed fork.

Fresh `npm ci` and `npm audit --audit-level=low` report zero findings at all
severities on this date. All 32 example tests pass. The actual promptfoo CLI
passes both baseline and scoped profiles on Node 26.11.0, with four distinct
managed runs and four reference-pinned waits per profile. Retained local
receipts are under `outputs/runkite-pr93-picomatch-20261118/`; hosted CI must
qualify the committed follow-up separately.

The qualified evaluator renders without template watching. Generic Nunjucks
watcher compatibility is outside this recipe: `NodeResolveLoader({ watch: true })`
calls Chokidar without paths, which version 5 rejects. This change does not
qualify arbitrary Nunjucks applications, watch mode or promptfoo's web server.
The separate Admin Vite build dependency still reaches affected picomatch;
a clean promptfoo audit does not resolve that release gate.

Use the committed lockfile with `npm ci`; do not use `npm audit fix --force`, silently downgrade promptfoo or assume unused paths justify shipping known findings. Repeat the audit and native qualification when changing this dependency closure. Promptfoo's broad optional ecosystem remains outside this example's feature claim.
