# v0.3 announcement draft — DO NOT PUBLISH YET

**Status:** draft only. Publish after the `v0.3.0` tag is cut.  
**Hook:** shared control plane + 5-minute demo — **not** “we open-sourced a protocol.”  
**Links:** https://getrunkite.github.io/runkite/ · https://github.com/getrunkite/runkite · `#try` on the site

This cut is **self-host only**. A hosted control plane is not part of v0.3.

---

## One-liner (any channel)

Runkite is a self-hosted Agent Protocol control plane: one Go binary, pluggable runners (LangGraph, CrewAI, and friends), shared connectors/HITL/audit — try the Docker demo in ~5 minutes.

---

## Show HN

**Title:** Show HN: Runkite – self-hosted agent control plane (5-min Docker demo)

**Body:**

I got tired of three LangGraph/CrewAI scripts each carrying their own `GITHUB_TOKEN`, their own HITL, and their own logs — with agent-to-agent as ad-hoc HTTP.

Runkite is a self-hosted [Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane in one Go binary. Runners plug in over a small gRPC Runner Protocol (LangGraph, CrewAI, LlamaIndex, AutoGen, LangChain, LangGraph.js). Connectors, grants, mandatory HITL, kill/pause, and audit live on the plane — not copied into every agent process.

What it’s for:

- Several agents / more than one framework, shared connector secrets
- Ops that isn’t SSH + three log streams
- Persistence that doesn’t pretend ThreadState is a LangGraph checkpointer (opaque proxy store for LangGraph + CrewAI / LlamaIndex / AutoGen / LangChain message continuity)
- A proper support surface + redesign — not only a README
- Spend control on the plane: per-tenant/agent budgets, cost dashboards, alerts, chargeback export, optional price-aware routing

What it isn’t: a LangSmith clone, or “just another agent framework.”

Try it (Docker only, laptop demo):

https://getrunkite.github.io/runkite/#try

```
git clone https://github.com/getrunkite/runkite && cd runkite
docker compose -f docker-compose.dev.yml up -d --build
# echo agent → HITL approve in Admin
```

Repo: https://github.com/getrunkite/runkite (BUSL → Apache after 4 years per release)

Happy to take questions on the multi-agent scenario, governance, or proxy checkpoints.

---

## Reddit (r/LocalLLaMA / r/LangChain / r/selfhosted — tune sub)

**Title:** Runkite – self-hosted agent control plane (framework-agnostic, 5-min Docker demo)

When one LangGraph script isn’t enough — coordinator + worker + maybe a second framework — you usually end up with three `.env` copies of the same connector secret and three places to do HITL.

Runkite is an Agent Protocol control plane (Go binary). Bring LangGraph / CrewAI / LlamaIndex / AutoGen / LangChain / LangGraph.js runners; the plane owns jobs, streaming, connectors, grants, HITL, kill, and audit.

Self-host under BUSL (converts to Apache 4y after each release).

5-minute path: site Try it → compose up → echo → approve.  
https://getrunkite.github.io/runkite/#try  
https://github.com/getrunkite/runkite

Feedback welcome, especially from people running multi-agent stacks today.

---

## X / short

Runkite: self-hosted agent control plane.

Any framework runner. Shared connectors + HITL + audit on the plane — not three `.env` files.

Docker demo in ~5 min → echo → approve.  
https://getrunkite.github.io/runkite/#try

---

## What we deliberately don’t lead with

- “We published a Runner Protocol” (as the headline)
- Checkpoint/proxy internals as the headline (fine in comments)
- EKS theater as the headline
- A hosted control plane (not in this release)

## Publish checklist

- [ ] `v0.3.0` tag cut
- [ ] Re-read links (`#try`, Releases, PyPI/npm if citing runners)
- [ ] One founder pass for tone; no internal plan IDs in the public text
