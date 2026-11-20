# Runkite landing (GitHub Pages)

Product site source. `.github/workflows/pages.yml` publishes this folder as a
GitHub Pages artifact. Once the custom domain and DNS are configured, the same
deployment serves https://getrunkite.com/; the project URL
https://getrunkite.github.io/runkite/ redirects to the canonical domain.
Visuals live in `assets/`.

**Website:** https://getrunkite.com/

**Brand system:** [`brand.css`](./brand.css) is the shared NOC canvas (Fraunces + IBM Plex Sans + JetBrains Mono, amber). The product landing ([`landing.css`](./landing.css)) uses a product-first hero, two execution paths and a self-hosted topology. Support and design notes retain the shared brand.

**Routes (uniform multi-page nav):**

- [`/`](./index.html), product landing
- [`/support/try.html`](./support/try.html), try path
- [`/support/`](./support/). Docs handbook (catalog + feature guides + chapters + decisions)
- [`/support/integrations.html`](./support/integrations.html), ecosystem use cases, setup paths and tested scope
- [`/support/sdk-integration.html`](./support/sdk-integration.html), execution ownership and SDK integration paths
- [`/design/`](./design/), engineering notes hub (fencing, subscribe-before-enqueue, createRunCtx, poison pill)
- [`/admin/`](https://getrunkite.com/admin/), retained public Admin availability route

`try.html` at the site root redirects to `/support/try.html`.
`www-getrunkite/index.html` is a compatibility redirect to the canonical product
landing. Keep those stubs as redirects, not alternate copies of the product page.

## Source and release labels

The Pages workflow uploads this directory as static HTML whenever `site/**`
changes reach `main`. Editing Markdown in `docs/` does not regenerate this
handbook. Keep the public HTML and its linked Markdown references aligned.

There is no second static-site deployment to a separate web server. Configure
the canonical custom domain in the repository's Pages settings and DNS, then
verify its HTTPS response after the Pages workflow completes. The publishing
source is GitHub Actions, so a source-tree `CNAME` file is not required and is
ignored by Pages. See [the hosting runbook](../docs/site-hosting.md) and the
[GitHub domain instructions](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site).

The public demo backend is currently offline. Keep Admin links and `/admin/`
available as a small static status page with local setup guidance. Availability
belongs on that route and its link tooltip, not in a site-wide alert. The static
website does not provide Agent Protocol APIs, SSE/WebSocket execution or the
runner's gRPC service. A future backend can be connected without replacing
every published Admin link; the hosting runbook describes the boundary.

The SDK integration guide separates the released decision API, the separately
published decision helpers (v0.8.0+), and the managed AI SDK backend (v0.5.8+).
The managed UI bridge remains a checkout example. A checkout feature must not
be advertised as available in an existing npm/PyPI install. When a feature is
released, update these labels together with the changelog and package install
guidance. Keep public install commands unpinned unless an API
compatibility example specifically requires a tested SDK version.

The homepage's `#packages` catalog groups published artifacts by their role:
the Go control plane, managed Python/Node runners and server-side decision
helpers. The Install page's `#packages` table lists the binary release, four
registry distributions and three container images. Keep direct registry links,
image names and those role boundaries aligned with the Release workflow and
package manifests. This grouping lets visitors choose a deployment and an
execution boundary without mistaking the list for eight required installs.
JavaScript and TypeScript share the npm governance helper; a managed Node
runner does not depend on it. Go is needed for source builds, not for a
downloaded control-plane binary. Framework extras belong to their package;
the chart and examples remain source assets. The hero's package shortcut
should reach this catalog without changing the existing walkthrough entry.

The product is self-host only. The offline public demo is not a hosted
control-plane offering. Keep model-loop ownership explicit: managed runners
produce Threads/Runs; app-owned decisions do not. Claims about approvals, kill,
spend, native resume and durable state must name the boundary that enforces them.

For screenshots and engineering proofs, preserve historical evidence and its
scope. Do not turn a single-plane backend test into a multi-replica HA claim.

Local preview:

```bash
cd site && python3 -m http.server 8765
```

Then open http://127.0.0.1:8765/

## Presentation assets and behavior

The product story leads with operating outcomes, then shows the architecture.
`#operations` contains a three-view Admin tour: inspect a managed run, review a
connector request and inspect a policy decision. These are recorded sample-data
captures, not a live session or screenshots of the newer connector action
evidence screen. Full-size links preserve access to the uncropped captures.
`product-tour.js` progressively enhances the static sections into keyboard
tabs. It validates the complete mapping before hiding any content. Without
JavaScript or with invalid markup, the controls stay hidden and every section
remains readable. The tour never advances automatically or changes playback
preferences in the existing animations.

Keep the outcome copy tied to its actual enforcement boundary. The incoming
public `/mcp` agent interface is distinct from the outgoing connector proxy.
The first-run source recipe pins matching plane and runner releases, creates a
virtual environment and ends with a provider-free `/runs/wait` result. This is
an intentional version-paired example; generic package-install guidance can
remain unpinned. When advancing it, verify the complete recipe and result
before changing both versions. See the
[presentation design record](../docs/internal/product-presentation.md) for
the source references and rationale.

The homepage uses the real `ui-runs.png` Admin capture, explicitly labeled as
demo data. `landing.js` starts the existing walkthrough GIF automatically unless
the visitor requests reduced motion. Pause restores the still; Play restarts the
walkthrough. A load failure restores the still and marks the control unavailable.
With JavaScript disabled, all copy, navigation and the static product image
remain available; the inactive Play control stays hidden.

The separate `execution-flow.css` and `execution-flow.js` restore the animated
managed-run diagram near the top of the page, also linked from the hero. Its two
views explain the same managed execution model: the original Agent Protocol /
LangGraph client entry, and framework runners with model calls. Keep the
original client path discoverable before the framework profiles. View controls
are progressive enhancement; both labeled views remain readable without
JavaScript, and a browser without animation support must still display their
content and navigation.

The diagrams illustrate protocol handoffs, not live telemetry. Model calls
originate in the runner; the optional MCP branch returns through the plane's
connector gate. App-owned decisions are a separate path. Both views start in
play mode unless reduced motion is requested. Desktop uses SVG particles;
mobile pulses its existing directional arrows while keeping desktop SVG paths
hidden. Only the selected, visible flow animates. Offscreen and hidden-tab
suspension preserve play intent; the control continues to offer Pause. A manual
Pause survives view and viewport changes. An explicit Play can override the
initial reduced-motion default, while enabling reduced motion pauses playback.
JavaScript-disabled views retain the labeled topology. Keep the desktop and
mobile arrow directions consistent when changing this diagram.

`assets/runkite-social.svg` is the editable social-preview source. Its
1200 by 630 JPEG render is used by Open Graph and Twitter card metadata. Keep the
JPEG in sync after editing the SVG. `assets/runkite-mark.svg` is the local favicon.
The root README uses `docs/assets/runkite-overview.svg` for the two integration
paths. These diagrams explain product boundaries; they are not live activity or
screenshots. Existing screenshots and engineering evidence remain unchanged.

The homepage links to the published Runkite integration guide on the vLLM
Semantic Router blog, directly below the framework strip. Keep its attribution
explicit. Publication is an integration walkthrough, not an endorsement,
partnership or production-adopter claim. The entire callout is one native link,
including its logo and background, with one keyboard focus target. It is static
HTML and does not depend on JavaScript or a third-party embed. The unmodified
publisher logo and its source/license notice live in `assets/`; preserve that
attribution when updating the artwork. Framework names link to their individual
contracts in the SDK guide. The "Bring your framework" introduction opens that
guide too.

The homepage's "Works with your stack" section links to the public integrations
hub and named support recipes. Explain the task each connection enables, not
just the project name: evaluate a managed agent, call a specialist, deliver
chat, export traces or route model requests. Keep the hub reachable from the
docs catalog as well as the homepage. Its role is discovery, not a second
copy of each integration's setup instructions or qualification history.

Keep availability visible beside the claim. A published runner, a checkout
caller example, a configuration recipe and an experimental workload fixture
are different deliverables. Third-party names describe interoperability, not
endorsement or adoption. In particular, the promptfoo provider qualifies the
ordinary evaluator path with a scripted worker. Its bundled `ready` smoke test
does not establish automated red-team coverage or verify actual tool effects.
Link to the task guide for setup and to source records for the precise evidence.

Use the same visible destination cues on homepage action links and framework
profiles: `→` for another site page, `↗` for an external destination, and `↓` for
a section of the current page. Keep these decorative spans `aria-hidden="true"`
so screen readers announce the link's name. Navigation and underlined prose
links do not need extra arrows. Match hover feedback with visible keyboard
focus, and reserve selected-page styling for the actual current page.

Engineering articles have static section links and named keyboard-scrollable
diagrams. Preserve those targets when changing headings or diagrams. Handbook
code blocks are focusable groups so keyboard users can scroll long commands;
the Copy control must still copy only the original code. Keep the Admin entry
point labeled "Open Admin" while its destination explains shared-demo
availability, without a misleading current-page highlight.
Wrapped handbook navigation scrolls with the page so enlarged text cannot
obscure linked section headings.

Keep runtime ownership explicit when editing the page: managed executions create
Threads and Runs; app-owned tool decisions do not. Connector policy requires the
connector gate. The managed UI bridge remains a checkout example, while the
decision helpers are independently published on npm and PyPI from v0.8.0.
Current source uses Apache-2.0 and may be described as open source. Keep the
product self-host only. The homepage links to the latest verified release.
v0.6.1 was the first Apache-2.0 release. Published v0.6.0 retains its shipped BUSL-1.1 terms;
changing the source license does not update old registry artifacts or tags.
The footer links to the current Apache-2.0 license. Keep the historical
distinction in licensing and release documentation. Published framework rows
link to their integration contracts.

For a presentation change, check the homepage at 320, 390, 768 and 1440 pixel
widths, keyboard focus and Play/Pause behavior. Verify local links, SVG/JPEG
dimensions and all images before allowing the existing Pages workflow to publish.
Also inspect the entire page from hero to footer at wide and narrow widths,
including each flow view and the operations section. Check text reflow at 200%
browser zoom, not only page width: compare text and card bounds, look for clipped
copy or offscreen controls, and confirm short introductions do not leave a large
empty column beside long content. A successful link scan does not validate those
visual conditions.

Run `make check-doc-links` after editing the public HTML. The checker validates
local links, section anchors, CSS assets and GitHub Pages path compatibility. It
also reports pages not reached from the product, docs or engineering catalogs.
External sites and backend routes require separate checks.

Run `make test-site` for the link check and the shared UI regression tests. They
cover copy failures, focus, optional storage, sidebar scrolling, skip targets,
the FinOps JSON sample, view selection and animation fallback behavior. Keep
visual checks for every page and full content bounds alongside these tests;
unit fixtures do not verify browser layout.
