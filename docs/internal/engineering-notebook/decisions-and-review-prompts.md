# Decision map and review prompts

[Notebook home](README.md)

Use these cards to reconstruct an explanation, not to recite a prepared claim.
Each answer should identify a caller, a failure sequence, the invariant, a real
implementation point and the remaining limit. The examples are study scenarios,
not claims about production incidents or measured customer scale.

## Why separate a Go plane from SDK execution?

The plane provides shared operational contracts while separate workers import
the SDKs and own their model loops. One distributable plane binary does not
mean one process runs the whole deployment. The tradeoff is an explicit worker
protocol and more failure boundaries in exchange for separating framework
dependencies and execution from plane deployment. Revisit the deployment
shape for a concrete caller, not because another SDK places its server and UI
in one repository. Start with [chapter 1](01-who-owns-the-loop.md).

## Why have both managed runs and app-owned decisions?

An application that owns execution can wait before a tool without surrendering
its loop. It cannot obtain managed-run visibility and cancellation simply by
submitting a correlation ID. A managed runner is the separate path for work
Runkite admits and dispatches. Explain why inventing an Admin run from partial
application reports would overstate authority. Reconsider external-run tracking
only for a real caller with a clearly cooperative contract. Compare
`handleGovernanceDecide` and `admissionGate.Decide` in
[chapter 1](01-who-owns-the-loop.md).

## Why are run ID, record identity and generation separate?

A public name can be reused, a physical run record can be replaced, and one
record can be retried. Those are different transitions. Show a stale request
that was valid when it started and reaches its write after a replacement.
Then explain why HTTP preflight, a blob ETag or a reusable run ID alone cannot
prove current assignment authority. Read the record/write fences in
[chapter 2](02-recovery-and-original-identity.md).

## Why not acknowledge a job as soon as its agent returns?

The agent result, native history, durable run result, accounting/finalization
and queue state can disagree after failures. Trace a committed result whose
acknowledgement is lost, then a database write that fails before completion.
The recovery actions should differ. Finally place a remote side effect before
the checkpoint commit and state the uncertainty that remains. Do not erase
that window with an exactly-once label. Follow
[chapter 2](02-recovery-and-original-identity.md).

## Why bind approval to more than a displayed tool name?

The same name can carry different arguments, tenant scope, original run,
configured destination, policy or observed schema. Identify which of those
facts is actually bound on the path being discussed. Explain why redacted
display data is not necessarily a lossless authorization payload and why
approval alone neither dispatches a tool nor resumes every SDK. Then explain
which app-owned decisions cannot be recalled after handoff. Read
[chapter 3](03-authority-credentials-and-evidence.md).

## Why coordinate credentials without building another vault?

Vault retains issuance and revocation authority. Runkite needs enough private
cleanup state and public metadata to bind a configured lease consumer to the
intended operation and reconcile cleanup. Explain why long-lived user-token
storage, arbitrary secret issuance and public exposure of cleanup handles are
different responsibilities. Include expiry, failed cleanup and process loss in
the discussion. Follow [chapter 3](03-authority-credentials-and-evidence.md),
then the focused credential lifecycle record it links.

## Why are discovery and operator displays deliberately conservative?

An input schema is useful without attesting the live worker. A declared
registry requirement is useful without installing it. A projected cron time
is useful without promising a fire, and a retained run row is not a fire
ledger. Explain which extra authority, freshness or durable observation would
be needed for each stronger claim. The same discipline keeps an ambiguous
tool/reasoning result out of a final-answer preview. Read
[chapter 4](04-integrations-discovery-and-operator-truth.md).

## Why integrate through protocols rather than absorb products?

Choose a concrete path: an external MCP client calls one specialist, an SDK
runner sends model traffic to a configured router, or OTLP exports traces to
an external backend. Name the state and authority each side retains. Explain
what would become duplicated by adding another orchestrator, credential vault,
policy engine or UI to the binary. An upstream protocol implementation still
needs bounded interoperability evidence. Follow
[chapter 4](04-integrations-discovery-and-operator-truth.md).

## Why distinguish source, package and observed publishing authority?

A passing source test cannot prove the installed distribution's contents. A
configured publisher cannot prove that its live OIDC exchange succeeded.
Matching public files after recovery cannot prove a second upload occurred.
Use the Node dispatcher and partial-registry-publication cases in
[chapter 5](05-proving-and-shipping-the-system.md) to explain how a correct
claim depends on the exact boundary tested. Include immutable source, artifact
identity, original failed outcomes and the scope of the recovery controller.

## A whiteboard exercise that connects the chapters

Design an application that asks a managed specialist to prepare a refund,
requires review before a connector call, streams its answer and survives a
browser disconnect plus worker loss. Before drawing components, clarify
whether the model loop is app-owned or runner-owned. Those choices lead to
different guarantees; do not silently combine them.

For the managed variant, walk through this sequence:

1. The application authenticates its user and persists its operation mapping.
   The caller's authority scopes which specialist it may invoke and observe.
2. The plane admits a run and assigns work with original-record identity and
   generation. Describe where durable state and queue state can diverge.
3. The runner loads native history and executes. A connector call resolves its
   actual policy/credential path and any required review. A browser closing
   delivery does not itself establish cancellation or rollback.
4. Inject failure once before a state commit and once after it. Identify what
   the replacement worker can prove. Add a reused public run ID and explain
   which stale operations must lose authority.
5. Reattach presentation using application-owned identities and durable status.
   Distinguish an accepted Stop request, a failed delivery and a successful run.
6. Describe the accounting and evidence left behind. State which observations
   require reconciliation with the refund service rather than inference from
   Runkite alone.

Now repeat the design with the application keeping its loop and using only
`/governance/decide`. Remove every managed-run guarantee that no longer follows.
This is a stronger test of understanding than adding more boxes to the first
diagram.

## Questions to answer honestly in an interview

- Which behavior did the repository's regression test reproduce, and which
  failure modes are only hypothetical extensions of that example?
- Which latency, throughput, cost and recovery measurements can be traced to
  a dated qualification record? If no appropriate measurement exists, explain
  the measurement plan instead of inventing a production number.
- What did the small interface simplify, and which per-framework work remained?
- Where did refusing uncertain work hurt availability, and why was that cost
  accepted? When would that decision need revisiting?
- What would a stronger guarantee require: provider idempotency, an atomic
  write capability, a durable occurrence ledger, an execution boundary or a
  new product contract?

For personal reflection, add a dated note after a meaningful change: what
surprised you, what evidence changed your mind, which alternative you actually
considered and what you would choose under different constraints. Keep those
first-person observations separate from this repository-derived retrospective.
