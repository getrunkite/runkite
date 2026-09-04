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

# Cross-call regression check: a single successful chat proves tokens show
# up at all, but says nothing about whether they show up *correctly*. Two
# real bugs shipped past that single-call check before this existed:
#   - LangGraph/LangGraph.js: a stateful graph's checkpoint carries every
#     prior turn's AIMessage.usage_metadata forward, so accumulating usage
#     from the full "values" snapshot re-summed old turns on every new run
#     on the same thread.
#   - CrewAI: crew.usage_metrics is the shared Crew instance's LIFETIME
#     total across every run ever dispatched to that graph_id, not this
#     call's own usage -- unrelated fresh-thread calls to the same agent
#     reported growing, unbounded prompt_tokens.
# The one signal both bugs share: sending the IDENTICAL prompt to the same
# agent on a brand-new (unrelated) thread twice must report the SAME
# prompt_tokens both times. Growth here on a fresh thread is never
# legitimate context growth (there is no shared conversation) -- it can
# only mean an earlier call's usage leaked into this one.
echo
echo "cross-call usage isolation check (identical prompt, unrelated fresh threads):"
fail=0
for agent in "${AGENTS[@]}"; do
  first=""
  for i in 1 2; do
    thread=$(curl -sf -X POST "$CP/threads" -H 'Content-Type: application/json' -d '{}' \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["thread_id"])')
    out=$(curl -sf -X POST "$CP/threads/$thread/runs/wait" -H 'Content-Type: application/json' \
      -d "{\"agent_id\":\"$agent\",\"input\":{\"messages\":[{\"role\":\"user\",\"content\":\"Say OK and nothing else.\"}]}}")
    prompt_tokens=$(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
out = d.get("values") or d.get("output") or d
u = (out.get("usage") if isinstance(out, dict) else None) or {}
print(int(u.get("prompt_tokens") or 0))
' "$out")
    if [[ "$i" -eq 1 ]]; then
      first="$prompt_tokens"
    elif [[ "$prompt_tokens" != "$first" ]]; then
      echo "$agent FAIL: prompt_tokens grew across unrelated fresh threads ($first -> $prompt_tokens) -- usage is leaking across calls"
      fail=1
    else
      echo "$agent OK: prompt_tokens stable at $first across two unrelated fresh-thread calls"
    fi
  done
done

if [[ "$fail" -ne 0 ]]; then
  echo "smoke_usage: cross-call usage isolation regression detected" >&2
  exit 1
fi
echo "smoke_usage: cross-call usage isolation holds for all ${#AGENTS[@]} agents"
