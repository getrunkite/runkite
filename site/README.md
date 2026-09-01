# Runkite landing (GitHub Pages)

Animated product landing served from this folder via `.github/workflows/pages.yml`.
Product visuals live in `assets/` (Admin walkthrough GIF + ecosystem diagram).

**Live:** https://getrunkite.github.io/runkite/

One-page sections (same nav pattern): `#scenario` · `#try` · `#stack` · `#governance` · `#architecture`.

**Support map:** [`support/`](./support/) — Try → Why → Scenario → Install → Checkpoints → Connectors/HITL → Ops → Security → FinOps → Protocol → Limitations, plus [chapters](./support/chapters/) (primers), [decisions](./support/decisions/), and [engineering notes](./design/) (fencing, subscribe-before-enqueue, createRunCtx, poison pill).

Narrative order: why a plane → 5-minute try → product depth.  
`#when-not` (inside Scenario) is the thin "when not to use" callout.

`try.html` redirects to `/#try` so older links still work.

Engineering notes hub: [`design/`](./design/) — skimmable crash/admission set:

- [Generation fencing](./design/fencing.html)
- [Subscribe-before-enqueue](./design/subscribe-before-enqueue.html)
- [createRunCtx / fail-closed](./design/create-run-ctx.html)
- [Poison pill / reclaim ceiling](./design/poison-pill.html)

Local preview: open `site/index.html` in a browser (or any static file server).
