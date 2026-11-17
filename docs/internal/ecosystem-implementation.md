# Ecosystem implementation and release gates

Status: active development after published v0.9.0. Changes below are Unreleased
until a later coordinated release explicitly lists them. Published tags and
packages remain immutable. No version number is selected by this plan.

The [public-project survey](ecosystem-review-20261116.md) identifies ideas worth
adapting, existing Runkite contracts and rejected duplication. This document
tracks the bounded implementation program. Comparing a project is not a claim
that its runtime has been integrated or that all its features belong here.

## Sequence

1. **Preserve registry metadata.** Admin edits must carry loaded opaque metadata
   through the existing replacement PUT. Preserve immutable older versions and
   omission semantics. Regression tests use the production form helpers.
   Tracking: [PR #73](https://github.com/getrunkite/runkite/pull/73).
2. **Scoped specialist access.** Separate invocation from agent-wide observation,
   filter MCP catalogs and check calls, bind stateful authority, and fail closed
   across retained run replacement. Qualify the real API, runner and specialist
   caller. This does not introduce requester-private thread ownership.
3. **Typed MCP discovery.** Enrich the existing nested input envelope from bounded
   usable reports. Preserve raw invocation, generic fallback and session snapshot
   semantics. Never fetch a remote schema or infer freshness.
4. **Capabilities and catalog requirements.** Show advisory configured-profile
   facts, explicit unknown values and fixed guide links. Preserve opaque metadata.
   An optional versioned catalog convention describes requirements and a safe
   sample, without installing code, resolving secrets or granting authority.
5. **Cron visibility.** Show tenant-correct retained thread and latest run.
   Preserve definition-only callers and scheduler behavior. Empty retained
   history, failed reads and pre-dispatch failures remain different evidence.
6. **Offline policy fixtures.** Add a separate bounded CLI over the current static
   engine. It must make no model, Vault, downstream or durable-state calls.
   Keep live simulation and logical regression testing distinct.
7. **Langflow MCP caller.** Qualify native pinned remote discovery and a real
   specialist invocation with a provider-free worker. Document setup, authority,
   retry retention and the difference between this path and the surrounding
   Langflow conversation.
8. **promptfoo provider.** Evaluate ordinary managed runs through public APIs.
   Preserve operation identity across transport retries and prevent an outer
   evaluator retry from silently creating another run. Qualify the pinned public
   provider interface and real local execution without an evaluator UI in Go.
9. **AG-UI text transport.** Translate application-owned managed run delivery into
   the public event protocol outside the plane. Reuse ownership, durable finish,
   attempt checks, detach and explicit Stop. Text and terminal lifecycle only;
   do not invent tool, interrupt or approval support.

Each slice carries implementation, meaningful regression tests, an engineering
note and operator/developer guidance. Admin changes rebuild the embedded bundle.
Public REST changes update the generator and checked-in OpenAPI output. Source
features are labelled Unreleased on support pages. Existing v0.9.0 installation
and package labels remain accurate.

## Qualification and publication

Use independently reviewable commits and pull requests under the maintainer's
personal identity. Run focused tests first, then affected suites and installed
or live local protocol checks. Keep reproducible receipts outside the product
tree and link only nonsecret public evidence in pull requests. A green hosted
pipeline is required before merging; infrastructure failures are investigated,
not hidden by weakening checks.

After all selected slices merge, run combined qualification on their exact
integrated tree, including tenant isolation, retry/reclaim and durable output.
Check the built Admin, static support links, engineering navigation, release
availability labels and package/artifact contents. The later release will list
only what was actually qualified. No finite suite proves absence of all bugs.

## Follow-on qualification: Langfuse OTLP

The [Langfuse recipe](../langfuse.md) is the next authorized slice. It reuses
existing v0.9.0 OTLP exporters with explicit per-language HTTP encodings, project
authorization and a separately operated receiver. The
[engineering note](langfuse-otlp.md) records passing actual-exporter wire checks
and local native Langfuse v4.81.0 persistence qualification on November 17, 2026.
The latter persisted seven ordinary spans from three services using the real
exporter/run-span/LangChain callback helpers, with no plane dispatch, framework
worker or model call. Its [reproducible qualifier](../../examples/langfuse_otlp/README.md)
and sanitized receipt keep that boundary distinct from a full managed-run or HA
qualification. The new workflow covers wire/offline checks only. This adds no
exporter, model wrapper or embedded backend, and does not change v0.9.0
availability labels.

## Follow-on qualification: OpenShell effective policy

The Unreleased [effective-policy fixture](openshell-effective-policy.md) reuses
the existing external OpenShell 0.1.8 JSONL workload and adds an optional
qualification-only checker. It captures current/loaded sandbox policy before
exec and after probes, compares the observed policy with the original boundary
under the pinned checker's model, and fails on unsupported inputs or observed
drift. The fresh provider-free local macOS fixture passed with TLS and confirmed
cleanup. Inspect the automatic Linux workflow's actual current result separately
from that measured Darwin/ARM64 evidence.

This adds evidence to RFE #47 and the RFE #52 reviewer handoff without closing
either tracking issue or independent assessment. It changes no production
worker or enforcement gate. Snapshot observations cannot prove atomic exec
binding, continuous kernel enforcement or absence of hidden provider state.
Existing HA/CNI qualifications remain separate and the profile stays
experimental.

## Deliberately outside this release gate

Temporal and Cloudflare caller examples remain demand-led. Broader OpenShell
composition, atomic exec binding and continuous enforcement require separate
contracts and qualification; the bounded snapshot comparison above does not
establish them.

Response inspection, optional tools-only connector profiles, per-fire cron
receipts and requester-private ownership require separate callers and designs.
Do not create a second vault, issuer, policy engine, workflow scheduler,
observability product or model loop. Do not copy another product's runtime or
UI into the binary. The existing Deep Agents path already uses LangGraph.

An integration receipt does not establish upstream endorsement, sandboxing,
business-effect idempotency or governance over native tools that bypass a gate.
