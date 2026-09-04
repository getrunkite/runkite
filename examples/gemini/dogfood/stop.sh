#!/usr/bin/env bash
set -euo pipefail
DOGFOOD="$(cd "$(dirname "$0")" && pwd)"
RUN="$DOGFOOD/.run"
mkdir -p "$RUN"
if [[ -f "$RUN/pids" ]]; then
  while read -r pid; do
    [[ -n "$pid" ]] || continue
    kill "$pid" 2>/dev/null || true
  done <"$RUN/pids"
fi
for port in 2026 50051 3100 3101 3102 3103 3104 3105 3106 3107; do
  if command -v lsof >/dev/null 2>&1; then
    pids=$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)
    if [[ -n "${pids:-}" ]]; then
      kill $pids 2>/dev/null || true
    fi
  fi
done
if command -v pkill >/dev/null 2>&1; then
  pkill -f "examples/gemini/dogfood" 2>/dev/null || true
  pkill -f "runkite_runner .*dogfood" 2>/dev/null || true
  pkill -f "langchain_adapter .*dogfood" 2>/dev/null || true
  pkill -f "crewai_adapter .*dogfood" 2>/dev/null || true
  pkill -f "llamaindex_adapter .*dogfood" 2>/dev/null || true
  pkill -f "autogen_adapter .*dogfood" 2>/dev/null || true
fi
rm -f "$RUN/pids" "$RUN/names"
echo "stopped"
