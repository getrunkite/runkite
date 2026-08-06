---
name: runkite-new-agent
description: >-
  Scaffold a new LangGraph (or adapter) agent that runs on the Runkite
  control plane — langgraph.json, graph module, local serve + runner
  commands. Use when the user wants a new Runkite agent, example, or
  "connect my graph to Runkite".
---

# New agent on Runkite

Create a minimal agent that the Runkite control plane can bootstrap and
dispatch via the Python (or TypeScript) runner.

## Defaults

- Language: **Python + LangGraph** unless the user asks for CrewAI,
  LlamaIndex, AutoGen, plain LangChain, or LangGraph.js.
- Start from `examples/echo_agent/` patterns in this repo.
- Do **not** invent proprietary product names or embed third-party
  internal apps in committed examples.

## Steps

1. **Pick an `agent_id`** — snake_case, unique in the deployment
   (becomes the graph key in `langgraph.json` and Admin UI).

2. **Create a folder** under `examples/<agent_id>/` (or the user’s app
   root):

   ```
   examples/<agent_id>/
     langgraph.json
     graph.py          # or graph.ts for LangGraph.js
   ```

3. **`langgraph.json`** (minimal):

   ```json
   {
     "graphs": {
       "<agent_id>": "./graph.py:graph"
     },
     "dependencies": ["."]
   }
   ```

   Optional later: `auth`, `cors.allow_origins`, `run_timeout`,
   connectors — see `docs/configuration.md`.

4. **`graph.py`** — compile a LangGraph `StateGraph` and export
   `graph`. Prefer a tiny deterministic node first (no LLM) so
   smoke-testing does not need API keys. Mirror
   `examples/echo_agent/graph.py`.

5. **Wire dual-mode** (production): runners with `POSTGRES_DSN` get
   durable LangGraph checkpoints (`AsyncPostgresSaver`). Without DSN,
   `MemorySaver` is ephemeral — say so if the user only runs local
   zero-deps.

6. **Run locally (Supported-shaped):**

   ```bash
   # terminal A — control plane (from repo root after make build)
   export RUNKITE_ALLOW_INSECURE_SERVE=1   # only for local demo without auth
   ./runkite dev --config examples/<agent_id>/langgraph.json

   # terminal B — runner
   export PYTHONPATH=python
   python/python/.venv/bin/python -m runkite_runner \
     --config examples/<agent_id>/langgraph.json \
     --grpc-address 127.0.0.1:50051 \
     --http-address http://127.0.0.1:2026
   ```

   Prefer the project’s documented venv paths if they differ. For
   production `serve`, set `POSTGRES_DSN`, `REDIS_URL`,
   `RUNNER_TOKEN_PYTHON_LANGGRAPH`, and client `auth` — see
   `docs/deployment.md`.

7. **Smoke**

   ```bash
   curl -sS -X POST http://127.0.0.1:2026/threads \
     -H 'Content-Type: application/json' -d '{}'
   # create a run on that thread with assistant_id / agent = <agent_id>
   ```

   Or open `http://127.0.0.1:2026/admin/` and confirm the agent is listed.

8. **Other frameworks** — point at existing examples instead of
   inventing new adapter code:
   - CrewAI → `examples/crewai_agent/`
   - LlamaIndex → `examples/llamaindex_agent/`
   - AutoGen → `examples/autogen_agent/`
   - LangChain → `examples/langchain_agent/`
   - TypeScript → `examples/echo_agent_ts/`
   - Real LLM → `examples/gemini/`

## Checklist before finishing

- [ ] `graph` (or adapter entry) exports correctly
- [ ] `langgraph.json` graph id matches what clients will send
- [ ] Local `dev` + runner smoke works OR user has Supported env vars
- [ ] No secrets committed; `.env` stays gitignored
- [ ] Docs pointer: `docs/quickstart.md`, `docs/runners.md`

## Out of scope for this skill

- Full multi-tenant JWT design
- Publishing to the registry marketplace
- Helm production hardening
- Rewriting the Admin UI
