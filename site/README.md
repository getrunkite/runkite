# Runkite landing (GitHub Pages)

Product site served from this folder via `.github/workflows/pages.yml`.
Visuals live in `assets/`.

**Live:** https://getrunkite.github.io/runkite/

**Brand system (W2):** [`brand.css`](./brand.css) — dark cinematic canvas, Fraunces + IBM Plex Sans + JetBrains Mono, cobalt accent (not mint-on-black). Shared by landing, support, and design notes.

**Routes (uniform multi-page nav):**
- [`/`](./index.html) — product landing
- [`/support/try.html`](./support/try.html) — try path
- [`/support/`](./support/) — Docs catalog + chapters + decisions
- [`/design/`](./design/) — engineering notes hub (fencing, subscribe-before-enqueue, createRunCtx, poison pill)

`try.html` at the site root redirects to `/support/try.html`.

Local preview:

```bash
cd site && python3 -m http.server 8765
```

Then open http://127.0.0.1:8765/
