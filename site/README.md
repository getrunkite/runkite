# Runkite landing (GitHub Pages)

Product site served from this folder via `.github/workflows/pages.yml`.
Visuals live in `assets/`.

**Live:** https://getrunkite.com/

**Brand system:** [`brand.css`](./brand.css) is the shared NOC canvas (Fraunces + IBM Plex Sans + JetBrains Mono, amber). The product landing ([`landing.css`](./landing.css)) adds an amber bloom and a moving hop diagram on the fold. Support and design notes stay hard-edged.

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
