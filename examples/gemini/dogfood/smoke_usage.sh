#!/usr/bin/env bash
# Live metering contract: every dogfood agent must emit Output.usage with
# tokens + model after one successful Gemini turn. Catches the silent FinOps
# gap where chat works but Spend stays empty/$0.
#
# Requires dogfood already running (./examples/gemini/dogfood/start.sh) and
# GOOGLE_API_KEY / GEMINI_MODEL available to those runners.
set -euo pipefail

CP="${CP:-http://127.0.0.1:2026}"
AGENTS=(
  gemini_langgraph
  gemini_langchain
  gemini_crewai
  gemini_llamaindex
  gemini_autogen
  gemini_langgraphjs
)

fail=0
for agent in "${AGENTS[@]}"; do
  thread=$(curl -sf -X POST "$CP/threads" -H 'Content-Type: application/json' -d '{}' \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["thread_id"])')
  out=$(curl -sf -X POST "$CP/threads/$thread/runs/wait" -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"$agent\",\"input\":{\"messages\":[{\"role\":\"user\",\"content\":\"Say OK\"}]}}")
  if ! python3 -c '
import json,sys
d=json.loads(sys.argv[1])
out=d.get("values") or d.get("output") or d
u=(out.get("usage") if isinstance(out, dict) else None) or {}
ok = (
  isinstance(u, dict)
  and int(u.get("prompt_tokens") or 0) + int(u.get("completion_tokens") or 0) > 0
  and bool(u.get("model"))
)
print(sys.argv[2], "OK" if ok else "FAIL", "usage=", u)
sys.exit(0 if ok else 1)
' "$out" "$agent"; then
    fail=1
  fi
done

if [[ "$fail" -ne 0 ]]; then
  echo "smoke_usage: one or more agents missing Output.usage (tokens+model)" >&2
  exit 1
fi
echo "smoke_usage: all ${#AGENTS[@]} agents metered"
