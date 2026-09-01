#!/usr/bin/env bash
# Kind Helm install smoke (K0-shaped).
#
# Creates a kind cluster, installs plain Postgres+Redis, loads locally built
# images, helm-installs values-supported, runs /readyz + one echo_agent turn,
# then deletes the cluster. Does NOT claim Kubernetes Supported, EKS, soak,
# or TLS — see deploy/helm/runkite/README.md proof posture.
#
# Usage (from repo root):
#   make kind-helm-smoke
#   KEEP_CLUSTER=1 bash scripts/kind-helm-smoke.sh   # skip teardown (debug)
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
# Avoid colliding with a leftover docker-compose.multi LB on :2026.
LOCAL_PORT="${KIND_SMOKE_PORT:-12026}"
BASE="http://127.0.0.1:${LOCAL_PORT}"

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

API_KEY="$(openssl rand -hex 32)"
RUNNER_TOKEN="$(openssl rand -hex 32)"
PF_PID=""

cleanup() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    echo "==> KEEP_CLUSTER=1 — leaving kind cluster '${CLUSTER}' up"
    return
  fi
  echo "==> deleting kind cluster '${CLUSTER}'"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> kind cluster '${CLUSTER}'"
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

echo "==> credentials Secret"
POSTGRES_DSN='postgres://runkite:runkite@postgres:5432/runkite?sslmode=disable'
REDIS_URL='redis://redis:6379'
kubectl -n "${NS}" create secret generic runkite-creds \
  --from-literal=POSTGRES_DSN="${POSTGRES_DSN}" \
  --from-literal=REDIS_URL="${REDIS_URL}" \
  --from-literal=RUNNER_TOKEN="${RUNNER_TOKEN}" \
  --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH="${RUNNER_TOKEN}" \
  --from-literal=RUNNER_TENANTS_PYTHON_LANGGRAPH=default \
  --from-literal=RUNKITE_API_KEY="${API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> helm install (values-supported)"
helm upgrade --install "${RELEASE}" "${ROOT}/deploy/helm/runkite" \
  --namespace "${NS}" \
  -f "${ROOT}/deploy/helm/runkite/values.yaml" \
  -f "${ROOT}/deploy/helm/runkite/values-supported.yaml" \
  --set secrets.existingSecret=runkite-creds \
  --set image.repository=runkite \
  --set image.tag=kind-smoke \
  --set image.pullPolicy=IfNotPresent \
  --set runner.image.repository=runkite-runner \
  --set runner.image.tag=kind-smoke \
  --set runner.image.pullPolicy=IfNotPresent \
  --wait --timeout 5m

echo "==> waiting for control plane + runner Ready"
kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s

echo "==> port-forward svc/${RELEASE} ${LOCAL_PORT}:2026"
kubectl -n "${NS}" port-forward "svc/${RELEASE}" "${LOCAL_PORT}:2026" >/tmp/runkite-kind-pf.log 2>&1 &
PF_PID=$!
sleep 1
if ! kill -0 "${PF_PID}" 2>/dev/null; then
  echo "error: port-forward exited immediately; see /tmp/runkite-kind-pf.log" >&2
  cat /tmp/runkite-kind-pf.log >&2 || true
  exit 1
fi

auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")

echo "==> waiting for /readyz"
deadline=$((SECONDS + 180))
until curl -sf "${BASE}/readyz" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "error: /readyz not ready within 180s" >&2
    kubectl -n "${NS}" get pods -o wide >&2 || true
    kubectl -n "${NS}" logs "deploy/${RELEASE}" --tail=80 >&2 || true
    exit 1
  fi
  sleep 2
done
echo "    readyz ok"

echo "==> waiting for echo_agent registration"
deadline=$((SECONDS + 180))
registered=0
while (( SECONDS < deadline )); do
  body="$(curl -sf -X POST "${BASE}/agents/search" "${auth[@]}" -d '{"limit":100}' 2>/dev/null || true)"
  if echo "${body}" | grep -q 'echo_agent'; then
    registered=1
    break
  fi
  sleep 3
done
if (( registered == 0 )); then
  echo "error: echo_agent never registered" >&2
  kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=80 >&2 || true
  kubectl -n "${NS}" logs "deploy/${RELEASE}" --tail=80 >&2 || true
  exit 1
fi
echo "    echo_agent registered"

echo "==> create thread + run echo_agent"
thread_json="$(curl -sf -X POST "${BASE}/threads" "${auth[@]}" -d '{}')"
thread_id="$(echo "${thread_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
if [[ -z "${thread_id}" ]]; then
  echo "error: no thread_id: ${thread_json}" >&2
  exit 1
fi

run_json="$(curl -sf -X POST "${BASE}/threads/${thread_id}/runs" "${auth[@]}" \
  -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"human","content":"kind helm smoke"}]}}')"
run_id="$(echo "${run_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
if [[ -z "${run_id}" ]]; then
  echo "error: no run_id: ${run_json}" >&2
  exit 1
fi
echo "    thread_id=${thread_id} run_id=${run_id}"

echo "==> waiting for run success"
deadline=$((SECONDS + 180))
status=""
while (( SECONDS < deadline )); do
  status="$(curl -sf "${BASE}/threads/${thread_id}/runs/${run_id}" "${auth[@]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
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
  kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=80 >&2 || true
  kubectl -n "${NS}" logs "deploy/${RELEASE}" --tail=80 >&2 || true
  exit 1
fi
echo "    run success"

cat <<EOF

PASS — kind Helm smoke complete.

  cluster:   ${CLUSTER} (deleted on exit unless KEEP_CLUSTER=1)
  namespace: ${NS}
  proof:     /readyz + echo_agent run success via values-supported

Kubernetes packaging remains Compatible. Compose multi soak remains the
Supported HA correctness proof. This is not an EKS or kind soak.
EOF
