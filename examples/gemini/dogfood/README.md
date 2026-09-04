# Local multi-framework playground

Runs your Runkite control plane locally with the Gemini example agents
(LangGraph, LangChain, CrewAI, LlamaIndex, AutoGen, LangGraph.js) plus HITL
helpers — one process tree, shared FinOps Spend.

## URLs

| URL | Purpose |
|-----|---------|
| http://127.0.0.1:2026 | Control plane API |
| http://127.0.0.1:2026/admin/ | Admin UI |
| http://127.0.0.1:2026/admin/spend | Spend / usage |
| http://127.0.0.1:3100/ | Hub UI (all agents) |
| http://127.0.0.1:3101/ … :3107/ | Per-framework chat UIs |

## Credentials

Copy `.env.llm.example` → `.env.llm` at the repo root and set `GOOGLE_API_KEY`.
Never commit `.env.llm`. Or set `RUNKITE_LLM_ENV=/path/to/your.env`.

Model ids that appear in Spend should also have rows in
`00_controlplane.json` → `finops.pricebook`.

## Start / stop

```bash
./examples/gemini/dogfood/start.sh
./examples/gemini/dogfood/stop.sh
```

Clean local DB / logs and restart:

```bash
./examples/gemini/dogfood/stop.sh
rm -rf examples/gemini/dogfood/.run examples/gemini/dogfood/logs
./examples/gemini/dogfood/start.sh
```

## Usage smoke

```bash
./examples/gemini/dogfood/smoke_usage.sh
```

Fails if any agent is missing `Output.usage` (tokens + model), or if two
identical fresh-thread calls report growing `prompt_tokens` (lifetime /
cross-turn double-count regressions).

Extra agents on this control plane (for FinOps edge paths):

- `llm_approval_agent` — real LLM turn then HITL interrupt
- `unknown_provider_agent` — real LLM reply with usage stripped (`usage_unmetered`)
- `bare_concat_agent` — id-less messages (`operator.add` + cleared reply ids); exercises `skip_prefix`

## Prerequisites

1. `.env.llm` (or `RUNKITE_LLM_ENV`) with `GOOGLE_API_KEY`
2. `python/.venv` and adapter venvs under `python/adapters/*` where needed
3. Optional: `examples/gemini/langgraphjs_agent/node_modules` for LangGraph.js
