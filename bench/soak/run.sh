#!/usr/bin/env bash
# 30-minute multi-control-plane end-to-end soak (laptop-scale).
#
# Brings up 3 CP + nginx + 2 Python runners with soak agents, webhooks,
# cron, connectors; drives parallel Python (langgraph_sdk) + JS HTTP clients;
# samples Admin overview, Prometheus metrics, docker stats, webhook counts.
#
# Pass criteria / announce writeup: bench/soak/WRITEUP.md
#
# Usage (from repo root):
#   SOAK_DURATION=1800 make soak-multi
#   make soak-multi-short                  # 10 min rehearsal
#   SOAK_DURATION=120 bash bench/soak/run.sh   # shorter rehearsal
#
# Watch live: http://localhost:2026/admin/
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

DURATION="${SOAK_DURATION:-1800}"
PY_WORKERS="${SOAK_PY_WORKERS:-16}"
JS_WORKERS="${SOAK_JS_WORKERS:-12}"
URL="${SOAK_URL:-http://127.0.0.1:2026}"
OUT_DIR="${SOAK_OUT:-$ROOT/bench/soak/out/$(date +%Y%m%d-%H%M%S)}"
COMPOSE=(docker compose -f docker-compose.multi.yml -f docker-compose.soak.yml)

mkdir -p "$OUT_DIR"
exec > >(tee -a "$OUT_DIR/console.log") 2>&1

echo "== soak start $(date -u +%Y-%m-%dT%H:%M:%SZ) duration=${DURATION}s py=${PY_WORKERS} js=${JS_WORKERS} =="
echo "artifacts: $OUT_DIR"
echo "admin UI:  ${URL}/admin/"

SINK_PORT="${SOAK_SINK_PORT:-9099}"
PY_PID=""
JS_PID=""
SINK_PID=""
LOGS_PID=""
cleanup() {
  echo "== stopping loaders / sink / compose-log follow =="
  kill "$PY_PID" "$JS_PID" "$SINK_PID" "$LOGS_PID" 2>/dev/null || true
  wait "$PY_PID" "$JS_PID" "$SINK_PID" "$LOGS_PID" 2>/dev/null || true
}
trap cleanup EXIT

# --- webhook + preflight sink (refuse orphan :9099 — an old sink would
# pass curl health checks while THIS run's webhooks.jsonl stayed empty) ---
if command -v lsof >/dev/null 2>&1; then
  occupied="$(lsof -nP -iTCP:"$SINK_PORT" -sTCP:LISTEN 2>/dev/null | awk 'NR>1 {print $1"("$2")"}' | sort -u | tr '\n' ' ' || true)"
  if [[ -n "${occupied// }" ]]; then
    echo "error: host process(es) already listening on :${SINK_PORT}: ${occupied}" >&2
    echo "       kill the orphan webhook_sink (or whatever holds the port), then re-run." >&2
    exit 1
  fi
fi

python3 "$ROOT/bench/soak/webhook_sink.py" "$SINK_PORT" "$OUT_DIR/webhooks.jsonl" &
SINK_PID=$!
sleep 0.5
if ! kill -0 "$SINK_PID" 2>/dev/null; then
  echo "FATAL: webhook sink exited immediately (port bind failed?)" >&2
  exit 1
fi
if command -v lsof >/dev/null 2>&1; then
  listener_pid="$(lsof -nP -iTCP:"$SINK_PORT" -sTCP:LISTEN -t 2>/dev/null | head -1 || true)"
  if [[ -z "$listener_pid" || "$listener_pid" != "$SINK_PID" ]]; then
    echo "FATAL: :${SINK_PORT} listener pid=${listener_pid:-none} is not our sink (pid=$SINK_PID)" >&2
    exit 1
  fi
fi
curl -sf "http://127.0.0.1:${SINK_PORT}/" >/dev/null

# --- stack (3 CP + lb + 2 runners) ---
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi
: "${RUNNER_TOKEN:?set RUNNER_TOKEN in .env or environment}"

echo "== compose up (multi + soak override) =="
"${COMPOSE[@]}" up -d --build --force-recreate runkite-1 runkite-2 runkite-3 lb runner-1 runner-2

# Stream full compose logs for the whole soak — end-of-run --tail=200 is
# useless after a mid-run outage (only shutdown noise remains).
"${COMPOSE[@]}" logs -f --no-color >"$OUT_DIR/compose-full.log" 2>&1 &
LOGS_PID=$!

echo "== wait for /readyz =="
for i in $(seq 1 90); do
  if curl -sf "$URL/readyz" >/dev/null; then
    echo "ready after ${i}s"
    break
  fi
  if [[ "$i" -eq 90 ]]; then
    echo "FATAL: control plane not ready"
    "${COMPOSE[@]}" ps
    exit 1
  fi
  sleep 1
done

echo "== wait for soak agents (runners register) =="
for i in $(seq 1 90); do
  agents="$(curl -sf "$URL/admin-api/agents" || true)"
  if echo "$agents" | grep -q llm_sim_agent && echo "$agents" | grep -q echo_agent; then
    echo "agents registered after ${i}s"
    echo "$agents" | python3 -m json.tool >"$OUT_DIR/agents.json" || echo "$agents" >"$OUT_DIR/agents.json"
    break
  fi
  if [[ "$i" -eq 90 ]]; then
    echo "FATAL: soak agents never registered"
    echo "$agents"
    "${COMPOSE[@]}" logs --tail=80 runner-1 runner-2 runkite-1
    exit 1
  fi
  sleep 1
done

curl -sf "$URL/admin-api/overview" | python3 -m json.tool >"$OUT_DIR/overview-start.json" || true
curl -sf "$URL/admin-api/connectors" | python3 -m json.tool >"$OUT_DIR/connectors-start.json" || true
curl -sf "$URL/metrics" >"$OUT_DIR/metrics-start.txt" || true

# --- python venv + langgraph_sdk ---
VENV="$ROOT/bench/soak/.venv"
if [[ ! -d "$VENV" ]]; then
  python3 -m venv "$VENV"
fi
# shellcheck disable=SC1091
source "$VENV/bin/activate"
pip -q install -U pip
pip -q install 'langgraph-sdk>=0.1.0' httpx

# --- loaders ---
echo "== starting loaders =="
python3 "$ROOT/bench/soak/load_python.py" --url "$URL" --workers "$PY_WORKERS" --duration "$DURATION" \
  >"$OUT_DIR/load-python.log" 2>&1 &
PY_PID=$!

SOAK_URL="$URL" SOAK_JS_WORKERS="$JS_WORKERS" SOAK_DURATION="$DURATION" \
  node "$ROOT/bench/soak/load_js.mjs" >"$OUT_DIR/load-js.log" 2>&1 &
JS_PID=$!

# --- sample loop ---
END=$((SECONDS + DURATION))
SAMPLE=0
while (( SECONDS < END )); do
  SAMPLE=$((SAMPLE + 1))
  TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  {
    echo "--- sample $SAMPLE $TS ---"
    curl -sf "$URL/admin-api/overview" || echo '{"error":"overview failed"}'
    echo
    echo "webhook_counts:"
    curl -sf "http://127.0.0.1:${SINK_PORT}/" || true
    echo
    echo "docker_stats:"
    # shellcheck disable=SC2046
    docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}' \
      $(docker ps --filter name=runkite-multi --format '{{.Names}}') 2>/dev/null || true
  } >>"$OUT_DIR/samples.log"

  # tail loader health
  echo "[$TS] py_tail=$(tail -n 1 "$OUT_DIR/load-python.log" 2>/dev/null || true)"
  echo "[$TS] js_tail=$(tail -n 1 "$OUT_DIR/load-js.log" 2>/dev/null || true)"

  sleep 30
done

wait "$PY_PID" || true
wait "$JS_PID" || true

curl -sf "$URL/admin-api/overview" | python3 -m json.tool >"$OUT_DIR/overview-end.json" || true
curl -sf "$URL/admin-api/webhooks/dead-letters" | python3 -m json.tool >"$OUT_DIR/dead-letters.json" || true
curl -sf "$URL/admin-api/connectors" | python3 -m json.tool >"$OUT_DIR/connectors-end.json" || true
curl -sf "$URL/metrics" >"$OUT_DIR/metrics-end.txt" || true
# Stop the follow so the file flushes; keep a short end snapshot too.
kill "$LOGS_PID" 2>/dev/null || true
wait "$LOGS_PID" 2>/dev/null || true
LOGS_PID=""
"${COMPOSE[@]}" logs --no-color --tail=200 >"$OUT_DIR/compose-tail.log" 2>&1 || true

python3 - <<PY
import json, re
from pathlib import Path
out = Path("$OUT_DIR")
report = []
report.append("# Soak report")
report.append(f"- ended: open artifacts in \`{out}\`")
report.append(f"- duration_s: $DURATION")
report.append(f"- py_workers: $PY_WORKERS  js_workers: $JS_WORKERS")
report.append("")
for name in ("overview-start.json", "overview-end.json", "connectors-end.json"):
    p = out / name
    if p.exists():
        report.append(f"## {name}")
        report.append("\`\`\`json")
        report.append(p.read_text()[:4000])
        report.append("\`\`\`")
        report.append("")
wh = out / "webhooks.jsonl"
if wh.exists():
    from collections import Counter
    c = Counter()
    for line in wh.read_text().splitlines():
        try:
            ev = json.loads(line).get("event") or {}
            c[ev.get("type", "?")] += 1
        except Exception:
            c["parse_error"] += 1
    report.append("## webhook deliveries")
    report.append(str(dict(c)))
    report.append("")
for log in ("load-python.log", "load-js.log"):
    p = out / log
    if p.exists():
        lines = p.read_text().splitlines()
        report.append(f"## {log} (last 15 lines)")
        report.append("\`\`\`")
        report.extend(lines[-15:])
        report.append("\`\`\`")
        report.append("")
# quick metrics deltas of interest
m = (out / "metrics-end.txt").read_text() if (out / "metrics-end.txt").exists() else ""
interesting = [
    "runkite_runs_total",
    "runkite_webhook",
    "runkite_queue",
    "go_memstats_alloc_bytes",
    "process_resident_memory_bytes",
]
report.append("## metrics (filtered)")
report.append("\`\`\`")
for line in m.splitlines():
    if any(k in line for k in interesting) and not line.startswith("#"):
        report.append(line)
report.append("\`\`\`")
(out / "REPORT.md").write_text("\n".join(report) + "\n")
print((out / "REPORT.md").read_text())
PY

echo "== soak complete $(date -u +%Y-%m-%dT%H:%M:%SZ) =="
echo "report: $OUT_DIR/REPORT.md"
echo "stack left UP — Admin: ${URL}/admin/  (make multi-down to tear down)"
# keep stack up; only stop loaders/sink via trap
trap - EXIT
kill "$SINK_PID" 2>/dev/null || true
