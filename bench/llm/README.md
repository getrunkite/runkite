# Live Gemini LLM matrix

Runs real Gemini calls across every claimed runner framework × backend
combination (plus LangGraph cancel / HITL using the existing non-LLM
example agents in the same process).

## Secrets

1. `cp .env.llm.example .env.llm`
2. Put `GOOGLE_API_KEY` in `.env.llm` (gitignored — never commit)
3. Soft budget: `LLM_BUDGET_USD` (default 150)

Artifacts under `bench/llm/out/` are gitignored (may contain prompts/responses).

## Run

```bash
set -a && source .env.llm && set +a

# Plan only
python3 bench/llm/run_matrix.py --dry-run

# Fast path: sqlite + all frameworks that have local venvs
python3 bench/llm/run_matrix.py --backends sqlite_inprocess

# Full N×N (needs `make infra-up`)
python3 bench/llm/run_matrix.py
```

## Example agents

See [`examples/gemini/`](../../examples/gemini/) for copy-paste Gemini agents per framework.
