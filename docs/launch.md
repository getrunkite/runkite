# Launch / announcement (governance preview)

Maintainer checklist and channel copy for announcing the governance wedge.
Product proof already on `main`: Supported HA (`make soak-multi` /
[`bench/soak/WRITEUP.md`](../bench/soak/WRITEUP.md)), plane governance
(`make smoke-governance` / [Trust & governance](trust-governance.md)).
Kubernetes remains **Compatible** (kind smokes only; paid EKS deferred).

**Primary CTA for launch posts:** try `runkite dev` (or star if the channel
hates install instructions — pick one per post, not both as equals).

Do **not** cut a new PyPI/npm/GitHub release until you explicitly say so.
Do **not** claim LangSmith API parity, Mongo governance equality, in-graph
tool sandboxing, FinOps dashboards, or EKS soak.

---

## Pre-flight (before any post)

- [ ] Site + repo + releases + PyPI + npm open in a private window (no 404)
- [ ] `runkite dev` (or Docker) + runner → one successful run + Admin at `/admin/`
- [ ] `make smoke-governance` green (deny + audit + HITL one-shot)
- [ ] Limitations linked: [limitations.md](limitations.md)
- [ ] Decide date/time (default proposal: Tuesday ~09:00 America/New_York, Show HN first)

---

## Show HN

**Title (preferred):**

```text
Show HN: Runkite – self-hosted agent control plane in one Go binary (any framework)
```

**Body:**

```text
I built Runkite: a self-hosted Agent Protocol control plane in Go.

Same *job* people buy LangSmith Deployments for (durable threads/runs,
streaming, HITL, cron, Admin) — but not a LangSmith API clone and not
locked to LangGraph. Runners plug in over a small gRPC protocol:
LangGraph, CrewAI, LlamaIndex, AutoGen, LangChain, LangGraph.js.

Beyond the ops plane: fail-closed connector grants, durable policy audit,
connector HITL, kill/pause, and break-glass on SQL backends — with Admin
pages for each (Postgres Supported). Prove locally with
`make smoke-governance`. Multi-CP HA is soaked on Compose
(Postgres+Redis); Helm/kind are packaging smokes — Kubernetes stays
Compatible until a real-cloud soak.

One static binary + embedded Admin UI. Preview (BUSL).

Not “open the LangGraph runtime” — if you only need stock LangGraph
Agent Server + PG/Redis, that’s a different project. Runkite is for a
generic plane you operate yourself.

Site: https://getrunkite.github.io/runkite/
Repo: https://github.com/getrunkite/runkite
Try: binary or `docker pull ghcr.io/getrunkite/runkite` +
`pip install runkite-runner` → `runkite dev` → open /admin/

Limitations are documented. Stars/issues welcome.
```

**Reply one-liner** (langhost / “is it LangSmith?”):

```text
Those keep you on the LangGraph Agent Server path. Runkite is a separate
control plane if you need multiple frameworks / backends under one protocol,
plus connector policy + audit on the plane you operate.
```

---

## LinkedIn

```text
Shipped a public preview of Runkite — self-hosted Agent Protocol control plane.

Go control plane + framework-agnostic runners (LangGraph, CrewAI, LlamaIndex,
AutoGen, LangChain, LangGraph.js). Embedded Admin. Postgres+Redis HA.

Also on the plane: fail-closed connector grants, durable audit, connector HITL
(SQL backends). Own the runs — and who may call which connector.

https://getrunkite.github.io/runkite/
```

---

## X / Twitter (skeleton)

1. Hook + Admin GIF: self-hosted Agent Protocol CP — Go binary, any framework runner.
2. Ops: threads/runs/HITL/cron/Admin — not a LangSmith API clone.
3. Governance: connector grants + audit + HITL on the plane (`smoke-governance`).
4. Link site + repo.
5. CTA: `runkite dev` or star.

---

## Reddit titles

```text
r/golang: Runkite — Agent Protocol control plane in one Go binary (HA + governance)
r/LangChain: Framework-agnostic Agent Protocol CP (not LangGraph-only) + connector policy
r/selfhosted: Self-hosted agent control plane (binary/Docker, Admin, Postgres+Redis)
r/LocalLLaMA: Self-hosted agent control plane (Go) — framework-agnostic, Admin included
```

Adapt the Show HN body; keep non-claims.

---

## Channels (ordered)

| Priority | Channel |
|----------|---------|
| P0 | Show HN first (~Tue 9am ET) |
| P0 | LangChain / LlamaIndex / CrewAI Discords — helpful, one link |
| P0 | LinkedIn |
| P1 | Reddit (`r/golang`, `r/LangChain`, `r/selfhosted`, `r/LocalLLaMA`) |
| P2 | X thread + GIF |
| P2 | Short blog (dev.to / LinkedIn) Day 0 evening or Day 3 |

---

## Non-claims (paste if challenged)

- Not LangSmith API-compatible; not langhost / “open LangGraph Agent Server.”
- Not equal governance on Mongo (SQL audit / grants / pending HITL).
- Not in-graph `AuthorizeTool` / sandboxing native framework tools.
- Not FinOps dashboards or paid EKS soak; Helm on kind ≠ Supported K8s.
- BUSL preview — self-host freely; no offering as hosted SaaS without a license.
