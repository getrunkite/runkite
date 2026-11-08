# Runkite landing (GitHub Pages)

Product site source. `.github/workflows/pages.yml` publishes this folder to
https://getrunkite.github.io/runkite/.
Visuals live in `assets/`.

**Live:** https://getrunkite.com/

**Brand system:** [`brand.css`](./brand.css) is the shared NOC canvas (Fraunces + IBM Plex Sans + JetBrains Mono, amber). The product landing ([`landing.css`](./landing.css)) uses a product-first hero, two execution paths and a self-hosted topology. Support and design notes retain the shared brand.

**Routes (uniform multi-page nav):**

- [`/`](./index.html), product landing
- [`/support/try.html`](./support/try.html), try path
- [`/support/`](./support/). Docs handbook (catalog + feature guides + chapters + decisions)
- [`/support/sdk-integration.html`](./support/sdk-integration.html), execution ownership and SDK integration paths
- [`/design/`](./design/), engineering notes hub (fencing, subscribe-before-enqueue, createRunCtx, poison pill)

`try.html` at the site root redirects to `/support/try.html`.
`www-getrunkite/index.html` is a compatibility redirect to the canonical product
landing. Keep those stubs as redirects, not alternate copies of the product page.

## Source and release labels

The Pages workflow uploads this directory as static HTML whenever `site/**`
changes reach `main`. Editing Markdown in `docs/` does not regenerate this
handbook. Keep the public HTML and its linked Markdown references aligned.

A separately hosted copy of `getrunkite.com` must also receive the updated site
files through its own deployment. A successful Pages run alone does not prove
that the canonical domain is current. Verify the rendered content on both URLs
after publication. Deploy only static site files; preserve the domain's Admin
and API routes.

The SDK integration guide separates the released decision API, unpublished
in-repository helpers, and the managed AI SDK backend (v0.5.8+). A checkout
feature must not be advertised as available in an existing npm/PyPI install.
When a feature is released, update these labels together with the changelog and
package install guidance. Keep public install commands unpinned unless an API
compatibility example specifically requires a tested SDK version.

The product is self-host only. The public demo Admin is a sandbox, not a hosted
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

The homepage uses the real `ui-runs.png` Admin capture, explicitly labeled as
demo data. `landing.js` optionally swaps it for the existing walkthrough GIF
when a visitor presses Play, and restores the still on Pause. Nothing animates
automatically. With JavaScript disabled, all copy, navigation and the static
product image remain available; the inactive Play control stays hidden.

The separate `execution-flow.css` and `execution-flow.js` restore the animated
managed-run diagram near the top of the page, also linked from the hero. It is
an illustration of protocol handoffs, not live telemetry. Model calls originate
in the runner; the optional MCP branch returns through the plane's connector
gate. App-owned decisions are a separate path. Motion starts only on desktop
when reduced motion is not requested, pauses outside the viewport or in a
hidden tab, and has a visible Pause control. Mobile, reduced-motion and
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

Keep runtime ownership explicit when editing the page: managed executions create
Threads and Runs; app-owned tool decisions do not. Connector policy requires the
connector gate, and the bridge and governance helpers remain repository code.
Current source uses Apache-2.0 and may be described as open source. Keep the
product self-host only. The homepage links to the published v0.6.1 release,
the first Apache-2.0 release. Published v0.6.0 retains its shipped BUSL-1.1 terms;
changing the source license does not update old registry artifacts or tags.
The footer preserves that distinction. Published framework rows link to their
integration contracts.

For a presentation change, check the homepage at 320, 390, 768 and 1440 pixel
widths, keyboard focus and Play/Pause behavior. Verify local links, SVG/JPEG
dimensions and all images before allowing the existing Pages workflow to publish.

Run `make check-doc-links` after editing the public HTML. The checker validates
local links, section anchors, CSS assets and GitHub Pages path compatibility. It
also reports pages not reached from the product, docs or engineering catalogs.
External sites and backend routes require separate checks.
