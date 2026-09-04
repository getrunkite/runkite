# Gemini example agents

Reference agents that call a real Google Gemini model (instead of offline stubs)
so you can exercise every Runkite runner kind end-to-end.

| Folder | Runner kind | Agent id |
|--------|-------------|----------|
| `langgraph_agent/` | `python-langgraph` | `gemini_langgraph` |
| `langchain_agent/` | `python-langchain` | `gemini_langchain` |
| `crewai_agent/` | `python-crewai` | `gemini_crewai` |
| `llamaindex_agent/` | `python-llamaindex` | `gemini_llamaindex` |
| `autogen_agent/` | `python-autogen` | `gemini_autogen` |
| `langgraphjs_agent/` | `typescript-langgraphjs` | `gemini_langgraphjs` |

## Credentials

1. Copy repo-root `.env.llm.example` → `.env.llm` (gitignored).
2. Set `GOOGLE_API_KEY` (and optionally `GEMINI_MODEL`).
3. Never commit `.env.llm`.

Optionally point at a secrets file outside the repo with `RUNKITE_LLM_ENV=/path/to/env`.

Python examples load credentials via `_env.py`. For the TypeScript example, export
the same variables before starting the runner.

## Quick run (LangGraph Python)

```bash
set -a && source .env.llm && set +a
# start control plane + runner pointed at examples/gemini/langgraph_agent/langgraph.json
```

## Local playground (all frameworks + Admin)

```bash
./examples/gemini/dogfood/start.sh
# hub: http://127.0.0.1:3100/  — see dogfood/README.md
./examples/gemini/dogfood/smoke_usage.sh
```

For a broader frameworks × backends matrix, see `bench/llm/`.
