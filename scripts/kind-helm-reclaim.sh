#!/usr/bin/env bash
# Kind Helm mid-run reclaim proof (K2-shaped).
#
# Boots values-supported + values-reclaim (slow_agent, 2 runners). Starts a
# slow_agent run, force-deletes one CP pod (K2 name), then force-deletes
# runner pods so heartbeats stop and ReclaimStale fires under Redis HA.
# Pass: surviving CP logs "reclaimed stale jobs" and the same run_id reaches
# success.
#
# Usage (from repo root):
#   make kind-helm-reclaim
#   ATTACH=1 KEEP_CLUSTER=1 bash scripts/kind-helm-reclaim.sh
#
# Prereqs: kind, kubectl, helm, docker, curl, python3, openssl
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

CLUSTER="${KIND_CLUSTER_NAME:-runkite-helm-smoke}"
NS=runkite-smoke
RELEASE=runkite
CP_IMAGE=runkite:kind-smoke
RUNNER_IMAGE=runkite-runner:kind-smoke
LOCAL_PORT="${KIND_SMOKE_PORT:-12026}"
BASE="http://127.0.0.1:${LOCAL_PORT}"
POSTGRES_DSN='postgres://runkite:runkite@postgres:5432/runkite?sslmode=disable'
REDIS_URL='redis://redis:6379'

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: missing prerequisite '$1'" >&2
    case "$1" in
      kind) echo "       install: https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2 ;;
      kubectl) echo "       install: https://kubernetes.io/docs/tasks/tools/" >&2 ;;
      helm) echo "       install: https://helm.sh/docs/intro/install/" >&2 ;;
      docker) echo "       install Docker / OrbStack and ensure the daemon is running" >&2 ;;
    esac
    exit 1
  fi
}

need kind
need kubectl
need helm
need docker
need curl
need python3
need openssl

if ! docker info >/dev/null 2>&1; then
  echo "error: docker daemon not reachable" >&2
  exit 1
fi

PF_PID=""
API_KEY=""

stop_port_forward() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  PF_PID=""
}

cleanup() {
  stop_port_forward
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    echo "==> KEEP_CLUSTER=1 — leaving kind cluster '${CLUSTER}' up"
    return
  fi
  echo "==> deleting kind cluster '${CLUSTER}'"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

secret_val() {
  kubectl -n "${NS}" get secret runkite-creds -o "jsonpath={.data.$1}" \
    | python3 -c 'import sys, base64; print(base64.b64decode(sys.stdin.read()).decode())'
}

start_port_forward() {
  stop_port_forward
  echo "==> port-forward svc/${RELEASE} ${LOCAL_PORT}:2026"
  kubectl -n "${NS}" port-forward "svc/${RELEASE}" "${LOCAL_PORT}:2026" >/tmp/runkite-kind-pf.log 2>&1 &
  PF_PID=$!
  sleep 1
  if ! kill -0 "${PF_PID}" 2>/dev/null; then
    echo "error: port-forward exited immediately; see /tmp/runkite-kind-pf.log" >&2
    cat /tmp/runkite-kind-pf.log >&2 || true
    exit 1
  fi
}

wait_readyz() {
  echo "==> waiting for /readyz"
  local deadline=$((SECONDS + 180))
  until curl -sf "${BASE}/readyz" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "error: /readyz not ready within 180s" >&2
      kubectl -n "${NS}" get pods -o wide >&2 || true
      exit 1
    fi
    sleep 2
  done
  echo "    readyz ok"
}

wait_agent() {
  local agent_id="$1"
  local auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
  echo "==> waiting for ${agent_id} registration"
  local deadline=$((SECONDS + 180)) registered=0 body
  while (( SECONDS < deadline )); do
    body="$(curl -sf -X POST "${BASE}/agents/search" "${auth[@]}" -d '{"limit":100}' 2>/dev/null || true)"
    if echo "${body}" | grep -q "${agent_id}"; then
      registered=1
      break
    fi
    sleep 3
  done
  if (( registered == 0 )); then
    echo "error: ${agent_id} never registered" >&2
    kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=80 >&2 || true
    exit 1
  fi
  echo "    ${agent_id} registered"
}

run_status() {
  local thread_id="$1" run_id="$2"
  local auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
  curl -sf "${BASE}/threads/${thread_id}/runs/${run_id}" "${auth[@]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true
}

helm_install_reclaim() {
  helm upgrade --install "${RELEASE}" "${ROOT}/deploy/helm/runkite" \
    --namespace "${NS}" \
    -f "${ROOT}/deploy/helm/runkite/values.yaml" \
    -f "${ROOT}/deploy/helm/runkite/values-supported.yaml" \
    -f "${ROOT}/deploy/kind/values-reclaim.yaml" \
    --set secrets.existingSecret=runkite-creds \
    --set image.repository=runkite \
    --set image.tag=kind-smoke \
    --set image.pullPolicy=IfNotPresent \
    --set runner.image.repository=runkite-runner \
    --set runner.image.tag=kind-smoke \
    --set runner.image.pullPolicy=IfNotPresent \
    --wait --timeout 5m
}

boot_full() {
  echo "==> kind cluster '${CLUSTER}' (full boot)"
  if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    kind delete cluster --name "${CLUSTER}"
  fi
  kind create cluster --name "${CLUSTER}"

  echo "==> data plane (Postgres + Redis)"
  kubectl apply -f "${ROOT}/deploy/kind/data-plane.yaml"
  kubectl -n "${NS}" rollout status deployment/postgres --timeout=180s
  kubectl -n "${NS}" rollout status deployment/redis --timeout=120s

  echo "==> building images"
  docker build -t "${CP_IMAGE}" -f Dockerfile .
  docker build -t "${RUNNER_IMAGE}" -f Dockerfile.runner .

  echo "==> loading images into kind"
  kind load docker-image "${CP_IMAGE}" --name "${CLUSTER}"
  kind load docker-image "${RUNNER_IMAGE}" --name "${CLUSTER}"

  API_KEY="$(openssl rand -hex 32)"
  local token
  token="$(openssl rand -hex 32)"
  kubectl -n "${NS}" create secret generic runkite-creds \
    --from-literal=POSTGRES_DSN="${POSTGRES_DSN}" \
    --from-literal=REDIS_URL="${REDIS_URL}" \
    --from-literal=RUNNER_TOKEN="${token}" \
    --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH="${token}" \
    --from-literal=RUNKITE_API_KEY="${API_KEY}" \
    --dry-run=client -o yaml | kubectl apply -f -

  echo "==> helm install (values-supported + values-reclaim)"
  helm_install_reclaim
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s
}

boot_attach() {
  echo "==> ATTACH=1 — using existing kind cluster '${CLUSTER}'"
  if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "error: cluster '${CLUSTER}' not found" >&2
    exit 1
  fi
  kubectl config use-context "kind-${CLUSTER}" >/dev/null
  API_KEY="$(secret_val RUNKITE_API_KEY)"
  echo "==> ensuring reclaim overlay (slow_agent + 2 runners)"
  helm_install_reclaim
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s
}

if [[ "${ATTACH:-}" == "1" ]]; then
  boot_attach
else
  boot_full
fi

start_port_forward
wait_readyz
wait_agent slow_agent

# Snapshot CP pod names before kill (bash 3.2-safe — no mapfile).
CP_PODS_BEFORE=()
while IFS= read -r _pod; do
  [[ -n "${_pod}" ]] && CP_PODS_BEFORE+=("${_pod}")
done < <(kubectl -n "${NS}" get pods \
  -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=control-plane" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
if (( ${#CP_PODS_BEFORE[@]} < 2 )); then
  echo "error: need >=2 control-plane pods, got ${#CP_PODS_BEFORE[@]}" >&2
  exit 1
fi
CP_VICTIM="${CP_PODS_BEFORE[0]}"
echo "==> CP victim for force-delete: ${CP_VICTIM}"

echo "==> create slow_agent run"
auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
thread_json="$(curl -sf -X POST "${BASE}/threads" "${auth[@]}" -d '{}')"
thread_id="$(echo "${thread_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
run_json="$(curl -sf -X POST "${BASE}/threads/${thread_id}/runs" "${auth[@]}" \
  -d '{"agent_id":"slow_agent","input":{"messages":[{"role":"human","content":"kind helm reclaim"}]}}')"
run_id="$(echo "${run_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
if [[ -z "${thread_id}" || -z "${run_id}" ]]; then
  echo "error: create failed thread=${thread_json} run=${run_json}" >&2
  exit 1
fi
echo "    thread_id=${thread_id} run_id=${run_id}"

# Let a runner dequeue and start step_1 (~2s sleeps).
echo "==> waiting briefly for run to be in-flight"
sleep 2
status="$(run_status "${thread_id}" "${run_id}")"
echo "    status before kill: ${status:-unknown}"

echo "==> force-delete one CP pod (K2 name)"
kubectl -n "${NS}" delete pod "${CP_VICTIM}" --grace-period=0 --force >/dev/null
start_port_forward
wait_readyz

echo "==> force-delete runner pods (stop heartbeats → ReclaimStale)"
# Delete all runner pods so whichever held the lease dies. Deployment
# recreates them; surviving CP reaper reclaims the job for a new attempt.
kubectl -n "${NS}" delete pods \
  -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=runner" \
  --grace-period=0 --force >/dev/null
kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s

echo "==> waiting for reclaim log on surviving CP"
deadline=$((SECONDS + 45))
reclaimed=0
while (( SECONDS < deadline )); do
  if kubectl -n "${NS}" logs \
    -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=control-plane" \
    --since=2m 2>/dev/null | grep -q 'reclaimed stale jobs'; then
    reclaimed=1
    break
  fi
  sleep 2
done
if (( reclaimed == 0 )); then
  echo "error: did not observe 'reclaimed stale jobs' within 45s" >&2
  kubectl -n "${NS}" logs \
    -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=control-plane" \
    --since=3m >&2 || true
  exit 1
fi
echo "    reclaim observed"
kubectl -n "${NS}" logs \
  -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=control-plane" \
  --since=2m 2>/dev/null | grep 'reclaimed stale jobs' | tail -3 || true

echo "==> waiting for run ${run_id} → success"
deadline=$((SECONDS + 120))
status=""
while (( SECONDS < deadline )); do
  status="$(run_status "${thread_id}" "${run_id}")"
  if [[ "${status}" == "success" ]]; then
    break
  fi
  if [[ "${status}" == "error" || "${status}" == "timeout" || "${status}" == "interrupted" ]]; then
    echo "error: run ended with status=${status}" >&2
    kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=80 >&2 || true
    exit 1
  fi
  sleep 2
done
if [[ "${status}" != "success" ]]; then
  echo "error: run did not reach success (last status=${status})" >&2
  kubectl -n "${NS}" get pods -o wide >&2 || true
  exit 1
fi
echo "    run success after reclaim"

cat <<EOF

PASS — kind Helm mid-run reclaim complete.

  cluster:   ${CLUSTER} (deleted on exit unless KEEP_CLUSTER=1)
  namespace: ${NS}
  proof:     CP pod force-deleted mid slow_agent; runners force-deleted;
             observed "reclaimed stale jobs"; run_id=${run_id} → success

Non-claims: not Kubernetes Supported / EKS / soak / TLS; not graceful
SIGTERM drain; not full fencing/supersede unpause dance; not "CP delete
alone triggers ReclaimStale under Redis HA" (runner heartbeat stop is
what arms the reaper).
EOF
