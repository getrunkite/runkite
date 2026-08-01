#!/usr/bin/env bash
# Multi-control-plane smoke against docker-compose.multi.yml.
#
# Proves 3 CP replicas + nginx + 2 runners share Postgres/Redis end-to-end:
# health, agent registration, a real echo run to success, and Admin API
# visibility. Leaves the stack UP so you can open the Admin UI afterward
# (http://localhost:2026/admin/). Tear down with: make multi-down
#
# Wall clock: image build dominant (~5–15 min first time); the smoke itself
# is a few minutes. This is not a multi-hour soak.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
COMPOSE=(docker compose -f docker-compose.multi.yml)
BASE="${SMOKE_MULTI_BASE:-http://127.0.0.1:2026}"

if [[ -z "${RUNNER_TOKEN:-}" ]]; then
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a && source .env && set +a
  fi
fi
if [[ -z "${RUNNER_TOKEN:-}" ]]; then
  echo "error: RUNNER_TOKEN unset. Copy .env.example → .env and set it (openssl rand -hex 32)." >&2
  exit 1
fi

# Host :2026 must reach the compose nginx LB, not a leftover local
# `runkite serve`. On macOS a host process bound to IPv4 *:2026 wins over
# OrbStack's IPv6 publish of the same port -- smoke then creates runs in
# the wrong process while runners poll the real multi stack, and every
# run stays pending forever.
if command -v lsof >/dev/null 2>&1; then
  # lsof exits 1 when nothing is listening -- ignore that so an empty
  # port (pre-compose-up) does not abort the script under pipefail.
  host_pids="$(lsof -nP -iTCP:2026 -sTCP:LISTEN 2>/dev/null | awk 'NR>1 && $1 !~ /OrbStack|com\.docke/ {print $1"("$2")"}' | sort -u | tr '\n' ' ' || true)"
  if [[ -n "${host_pids// }" ]]; then
    echo "error: host process(es) listening on :2026 ahead of the multi-CP LB: ${host_pids}" >&2
    echo "       stop them (e.g. kill the local runkite) then re-run make smoke-multi." >&2
    exit 1
  fi
fi

echo "==> bringing up multi-CP stack (build if needed)"
"${COMPOSE[@]}" up -d --build

echo "==> waiting for load balancer /readyz"
deadline=$((SECONDS + 180))
until curl -sf "$BASE/readyz" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "error: /readyz not ready within 180s" >&2
    "${COMPOSE[@]}" ps >&2 || true
    exit 1
  fi
  sleep 2
done
echo "    readyz ok"

# Cheap proof we're talking to the compose stack (3 replicas share one
# Postgres that already has the all_agents bootstrap), not a stray local
# serve with a different DB.
overview_pre="$(curl -sf "$BASE/admin-api/overview" || true)"
if ! echo "$overview_pre" | grep -q '"cron_schedule_count":1'; then
  echo "error: $BASE/admin-api/overview does not look like docker-compose.multi.yml (expected cron_schedule_count=1)." >&2
  echo "       response: $overview_pre" >&2
  exit 1
fi

echo "==> waiting for echo_agent registration (runners via gRPC LB)"
deadline=$((SECONDS + 180))
registered=0
while (( SECONDS < deadline )); do
  # /agents/search is POST in Agent Protocol
  body="$(curl -sf -X POST "$BASE/agents/search" \
    -H 'Content-Type: application/json' \
    -d '{"limit":100}' 2>/dev/null || true)"
  if echo "$body" | grep -q 'echo_agent'; then
    registered=1
    break
  fi
  sleep 3
done
if (( registered == 0 )); then
  echo "error: echo_agent never registered" >&2
  "${COMPOSE[@]}" logs --tail=80 runner-1 runner-2 >&2 || true
  exit 1
fi
echo "    echo_agent registered"

echo "==> creating thread and running echo_agent"
thread_json="$(curl -sf -X POST "$BASE/threads" \
  -H 'Content-Type: application/json' \
  -d '{}')"
thread_id="$(echo "$thread_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
if [[ -z "$thread_id" ]]; then
  echo "error: no thread_id in create response: $thread_json" >&2
  exit 1
fi

run_json="$(curl -sf -X POST "$BASE/threads/${thread_id}/runs" \
  -H 'Content-Type: application/json' \
  -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"human","content":"multi-cp smoke"}]}}')"
run_id="$(echo "$run_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
if [[ -z "$run_id" ]]; then
  echo "error: no run_id in create response: $run_json" >&2
  exit 1
fi
echo "    thread_id=$thread_id run_id=$run_id"

echo "==> waiting for run success"
deadline=$((SECONDS + 120))
status=""
while (( SECONDS < deadline )); do
  status="$(curl -sf "$BASE/threads/${thread_id}/runs/${run_id}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
  if [[ "$status" == "success" ]]; then
    break
  fi
  if [[ "$status" == "error" || "$status" == "timeout" || "$status" == "interrupted" ]]; then
    echo "error: run ended with status=$status" >&2
    exit 1
  fi
  sleep 2
done
if [[ "$status" != "success" ]]; then
  echo "error: run did not reach success (last status=$status)" >&2
  exit 1
fi
echo "    run success"

echo "==> Admin API overview (what the UI Overview page polls)"
overview="$(curl -sf "$BASE/admin-api/overview")"
echo "$overview" | python3 -c '
import json,sys
o=json.load(sys.stdin)
print("    agents=%s threads=%s runs=%s cron=%s" % (
    o.get("total_agents"), o.get("total_threads"),
    o.get("total_runs"), o.get("cron_schedule_count")))
if (o.get("total_agents") or 0) < 1 or (o.get("total_runs") or 0) < 1:
    sys.exit("overview missing agents/runs")
'

echo "==> round-robin health probes (check lb logs for upstream= rotation)"
for i in 1 2 3 4 5 6; do
  curl -sf "$BASE/health" >/dev/null
done

cat <<EOF

PASS — multi-CP smoke complete.

Admin UI (live stack left running):
  $BASE/admin/

What you can watch there:
  - Overview: agent/thread/run counts (polls /admin-api/overview)
  - Runs / Threads: the smoke run above
  - Cron: yearly-echo schedule from all_agents config
  - Run detail: SSE event log for a specific run

What Admin is NOT:
  - Distributed tracing / Jaeger / Langfuse spans (needs OTEL_EXPORTER_OTLP_ENDPOINT)
  - Per-replica process logs (use: docker compose -f docker-compose.multi.yml logs -f)

Replica identity in logs:
  docker compose -f docker-compose.multi.yml logs runkite-1 runkite-2 runkite-3 | grep hostname
  docker compose -f docker-compose.multi.yml logs lb   # nginx upstream=...

Tear down when done:
  make multi-down
EOF
