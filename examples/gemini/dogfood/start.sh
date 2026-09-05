#!/usr/bin/env bash
# Start Runkite control plane + Gemini multi-framework runners + dogfood UIs.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
DOGFOOD="$(cd "$(dirname "$0")" && pwd)"
RUN="$DOGFOOD/.run"
LOG="$DOGFOOD/logs"
mkdir -p "$RUN" "$LOG"

cd "$ROOT"

# Secrets: RUNKITE_LLM_ENV (any path) or repo-root .env.llm (gitignored).
LLM_ENV_FILE="${RUNKITE_LLM_ENV:-}"
if [[ -n "$LLM_ENV_FILE" ]]; then
  LLM_ENV_FILE="${LLM_ENV_FILE/#\~/$HOME}"
elif [[ -f .env.llm ]]; then
  LLM_ENV_FILE="$ROOT/.env.llm"
fi
if [[ -z "${LLM_ENV_FILE:-}" || ! -f "$LLM_ENV_FILE" ]]; then
  echo "Missing LLM env file. Copy .env.llm.example → .env.llm, or set RUNKITE_LLM_ENV=/path/to/llm.env" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1090
source "$LLM_ENV_FILE"
set +a
if [[ -z "${GOOGLE_API_KEY:-}" ]]; then
  echo "GOOGLE_API_KEY empty in $LLM_ENV_FILE" >&2
  exit 1
fi
echo "LLM env: $LLM_ENV_FILE"

if [[ -f "$RUN/pids" ]]; then
  echo "Already running? Found $RUN/pids — run ./stop.sh first" >&2
  exit 1
fi

if [[ ! -x ./runkite ]] || [[ cmd -nt ./runkite ]] || [[ internal/adminui/dist/index.html -nt ./runkite ]]; then
  echo "building ./runkite (binary missing or older than cmd/adminui) ..."
  go build -o ./runkite ./cmd
fi

# LLM extras for LangGraph / LangChain in python/.venv
if ! python/.venv/bin/python -c "import langchain_google_genai" >/dev/null 2>&1; then
  echo "installing python/requirements-llm.txt into python/.venv ..."
  python/.venv/bin/pip install -q -r python/requirements-llm.txt
fi

# Predictable local dogfood posture: don't inherit a laptop POSTGRES_DSN /
# REDIS_URL that would mix backends. `runkite dev` + SQLite is enough for
# Spend/HITL dogfooding; runners use HTTP proxy checkpoints via RUNKITE_HTTP_URL.
unset POSTGRES_DSN MYSQL_DSN MONGO_URI REDIS_URL NATS_URL KAFKA_URL || true
# Isolated SQLite so leftover agents from other local experiments don't clutter Admin.
export DATABASE_PATH="$DOGFOOD/.run/dogfood.db"

export PYTHONPATH="${ROOT}/python:${ROOT}/python/adapters${PYTHONPATH:+:$PYTHONPATH}"
CP_HTTP=2026
CP_GRPC=50051
export RUNKITE_HTTP_URL="http://127.0.0.1:${CP_HTTP}"

start_bg() {
  local name="$1"
  shift
  # Do not echo full argv — may contain secrets via env.
  echo "→ $name"
  # Double-fork daemon (macOS-safe) so children survive this shell exiting.
  python3 "$DOGFOOD/daemonize.py" "$LOG/$name.log" "$@"
  # Best-effort pid capture from the eventual process listening / matching is hard
  # after double-fork; record name and rely on stop.sh port cleanup.
  echo "$name" >>"$RUN/names"
}

# Control plane (loads every *.json in dogfood/ — first file wins for finops/cors)
start_bg cp "$ROOT/runkite" dev --config "$DOGFOOD" --port "$CP_HTTP" --grpc-port "$CP_GRPC"

echo "waiting for control plane..."
for i in $(seq 1 40); do
  if curl -sf "http://127.0.0.1:${CP_HTTP}/health" >/dev/null; then
    break
  fi
  sleep 0.25
  if [[ "$i" -eq 40 ]]; then
    echo "control plane failed to become healthy — see $LOG/cp.log" >&2
    "$DOGFOOD/stop.sh" || true
    exit 1
  fi
done

# Runners (one process per runner_kind / config file)
start_bg runner-langgraph \
  env PYTHONPATH="$ROOT/python" \
  python/.venv/bin/python -m runkite_runner \
  --config "$DOGFOOD/00_controlplane.json" \
  --grpc-address "127.0.0.1:${CP_GRPC}"

start_bg runner-langchain \
  env PYTHONPATH="$ROOT/python:$ROOT/python/adapters" \
  python/.venv/bin/python -m langchain_adapter \
  --config "$DOGFOOD/10_langchain.json" \
  --grpc-address "127.0.0.1:${CP_GRPC}"

if [[ -x python/adapters/crewai_adapter/.venv/bin/python ]]; then
  start_bg runner-crewai \
    env PYTHONPATH="$ROOT/python:$ROOT/python/adapters" \
    python/adapters/crewai_adapter/.venv/bin/python -m crewai_adapter \
    --config "$DOGFOOD/20_crewai.json" \
    --grpc-address "127.0.0.1:${CP_GRPC}"
fi

if [[ -x python/adapters/llamaindex_adapter/.venv/bin/python ]]; then
  start_bg runner-llamaindex \
    env PYTHONPATH="$ROOT/python:$ROOT/python/adapters" \
    python/adapters/llamaindex_adapter/.venv/bin/python -m llamaindex_adapter \
    --config "$DOGFOOD/30_llamaindex.json" \
    --grpc-address "127.0.0.1:${CP_GRPC}"
fi

if [[ -x python/adapters/autogen_adapter/.venv/bin/python ]]; then
  start_bg runner-autogen \
    env PYTHONPATH="$ROOT/python:$ROOT/python/adapters" \
    python/adapters/autogen_adapter/.venv/bin/python -m autogen_adapter \
    --config "$DOGFOOD/40_autogen.json" \
    --grpc-address "127.0.0.1:${CP_GRPC}"
fi

if [[ -d typescript/runkite-runner/node_modules ]]; then
  EXAMPLE_NM="$ROOT/examples/gemini/langgraphjs_agent/node_modules"
  RUNNER_NM="$ROOT/typescript/runkite-runner/node_modules"
  start_bg runner-langgraphjs \
    env \
    GOOGLE_API_KEY="$GOOGLE_API_KEY" \
    GEMINI_MODEL="${GEMINI_MODEL:-gemini-2.0-flash}" \
    GEMINI_TEMPERATURE="${GEMINI_TEMPERATURE:-0}" \
    NODE_PATH="${EXAMPLE_NM}:${RUNNER_NM}${NODE_PATH:+:$NODE_PATH}" \
    bash -lc "cd '$ROOT/typescript/runkite-runner' && npx tsx src/cli.ts --config '$DOGFOOD/50_langgraphjs.json' --grpc-address '127.0.0.1:${CP_GRPC}' --http-address 'http://127.0.0.1:${CP_HTTP}'"
fi

# UIs
start_bg ui-hub python3 "$DOGFOOD/ui/serve.py" --port 3100 --hub --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-langgraph python3 "$DOGFOOD/ui/serve.py" --port 3101 --agent gemini_langgraph --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-langchain python3 "$DOGFOOD/ui/serve.py" --port 3102 --agent gemini_langchain --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-crewai python3 "$DOGFOOD/ui/serve.py" --port 3103 --agent gemini_crewai --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-llamaindex python3 "$DOGFOOD/ui/serve.py" --port 3104 --agent gemini_llamaindex --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-hitl python3 "$DOGFOOD/ui/serve.py" --port 3105 --agent approval_agent --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-autogen python3 "$DOGFOOD/ui/serve.py" --port 3106 --agent gemini_autogen --cp "http://127.0.0.1:${CP_HTTP}"
start_bg ui-langgraphjs python3 "$DOGFOOD/ui/serve.py" --port 3107 --agent gemini_langgraphjs --cp "http://127.0.0.1:${CP_HTTP}"

sleep 1
echo
echo "════════════════════════════════════════════════════════════"
echo " Runkite Gemini dogfood is up"
echo "════════════════════════════════════════════════════════════"
echo
echo " Control plane API     http://127.0.0.1:${CP_HTTP}"
echo " Admin UI              http://127.0.0.1:${CP_HTTP}/admin/"
echo " Admin Spend (FinOps)  http://127.0.0.1:${CP_HTTP}/admin/spend"
echo " Agents JSON           http://127.0.0.1:${CP_HTTP}/agents"
echo
echo " QA hub (checklist)     http://127.0.0.1:3100/"
echo " Chat playground        http://127.0.0.1:3100/index.html"
echo " LangGraph + Gemini    http://127.0.0.1:3101/"
echo " LangChain + Gemini    http://127.0.0.1:3102/"
echo " CrewAI + Gemini       http://127.0.0.1:3103/"
echo " LlamaIndex + Gemini   http://127.0.0.1:3104/"
echo " HITL / checkpoint     http://127.0.0.1:3105/"
echo " AutoGen + Gemini      http://127.0.0.1:3106/"
echo " LangGraph.js + Gemini http://127.0.0.1:3107/"
echo
echo " Logs: $LOG   Stop: $DOGFOOD/stop.sh"
echo "════════════════════════════════════════════════════════════"

# Smoke: list agents (Agent Protocol search)
curl -sf -X POST "http://127.0.0.1:${CP_HTTP}/agents/search" -H 'Content-Type: application/json' -d '{}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); items=d if isinstance(d,list) else d.get("agents") or d.get("items") or []; print("agents:", ", ".join(sorted({(a.get("agent_id") or a.get("assistant_id") or "?") for a in items})) or "(none)")' || true
