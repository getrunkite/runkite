#!/usr/bin/env bash
# Optional short EKS run loop. Requires an install left up with KEEP_NS=1.
#
#   KEEP_NS=1 make eks-smoke
#   SOAK_MINUTES=30 make eks-soak-loop
#   make eks-down
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

REGION="${AWS_REGION:-eu-north-1}"
CLUSTER="${EKS_CLUSTER_NAME:-runkite-k4}"
NS="${EKS_NS:-runkite-eks}"
RELEASE=runkite
LOCAL_PORT="${EKS_SMOKE_PORT:-13026}"
BASE="http://127.0.0.1:${LOCAL_PORT}"
MINUTES="${SOAK_MINUTES:-30}"

aws eks update-kubeconfig --name "${CLUSTER}" --region "${REGION}" >/dev/null

API_KEY="$(kubectl -n "${NS}" get secret runkite-creds -o jsonpath='{.data.RUNKITE_API_KEY}' | base64 --decode)"
[[ -n "${API_KEY}" ]] || {
  echo "error: runkite-creds missing — run KEEP_NS=1 make eks-smoke first" >&2
  exit 1
}

kubectl -n "${NS}" port-forward "svc/${RELEASE}" "${LOCAL_PORT}:2026" >/tmp/runkite-eks-soak-pf.log 2>&1 &
PF_PID=$!
trap 'kill ${PF_PID} 2>/dev/null || true' EXIT
sleep 2
kill -0 "${PF_PID}" 2>/dev/null || {
  echo "error: port-forward failed; see /tmp/runkite-eks-soak-pf.log" >&2
  exit 1
}

auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
deadline=$((SECONDS + MINUTES * 60))
ok=0
fail=0
echo "==> soak loop ${MINUTES}m"
while (( SECONDS < deadline )); do
  thread_json="$(curl -sf -X POST "${BASE}/threads" "${auth[@]}" -d '{}')" || {
    fail=$((fail + 1))
    sleep 5
    continue
  }
  thread_id="$(echo "${thread_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
  run_json="$(curl -sf -X POST "${BASE}/threads/${thread_id}/runs" "${auth[@]}" \
    -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"human","content":"soak"}]}}')" || {
    fail=$((fail + 1))
    sleep 5
    continue
  }
  run_id="$(echo "${run_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
  status=""
  for _ in $(seq 1 60); do
    status="$(curl -sf "${BASE}/threads/${thread_id}/runs/${run_id}" "${auth[@]}" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
    [[ "${status}" == "success" || "${status}" == "error" || "${status}" == "timeout" || "${status}" == "interrupted" ]] && break
    sleep 2
  done
  if [[ "${status}" == "success" ]]; then
    ok=$((ok + 1))
  else
    fail=$((fail + 1))
  fi
  echo "    ok=${ok} fail=${fail} last=${status}"
  sleep 3
done

echo "PASS — soak loop done: ok=${ok} fail=${fail} minutes=${MINUTES}"
echo "Remember: make eks-down"
