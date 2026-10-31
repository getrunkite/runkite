# Runkite landing (GitHub Pages)

Product site served from this folder via `.github/workflows/pages.yml`.
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

`assets/runkite-social.svg` is the editable social-preview source. Its
1200 by 630 JPEG render is used by Open Graph and Twitter card metadata. Keep the
JPEG in sync after editing the SVG. `assets/runkite-mark.svg` is the local favicon.
The root README uses `docs/assets/runkite-overview.svg` for the two integration
paths. These diagrams explain product boundaries; they are not live activity or
screenshots. Existing screenshots and engineering evidence remain unchanged.

Keep runtime ownership explicit when editing the page: managed executions create
Threads and Runs; app-owned tool decisions do not. Connector policy requires the
connector gate, and the bridge and governance helpers remain repository code.
The license is BUSL-1.1; use self-hosted/source-available language rather than an
Open Source claim. Published framework rows link to their integration contracts.

For a presentation change, check the homepage at 320, 390, 768 and 1440 pixel
widths, keyboard focus and Play/Pause behavior. Verify local links, SVG/JPEG
dimensions and all images before allowing the existing Pages workflow to publish.
