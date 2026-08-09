#!/usr/bin/env bash
# Kind Helm runner-token rotation proof (K1-shaped).
#
# Boots like kind-helm-smoke (or ATTACH=1 to an existing KEEP_CLUSTER cluster),
# walks the dual-token allowlist recipe (old → old,new → new), and proves
# echo_agent runs succeed before, during, and after revoke.
#
# Usage (from repo root):
#   make kind-helm-rotate
#   KEEP_CLUSTER=1 bash scripts/kind-helm-smoke.sh   # optional prep
#   ATTACH=1 KEEP_CLUSTER=1 bash scripts/kind-helm-rotate.sh
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
OLD=""
NEW=""

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

apply_creds() {
  local runner_tok="$1"
  local cp_allow="$2"
  kubectl -n "${NS}" create secret generic runkite-creds \
    --from-literal=POSTGRES_DSN="${POSTGRES_DSN}" \
    --from-literal=REDIS_URL="${REDIS_URL}" \
    --from-literal=RUNNER_TOKEN="${runner_tok}" \
    --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH="${cp_allow}" \
    --from-literal=RUNKITE_API_KEY="${API_KEY}" \
    --dry-run=client -o yaml | kubectl apply -f -
}

rollout() {
  local deploy="$1"
  kubectl -n "${NS}" rollout restart "deployment/${deploy}"
  kubectl -n "${NS}" rollout status "deployment/${deploy}" --timeout=180s
}

stop_port_forward() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  PF_PID=""
}

start_port_forward() {
  # Rolling CP restarts change Endpoints; a stale kubectl port-forward
  # keeps talking to a dead pod and makes /readyz look down forever.
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
  local refreshed=0
  until curl -sf "${BASE}/readyz" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "error: /readyz not ready within 180s" >&2
      kubectl -n "${NS}" get pods -o wide >&2 || true
      kubectl -n "${NS}" logs "deploy/${RELEASE}" --tail=80 >&2 || true
      exit 1
    fi
    # One mid-wait refresh if the forward died or stuck on a drained pod.
    if (( refreshed == 0 )) && (( SECONDS > deadline - 120 )); then
      echo "    refreshing port-forward (stale after CP roll?)"
      start_port_forward
      refreshed=1
    fi
    sleep 2
  done
  echo "    readyz ok"
}

wait_echo_registered() {
  local auth_hdr=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
  echo "==> waiting for echo_agent registration"
  local deadline=$((SECONDS + 180))
  local registered=0
  while (( SECONDS < deadline )); do
    body="$(curl -sf -X POST "${BASE}/agents/search" "${auth_hdr[@]}" -d '{"limit":100}' 2>/dev/null || true)"
    if echo "${body}" | grep -q 'echo_agent'; then
      registered=1
      break
    fi
    sleep 3
  done
  if (( registered == 0 )); then
    echo "error: echo_agent never registered" >&2
    kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=80 >&2 || true
    exit 1
  fi
  echo "    echo_agent registered"
}

run_echo() {
  local label="$1"
  local auth_hdr=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
  echo "==> echo_agent run (${label})"
  local thread_json thread_id run_json run_id status deadline
  thread_json="$(curl -sf -X POST "${BASE}/threads" "${auth_hdr[@]}" -d '{}')"
  thread_id="$(echo "${thread_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
  if [[ -z "${thread_id}" ]]; then
    echo "error: [${label}] no thread_id: ${thread_json}" >&2
    exit 1
  fi
  run_json="$(curl -sf -X POST "${BASE}/threads/${thread_id}/runs" "${auth_hdr[@]}" \
    -d "{\"agent_id\":\"echo_agent\",\"input\":{\"messages\":[{\"role\":\"human\",\"content\":\"kind helm rotate ${label}\"}]}}")"
  run_id="$(echo "${run_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
  if [[ -z "${run_id}" ]]; then
    echo "error: [${label}] no run_id: ${run_json}" >&2
    exit 1
  fi
  deadline=$((SECONDS + 180))
  status=""
  while (( SECONDS < deadline )); do
    status="$(curl -sf "${BASE}/threads/${thread_id}/runs/${run_id}" "${auth_hdr[@]}" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
    if [[ "${status}" == "success" ]]; then
      echo "    ${label}: success (run_id=${run_id})"
      return
    fi
    if [[ "${status}" == "error" || "${status}" == "timeout" || "${status}" == "interrupted" ]]; then
      echo "error: [${label}] run ended with status=${status}" >&2
      kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=80 >&2 || true
      exit 1
    fi
    sleep 2
  done
  echo "error: [${label}] run did not reach success (last status=${status})" >&2
  kubectl -n "${NS}" get pods -o wide >&2 || true
  exit 1
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
  OLD="$(openssl rand -hex 32)"
  apply_creds "${OLD}" "${OLD}"

  echo "==> helm install (values-supported, runner.replicaCount=2)"
  helm upgrade --install "${RELEASE}" "${ROOT}/deploy/helm/runkite" \
    --namespace "${NS}" \
    -f "${ROOT}/deploy/helm/runkite/values.yaml" \
    -f "${ROOT}/deploy/helm/runkite/values-supported.yaml" \
    --set secrets.existingSecret=runkite-creds \
    --set runner.replicaCount=2 \
    --set image.repository=runkite \
    --set image.tag=kind-smoke \
    --set image.pullPolicy=IfNotPresent \
    --set runner.image.repository=runkite-runner \
    --set runner.image.tag=kind-smoke \
    --set runner.image.pullPolicy=IfNotPresent \
    --wait --timeout 5m

  kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s
}

boot_attach() {
  echo "==> ATTACH=1 — using existing kind cluster '${CLUSTER}'"
  if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
    echo "error: cluster '${CLUSTER}' not found; run kind-helm-smoke with KEEP_CLUSTER=1 first, or omit ATTACH" >&2
    exit 1
  fi
  kubectl config use-context "kind-${CLUSTER}" >/dev/null
  if ! kubectl -n "${NS}" get secret runkite-creds >/dev/null 2>&1; then
    echo "error: secret runkite-creds missing in ${NS}" >&2
    exit 1
  fi
  API_KEY="$(secret_val RUNKITE_API_KEY)"
  OLD="$(secret_val RUNNER_TOKEN)"
  local replicas
  replicas="$(kubectl -n "${NS}" get deploy/"${RELEASE}"-runner -o jsonpath='{.spec.replicas}')"
  if [[ -z "${replicas}" || "${replicas}" -lt 2 ]]; then
    echo "==> scaling runner to 2 replicas for rolling rotation"
    kubectl -n "${NS}" scale "deployment/${RELEASE}-runner" --replicas=2
  fi
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
  kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s
}

# --- boot ---
if [[ "${ATTACH:-}" == "1" ]]; then
  boot_attach
else
  boot_full
fi

start_port_forward
wait_readyz
wait_echo_registered

NEW="$(openssl rand -hex 32)"
echo "==> rotation tokens: OLD=${OLD:0:8}… NEW=${NEW:0:8}…"

run_echo baseline

echo "==> phase: CP allowlist OLD,NEW (runners still on OLD)"
apply_creds "${OLD}" "${OLD},${NEW}"
rollout "${RELEASE}"
start_port_forward
wait_readyz
run_echo dual-old-runner

echo "==> phase: runners → NEW (CP still allows OLD,NEW)"
apply_creds "${NEW}" "${OLD},${NEW}"
rollout "${RELEASE}-runner"
wait_echo_registered
run_echo dual-new-runner

echo "==> phase: revoke OLD (CP allowlist NEW only)"
apply_creds "${NEW}" "${NEW}"
rollout "${RELEASE}"
start_port_forward
wait_readyz
run_echo post-revoke

cat <<EOF

PASS — kind Helm runner-token rotation complete.

  cluster:   ${CLUSTER} (deleted on exit unless KEEP_CLUSTER=1)
  namespace: ${NS}
  proof:     echo_agent success at baseline, dual-old-runner, dual-new-runner, post-revoke
  recipe:    CP allowlist old,new → roll CP → runners to new → roll runners → allowlist new → roll CP

Non-claims: not Kubernetes Supported / EKS / soak / TLS; not hot-reload
(tokens load at process start); not unique-per-pod secrets; not K2 reclaim.
EOF
