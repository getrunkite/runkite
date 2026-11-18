# Runkite engineering notebook

## Why keep this notebook

The useful thing to remember about a system is often the reason a tempting
shortcut was rejected. A release list can say that generation fencing shipped.
It does not necessarily help reconstruct the race that required it, why a run
ID was insufficient, or what a successful checkpoint still cannot prove about
an external effect.

This notebook connects Runkite's later engineering decisions into a study path.
It is intended for the maintainer returning to the project, a contributor
trying to understand a boundary, or a system-design discussion grounded in
code. Read it to recover the problem and reasoning, then follow the source and
regression tests rather than memorizing a feature description.

The original [architecture walkthrough](../architecture-deep-dive.md) already
explains the v0.2/v0.3 foundation at length. Later focused records also exist.
The gap is the connected account of how those foundations changed. This
companion preserves the old walkthrough and links the narrower records; it
does not replace them with another giant, supposedly timeless architecture
document.

## Edition and evidence

Written on November 18, 2026 against the implementation released as v0.10.0.
Its runtime tag identifies
[`33cf561`](https://github.com/getrunkite/runkite/commit/33cf561355e3b7284e1964de4b7f763d2925233b);
the existing publication/docs follow-up is
[`34f0421`](https://github.com/getrunkite/runkite/commit/34f0421923a8372be78745f1b46091640d2f2cb5).
The notebook itself is a later documentation addition, not part of those
immutable package payloads.

Three kinds of statement need different treatment:

- **Recorded history:** version introductions and observed failures are taken
  from the changelog and dated design/qualification records. A proposed feature
  in an old document is not proof that it shipped then.
- **Implementation fact:** code and tests show the current contract at the
  stated source. Test references identify evidence to inspect; this writing
  exercise does not claim to have rerun every linked test or a new production
  qualification.
- **Reconstructed rationale:** explanations of an alternative's consequences
  are architectural reasoning from those facts. Where an original record does
  not state a personal motive, the notebook does not invent what the founder
  thought or claim that a hypothetical production incident occurred.

Relative source links follow the checkout being read. For an exact historical
comparison, open the same path at the runtime tag above. For present operation,
use [Architecture](../../architecture.md), [Limitations](../../limitations.md),
[Trust and governance](../../trust-governance.md) and the
[operations runbook](../../ops-runbook.md). A study chapter should not silently
override a later supported contract.

## The connected reading path

1. **[Who owns the loop?](01-who-owns-the-loop.md)** Start with the original
   managed LangGraph path. Follow the application-owned SDK case that required
   a second decision door, and the separate managed adapters that preserve
   native SDK execution. Then separate browser chat, native history and durable
   completion. The question is which component owns which action, rather than
   how many frameworks can be named.
2. **[Recovery and original identity](02-recovery-and-original-identity.md)**
   Follow work after delivery becomes uncertain. Distinguish public run ID,
   record identity and attempt generation, then connect checkpoints, results,
   accounting, finalization and acknowledgement. The question is which fact
   makes a retry safe and which uncertainty must remain visible.
3. **[Authority, credentials and evidence](03-authority-credentials-and-evidence.md)**
   Derive authority from authenticated callers and assignments. Follow exact
   policy identities, review bindings, delegation, credential leases and
   evidence delivery. The question is where an operation can actually be
   refused, and what a recorded observation proves afterward.
4. **[Integrations, discovery and operator truth](04-integrations-discovery-and-operator-truth.md)**
   Connect external MCP callers, model routing, OTLP and application transports
   without importing their control planes. Then examine advisory discovery,
   registry metadata, cron history/preview and conservative UI output. The
   question is how useful information remains honest about its authority and
   freshness.
5. **[Proving and shipping the system](05-proving-and-shipping-the-system.md)**
   Follow the boundary from unit source to installed runtime, then from intended
   artifacts to actual public publication. The question is what evidence allows
   a claim, an upgrade or a recovery operation, rather than what a green badge
   appears to imply.

Use the [decision map and review prompts](decisions-and-review-prompts.md)
when returning after a break. It provides shorter retrieval paths and a worked
whiteboard outline; the chapters supply the explanations behind them.

## How the architecture reached this point

This is a chronology of recorded release contracts, not a reconstruction of
every development conversation. Verify individual introductions in
[CHANGELOG](../../../CHANGELOG.md).

**v0.1 established the distribution and managed-run foundation.** The public
preview shipped the Go plane, separate Python/TypeScript LangGraph runners,
container images, OpenAPI and Helm references. The important division was
already present: client-facing operations and framework execution belonged to
different processes.

**v0.2 and v0.3 made operations explicit.** SQL governance, connector approval,
admission and kill controls were followed by FinOps, opaque adapter checkpoints,
run manifests and additional Admin workflows. The historical walkthrough is
the best starting point for that foundation. Its older broad wording must be
read alongside the current contracts, especially for unsupported backends and
tools that bypass a gate.

**v0.4 through v0.5.3 exposed the difference between a tool name and an operation.**
Argument predicates and approval argument digests addressed what was actually
being approved. Live fixture replay and opt-in payload shrink introduced their
own execution and retention tradeoffs. v0.5.3 then let an application ask for
one tool decision while retaining its own loop. It did not create managed runs
or inherit run admission. [Chapter 3](03-authority-credentials-and-evidence.md)
explains why later approval bindings became stricter still.

**v0.5.6 through v0.6 changed both recovery and SDK reach.** Exact generation
fencing, persist-before-acknowledge behavior and strict MCP parsing closed
concrete failure cases. Managed AI SDK and Python SDK profiles made native
framework history, durable conversation commits and committed-turn replay
important operational contracts. Shared chat presentation and reconnect kept
application ownership separate. v0.6 also hardened SQL identities and other
runtime boundaries; [runtime-hardening.md](../runtime-hardening.md) preserves
those specific fixes. v0.6.1 changed licensing for new artifacts to Apache-2.0;
older tags retain their own shipped terms, as recorded in the
[transition note](../apache-2.0-transition.md).

**v0.7 and v0.8 expanded the evidence and authority model.** Managed action
evidence, tenant-scoped review, bound approvals, workload/delegation identity,
credential lifecycle, export and external execution controls added explicit
places to inspect and limit authority. The independent governance helpers
became published packages in v0.8.0. These releases did not make a cooperating
worker an unbypassable sandbox or constitute independent security assessment.
The [identity/distribution plan](../identity-and-distribution-plan.md) maps the
tracked RFEs to implementation and remaining qualification boundaries.

**v0.9 and v0.10 improved retry identity and the information around it.** Stable
public MCP operation IDs and clearer operator navigation were followed by
assignment identity protocol 1, original-record accounting/finalization,
scoped specialist observation, discovery and registry preservation, cron
visibility/preview and bounded ecosystem recipes. This required a coordinated
upgrade rather than silently treating missing identity as compatibility. The
[v0.10.0 publication record](../release-verification-v0100.md) distinguishes
actual public artifacts from source and fixture evidence.

## What to carry into a design discussion

The transferable ideas are relationships, not product names. A reusable public
ID is different from the identity of the stored record carrying it. A lease
does not establish that its holder is still authoritative. A checkpoint commit
does not atomically commit a remote payment. Approval of one configured action
does not authorize its changed neighbor. A schema helps a caller form input
without proving live readiness. A successful upload does not prove that users
can download the intended package.

The Go binary and embedded Admin make distribution convenient, but separate
runners and stateful backends remain part of the system. A design explanation
should include those operational costs. Likewise, discuss admission estimates
and reported usage without pretending to have a provider's invoice, or a
bounded fault rehearsal without presenting it as a production capacity result.

## Keep the notebook useful

For the next meaningful change, update the closest chapter's contract and add
the narrower design record when detail warrants it. Capture the triggering
caller or failure, the smallest counterexample, the invariant, rejected
alternatives, changed state/authority boundary, regression evidence, rollout
and conditions that would justify revisiting the decision.

Separate the date/source of an observation from the date the note was written.
Retain failed qualification outcomes when later work repairs them. Do not
retroactively turn a source-only example into a package, a reconstruction into
a personal memory, or a maintainer test into independent endorsement. Preserve
existing anchors when changing headings so earlier references keep working.
