# Gemini example agents (real LLM)

Reference agents that call **Google Gemini** instead of fake/offline LLMs.
Use these when you want to see a real model on every runner Runkite claims:

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

The Python examples load `.env.llm` via `_env.py`. For the TS example, export the vars (or `set -a && source ../../.env.llm && set +a`) before starting the runner.

## Quick run (LangGraph Python)

```bash
set -a && source .env.llm && set +a
# start control plane + runner pointed at examples/gemini/langgraph_agent/langgraph.json
```

For the full frameworks × backends matrix with budget tracking, see `bench/llm/`.
