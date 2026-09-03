#!/usr/bin/env bash
# EKS Helm smoke (K4 install proof).
#
# Prereq: cluster already exists (make eks-up). Does NOT create/delete EKS.
# Builds images, pushes to ECR, installs in-cluster Postgres/Redis + Helm
# values-supported, proves /readyz + echo_agent, then kills one CP pod and
# runs again (multi-replica survival on cloud).
#
# Usage (repo root):
#   make eks-smoke
#   KEEP_NS=1 bash scripts/eks-helm-smoke.sh
#
# See docs/k8s-eks-soak.md
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

REGION="${AWS_REGION:-eu-north-1}"
CLUSTER="${EKS_CLUSTER_NAME:-runkite-k4}"
NS="${EKS_NS:-runkite-eks}"
RELEASE=runkite
LOCAL_PORT="${EKS_SMOKE_PORT:-13026}"
BASE="http://127.0.0.1:${LOCAL_PORT}"
TAG="k4-$(date +%Y%m%d%H%M%S)"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: missing prerequisite '$1'" >&2
    exit 1
  }
}

need aws
need kubectl
need helm
need docker
need curl
need python3
need openssl

if ! aws sts get-caller-identity >/dev/null 2>&1; then
  echo "error: AWS credentials missing. Run: aws configure  (default region eu-north-1)" >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "error: docker daemon not reachable" >&2
  exit 1
fi

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
ECR="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com"
CP_REPO=runkite
RUNNER_REPO=runkite-runner
CP_IMAGE="${ECR}/${CP_REPO}:${TAG}"
RUNNER_IMAGE="${ECR}/${RUNNER_REPO}:${TAG}"

API_KEY="$(openssl rand -hex 32)"
RUNNER_TOKEN="$(openssl rand -hex 32)"
PF_PID=""

cleanup() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  if [[ "${KEEP_NS:-}" == "1" ]]; then
    echo "==> KEEP_NS=1 — leaving namespace ${NS} (still run make eks-down when finished)"
  fi
}
trap cleanup EXIT

echo "==> kubeconfig ${CLUSTER} (${REGION})"
aws eks update-kubeconfig --name "${CLUSTER}" --region "${REGION}" >/dev/null
kubectl get ns >/dev/null

echo "==> ECR login + repos"
for repo in "${CP_REPO}" "${RUNNER_REPO}"; do
  aws ecr describe-repositories --region "${REGION}" --repository-names "${repo}" >/dev/null 2>&1 \
    || aws ecr create-repository --region "${REGION}" --repository-name "${repo}" >/dev/null
done
aws ecr get-login-password --region "${REGION}" \
  | docker login --username AWS --password-stdin "${ECR}" >/dev/null

echo "==> build + push ${TAG}"
docker build -t "${CP_IMAGE}" -f Dockerfile .
docker build -t "${RUNNER_IMAGE}" -f Dockerfile.runner .
docker push "${CP_IMAGE}"
docker push "${RUNNER_IMAGE}"

echo "==> namespace + data plane"
kubectl get ns "${NS}" >/dev/null 2>&1 || kubectl create namespace "${NS}"
sed "s/runkite-smoke/${NS}/g" "${ROOT}/deploy/kind/data-plane.yaml" | kubectl apply -f -
# Namespace object in the manifest may recreate; ensure we use NS
kubectl -n "${NS}" rollout status deployment/postgres --timeout=180s
kubectl -n "${NS}" rollout status deployment/redis --timeout=120s

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

echo "==> helm install"
helm upgrade --install "${RELEASE}" "${ROOT}/deploy/helm/runkite" \
  --namespace "${NS}" \
  -f "${ROOT}/deploy/helm/runkite/values.yaml" \
  -f "${ROOT}/deploy/helm/runkite/values-supported.yaml" \
  --set secrets.existingSecret=runkite-creds \
  --set image.repository="${ECR}/${CP_REPO}" \
  --set image.tag="${TAG}" \
  --set image.pullPolicy=IfNotPresent \
  --set runner.image.repository="${ECR}/${RUNNER_REPO}" \
  --set runner.image.tag="${TAG}" \
  --set runner.image.pullPolicy=IfNotPresent \
  --wait --timeout 10m

kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=300s
kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=300s

start_pf() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  kubectl -n "${NS}" port-forward "svc/${RELEASE}" "${LOCAL_PORT}:2026" >/tmp/runkite-eks-pf.log 2>&1 &
  PF_PID=$!
  sleep 2
  if ! kill -0 "${PF_PID}" 2>/dev/null; then
    echo "error: port-forward failed; see /tmp/runkite-eks-pf.log" >&2
    cat /tmp/runkite-eks-pf.log >&2 || true
    exit 1
  fi
}

start_pf

auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")

wait_readyz() {
  local deadline=$((SECONDS + 180))
  until curl -sf "${BASE}/readyz" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "error: /readyz timeout" >&2
      kubectl -n "${NS}" get pods -o wide >&2 || true
      exit 1
    fi
    sleep 2
  done
}

echo "==> /readyz"
wait_readyz
echo "    ok"

echo "==> wait echo_agent"
deadline=$((SECONDS + 240))
body=""
while (( SECONDS < deadline )); do
  body="$(curl -sf -X POST "${BASE}/agents/search" "${auth[@]}" -d '{"limit":100}' 2>/dev/null || true)"
  echo "${body}" | grep -q 'echo_agent' && break
  sleep 3
done
if ! echo "${body}" | grep -q 'echo_agent'; then
  echo "error: echo_agent not registered" >&2
  kubectl -n "${NS}" logs "deploy/${RELEASE}-runner" --tail=100 >&2 || true
  exit 1
fi
echo "    registered"

run_echo() {
  local label="$1"
  local thread_json thread_id run_json run_id status
  thread_json="$(curl -sf -X POST "${BASE}/threads" "${auth[@]}" -d '{}')"
  thread_id="$(echo "${thread_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
  [[ -n "${thread_id}" ]] || {
    echo "error: no thread_id (${label}): ${thread_json}" >&2
    return 1
  }
  run_json="$(curl -sf -X POST "${BASE}/threads/${thread_id}/runs" "${auth[@]}" \
    -d "{\"agent_id\":\"echo_agent\",\"input\":{\"messages\":[{\"role\":\"human\",\"content\":\"eks ${label}\"}]}}")"
  run_id="$(echo "${run_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
  [[ -n "${run_id}" ]] || {
    echo "error: no run_id (${label}): ${run_json}" >&2
    return 1
  }
  echo "    ${label}: thread=${thread_id} run=${run_id}"
  local d=$((SECONDS + 180))
  status=""
  while (( SECONDS < d )); do
    status="$(curl -sf "${BASE}/threads/${thread_id}/runs/${run_id}" "${auth[@]}" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
    if [[ "${status}" == "success" ]]; then
      echo "    ${label}: success"
      LAST_RUN_ID="${run_id}"
      return 0
    fi
    if [[ "${status}" == "error" || "${status}" == "timeout" || "${status}" == "interrupted" ]]; then
      echo "error: ${label} status=${status}" >&2
      return 1
    fi
    sleep 2
  done
  echo "error: ${label} timeout (last status=${status})" >&2
  return 1
}

echo "==> smoke run"
run_echo smoke
SMOKE_RUN="${LAST_RUN_ID}"

echo "==> kill one CP pod, then run again"
CP_POD="$(kubectl -n "${NS}" get pods -l "app.kubernetes.io/instance=${RELEASE}" \
  --field-selector=status.phase=Running \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -v runner | head -n1)"
[[ -n "${CP_POD}" ]] || {
  echo "error: no CP pod found to kill" >&2
  exit 1
}
echo "    deleting ${CP_POD}"
kubectl -n "${NS}" delete pod "${CP_POD}" --force --grace-period=0 >/dev/null
start_pf
kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
wait_readyz
run_echo after-kill

STARTED="$(date -u +%Y-%m-%dT%H:%MZ)"
RESULTS="${ROOT}/docs/k8s-eks-soak-results.md"
cat > "${RESULTS}" <<EOF
# EKS K4 soak results

| Field | Value |
|-------|--------|
| Account | \`${ACCOUNT}\` |
| Region | \`${REGION}\` |
| Cluster | \`${CLUSTER}\` |
| Image tag | \`${TAG}\` |
| Namespace | \`${NS}\` |
| Smoke (UTC) | ${STARTED} |
| Torn down | _(fill after \`make eks-down\`)_ |
| Approx spend | _(Billing next day)_ |
| eks-smoke | **PASS** (echo_agent + CP pod kill + after-kill) |
| Smoke run_id | \`${SMOKE_RUN}\` |
| Optional soak loop | |
| Known issues | |

Do not call Kubernetes Supported in announce docs until tear-down + spend are filled.
EOF

cat <<EOF

PASS — EKS Helm smoke complete.

  cluster:   ${CLUSTER} (${REGION})
  namespace: ${NS}
  images:    ${CP_IMAGE}
             ${RUNNER_IMAGE}
  results:   docs/k8s-eks-soak-results.md

Next:
  KEEP_NS=1 already? then: SOAK_MINUTES=30 make eks-soak-loop
  ALWAYS: make eks-down when finished (billing stops when cluster is gone)

EOF
