# Showing the operational layer

The November 2026 presentation pass follows v0.10.0. It changes the static
website and onboarding guidance, not the control plane or a published package.
The question behind it was whether a visitor could see the work the product
already does, before having to understand its protocols.

## Lead with the job, then explain the system

The homepage already had two useful execution animations and an accurate
managed versus app-owned comparison. Its strongest visual material arrived
before much explanation of why an operator would need it. Framework names and
topology showed compatibility; they did not show reviewing an operation or
investigating a completed run.

The revised order puts an Admin product tour ahead of the execution diagrams.
The tour connects three ordinary operator tasks: inspect a run, review a
connector request and inspect a policy decision. Four short explanations then
cover credential resolution, target-bound review, worker recovery and usage.
Each leads to its specific support guide. The existing Agent Protocol and
native SDK views remain available, with their playback behavior preserved.

We kept the dark amber identity and existing captures. Another framework logo
row, simulated traffic counter or animated security score would add activity
without explaining an enforced capability. There are no invented customers,
endorsements, performance measurements or live-demo claims.

## Images are evidence of a screen, not every feature beside it

The tour reuses the actual [Run Detail](../../site/assets/ui-run-detail.png),
[Pending](../../site/assets/ui-pending.png) and
[Audit](../../site/assets/ui-audit.png) captures. They are labeled recorded
sample data. A CSS crop reduces unused space; the original image remains
available through a full-size link.

The Audit capture shows policy audit, including a sample break-glass decision.
It does not show the newer connector action evidence screen. The text and alt
description distinguish them. [Connector action evidence](../trust-governance.md)
is opt-in, SQL-backed and scoped to outgoing managed MCP tool attempts. A
recorded response is an observation, not proof of an external business effect.

## Preserve the product contracts in the copy

- [Secret references](../../internal/connector/secretref.go), through
  `ResolveSecretRef`, resolve environment, file and Vault values. MCP uses a
  plane-side proxy. Non-MCP sessions can return credentials to the runner.
  We do not describe all runner credentials as permanently hidden from workers.
- [Pending review](../../internal/api/admin_pending.go), including
  `checkPendingRevisions`, checks the current approval bindings. The
  [reviewer guide](../../site/support/tenant-review.html) documents configuration,
  requester separation and distinct-reviewer requirements. These are configured
  controls, not a claim that every installation enables them.
- [Recorded connector calls](../../internal/api/connector_action.go), through
  `handleRecordedConnectorAction`, bind evidence to the run, generation and
  policy context. SQL support and required audit remain explicit in the guide.
- [Incoming MCP](../../internal/api/mcpserver.go), through `newMCPServer` and
  `mcpToolHandler`, exposes configured agents to authorized clients and uses
  normal managed admission. This direction is separate from the outgoing
  connector gate. The homepage now links the existing [MCP guide](../mcp-server.md).
- Managed workers execute the native framework loop. App-owned decision callers
  keep execution and continuation in their application. The latter path creates
  no managed Thread or Run and does not inherit run kill switches or spend caps.

The current feature guides remain the detailed contracts. A landing-page
summary must not imply universal native resume, exactly-once external effects,
automatic governance of local tools or a security ranking.

## Progressive enhancement instead of another frontend runtime

[product-tour.js](../../site/product-tour.js) adds a small tab interaction to
static sections. It validates unique IDs and a complete one-to-one tab/panel
mapping before hiding content. Native buttons provide activation. Roving focus,
arrow keys, Home and End support keyboard navigation.

Without JavaScript, all sections and images remain readable. Invalid markup
also retains that fallback. Only valid initialization adds `data-enhanced` and
exposes the controls. The tour has no timers, autoplay or remote requests. It
does not change the original walkthrough or execution animation controllers.
GitHub Pages still serves the site; there is no new hosting dependency.

The controls use a content-responsive grid rather than three forced columns.
At narrow widths with enlarged text, they can stack instead of clipping their
labels. Inline action links also wrap within their cards. A 320 pixel preview
with doubled root text exposed both problems; both were corrected before
publication. This text-size fixture is distinct from native browser zoom.

## A first run should end in a result

The old homepage recipe started a plane and a runner but stopped before a
request. It also combined a moving checkout with an unpinned package. The
replacement pairs v0.10.0 source and `runkite-runner==0.10.0`, uses a virtual
environment and submits the provider-free echo graph to `/runs/wait`.

The recipe uses the same `CGO_ENABLED=0` static build as the release configuration.
This also avoids depending on the local C linker for the source example.
`cmdDev` in [serve.go](../../cmd/serve.go) accepts the explicit HTTP and gRPC
ports. [The echo graph](../../examples/echo_agent/graph.py) returns the submitted
message without a model account. The durable wait response must show a
successful run and the echoed message before we describe the example as working.

The support journey starts with that echo smoke test, then adds approval
behavior. The local unauthenticated profile is development-only. Supported
deployment and upgrade guidance remain separate, linked steps.

## Validation and its limits

Run `make test-site` for local links and UI regressions, including
[the tour tests](../../tests/site_product_tour_test.cjs). Render the homepage at
320, 390, 768 and 1440 pixel widths; exercise keyboard navigation, both execution
views, playback controls and enlarged text. Check the full page rather than
only the first viewport. A passing DOM fixture does not prove layout or
screen-reader behavior.

For this pass, the release source built with static Go settings, a fresh virtual
environment installed the published runner, and a real local echo request
returned `run.status: success` with `Hello, Runkite!`. Local Admin returned HTTP
200. This provider-free check validates the documented path; it is not live
provider, production HA or independent accessibility qualification.

Publish through the existing Pages workflow, then verify the live HTML, script,
styles and changed support pages. Keep the public Admin availability route
honest while its shared backend is offline. A website change does not require
moving the v0.10.0 tag or republishing its artifacts.
