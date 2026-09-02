# v0.3 announcement draft (A1) — DO NOT PUBLISH YET

**Status:** draft only.  
**Publish gate (locked with plan):** public `v0.3.0` tag = **train-done** (Phase A close-out + all Phase B exits, or a written founder-cut note). Not “P2 green alone,” not an early tag slice.  
**Hook (locked):** shared control plane + 5-minute demo — **not** “we open-sourced a protocol.”  
**Links:** https://getrunkite.github.io/runkite/ · https://github.com/getrunkite/runkite · `#try` on the site

---

## One-liner (any channel)

Runkite is a self-hosted Agent Protocol control plane (hosted plane also in this release): one Go binary, pluggable runners (LangGraph, CrewAI, and friends), shared connectors/HITL/audit — try the Docker demo in ~5 minutes.

---

## Show HN

**Title:** Show HN: Runkite – agent control plane (self-host + hosted CP, 5-min Docker demo)

**Body:**

I got tired of three LangGraph/CrewAI scripts each carrying their own `GITHUB_TOKEN`, their own HITL, and their own logs — with agent-to-agent as ad-hoc HTTP.

Runkite is a self-hosted [Agent Protocol](https://github.com/langchain-ai/agent-protocol) control plane in one Go binary (hosted control plane available too). Runners plug in over a small gRPC Runner Protocol (LangGraph, CrewAI, LlamaIndex, AutoGen, LangChain, LangGraph.js). Connectors, grants, mandatory HITL, kill/pause, and audit live on the plane — not copied into every agent process.

What it’s for:

- Several agents / more than one framework, shared connector secrets
- Ops that isn’t SSH + three log streams
- Persistence that doesn’t pretend ThreadState is a LangGraph checkpointer (opaque proxy store for LangGraph + CrewAI / LlamaIndex / AutoGen / LangChain message continuity)
- A proper support surface + redesign — not only a README
- Hosted control plane when you don’t want to run the CP yourself (BYO runner in the first hosted slice)
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

Happy to take questions on the multi-agent scenario, governance, proxy checkpoints, or the hosted plane.

---

## Reddit (r/LocalLLaMA / r/LangChain / r/selfhosted — tune sub)

**Title:** Runkite – agent control plane (self-host or hosted CP, framework-agnostic, 5-min Docker demo)

When one LangGraph script isn’t enough — coordinator + worker + maybe a second framework — you usually end up with three `.env` copies of the same connector secret and three places to do HITL.

Runkite is an Agent Protocol control plane (Go binary). Bring LangGraph / CrewAI / LlamaIndex / AutoGen / LangChain / LangGraph.js runners; the plane owns jobs, streaming, connectors, grants, HITL, kill, and audit.

Self-host under BUSL (converts to Apache 4y after each release), or use the hosted control plane and bring your own runner.

5-minute path: site Try it → compose up → echo → approve.  
https://getrunkite.github.io/runkite/#try  
https://github.com/getrunkite/runkite

Feedback welcome, especially from people running multi-agent stacks today.

---

## X / short

Runkite: agent control plane — self-host or hosted CP.

Any framework runner. Shared connectors + HITL + audit on the plane — not three `.env` files.

Docker demo in ~5 min → echo → approve.  
https://getrunkite.github.io/runkite/#try

---

## What we deliberately don’t lead with

- “We published a Runner Protocol” (as the headline)
- Checkpoint/proxy internals as the headline (fine in comments)
- EKS theater as the headline (K8s proof is in the train; don’t lead the announce with cluster cosplay)

## Publish checklist (train-done)

- [ ] Phase A close-out: cold tester PASS + P2a §4.1 re-verify + R1 (Redis/NATS/Kafka) + `make smoke-governance`
- [ ] Phase B exits green (W1 W2 U2 T2 O2 S-RLS S-CONT D4/D7/D8 C1/C2 CLOUD FINOPS K8S) **or** founder-cut note attached
- [ ] `v0.3.0` tag cut
- [ ] Re-read links (`#try`, Releases, hosted signup if live, PyPI/npm if citing runners)
- [ ] One founder pass for tone; no internal plan IDs in the public text
