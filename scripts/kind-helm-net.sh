#!/usr/bin/env bash
# Kind Helm Ingress + NetworkPolicy proof (K3-shaped).
#
# Creates a kind cluster with hostPort 8080→80, installs ingress-nginx,
# helm-installs values-supported + values-net, then proves:
#   - external Host: runkite.local → /readyz + echo_agent success (no PF)
#   - NetworkPolicy deny-inbound on runner pods (probe cannot connect)
#
# Requires kind ≥ 0.24 (kindnet enforces NetworkPolicy).
#
# Usage (from repo root):
#   make kind-helm-net
#   KEEP_CLUSTER=1 bash scripts/kind-helm-net.sh
#
# Prereqs: kind, kubectl, helm, docker, curl, python3, openssl
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

CLUSTER="${KIND_CLUSTER_NAME:-runkite-helm-net}"
NS=runkite-smoke
RELEASE=runkite
CP_IMAGE=runkite:kind-smoke
RUNNER_IMAGE=runkite-runner:kind-smoke
# Host side of kind extraPortMappings (see deploy/kind/cluster-net.yaml).
HOST_HTTP_PORT="${KIND_NET_HTTP_PORT:-8080}"
BASE="http://127.0.0.1:${HOST_HTTP_PORT}"
HOST_HDR=(-H "Host: runkite.local")
POSTGRES_DSN='postgres://runkite:runkite@postgres:5432/runkite?sslmode=disable'
REDIS_URL='redis://redis:6379'
# Pin a known kind provider manifest (Admission webhook + ingress-ready).
INGRESS_NGINX_URL="${INGRESS_NGINX_MANIFEST:-https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.12.1/deploy/static/provider/kind/deploy.yaml}"

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

# kind ≥ 0.24 required for NetworkPolicy enforcement on kindnet.
kind_ver="$(kind version 2>/dev/null | awk '{print $2}' | sed 's/^v//')"
kind_major="${kind_ver%%.*}"
kind_minor="$(echo "${kind_ver}" | cut -d. -f2)"
if [[ -z "${kind_major}" || -z "${kind_minor}" ]] \
  || (( kind_major == 0 && kind_minor < 24 )); then
  echo "error: kind >= 0.24 required for NetworkPolicy enforcement (got ${kind_ver:-unknown})" >&2
  exit 1
fi

API_KEY=""

cleanup() {
  if [[ "${KEEP_CLUSTER:-}" == "1" ]]; then
    echo "==> KEEP_CLUSTER=1 — leaving kind cluster '${CLUSTER}' up"
    echo "    curl: curl -sS -H 'Host: runkite.local' ${BASE}/readyz"
    return
  fi
  echo "==> deleting kind cluster '${CLUSTER}'"
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

curl_host() {
  # External path via Ingress — Host header required.
  curl -sf --connect-timeout 5 --max-time 30 "${HOST_HDR[@]}" "$@"
}

wait_readyz() {
  echo "==> waiting for Ingress /readyz (Host: runkite.local → :${HOST_HTTP_PORT})"
  local deadline=$((SECONDS + 180))
  until curl_host "${BASE}/readyz" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "error: /readyz via Ingress not ready within 180s" >&2
      kubectl -n ingress-nginx get pods -o wide >&2 || true
      kubectl -n "${NS}" get ingress,svc,pods -o wide >&2 || true
      exit 1
    fi
    sleep 3
  done
  echo "    readyz ok via Ingress"
}

wait_echo() {
  local auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
  echo "==> waiting for echo_agent registration (via Ingress)"
  local deadline=$((SECONDS + 180)) registered=0 body
  while (( SECONDS < deadline )); do
    body="$(curl_host -X POST "${BASE}/agents/search" "${auth[@]}" -d '{"limit":100}' 2>/dev/null || true)"
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

echo "==> kind cluster '${CLUSTER}' (Ingress port-map)"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  kind delete cluster --name "${CLUSTER}"
fi
kind create cluster --name "${CLUSTER}" --config "${ROOT}/deploy/kind/cluster-net.yaml"

echo "==> install ingress-nginx (kind provider)"
kubectl apply -f "${INGRESS_NGINX_URL}"
kubectl -n ingress-nginx wait --for=condition=Available deployment/ingress-nginx-controller --timeout=180s
# Admission webhook Ready
deadline=$((SECONDS + 120))
until kubectl -n ingress-nginx get pods -l app.kubernetes.io/component=controller \
  -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null | grep -q true; do
  if (( SECONDS >= deadline )); then
    echo "error: ingress-nginx controller not Ready" >&2
    kubectl -n ingress-nginx get pods -o wide >&2 || true
    exit 1
  fi
  sleep 2
done
echo "    ingress-nginx Ready"

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
token="$(openssl rand -hex 32)"
kubectl -n "${NS}" create secret generic runkite-creds \
  --from-literal=POSTGRES_DSN="${POSTGRES_DSN}" \
  --from-literal=REDIS_URL="${REDIS_URL}" \
  --from-literal=RUNNER_TOKEN="${token}" \
  --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH="${token}" \
  --from-literal=RUNKITE_API_KEY="${API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> helm install (values-supported + values-net)"
helm upgrade --install "${RELEASE}" "${ROOT}/deploy/helm/runkite" \
  --namespace "${NS}" \
  -f "${ROOT}/deploy/helm/runkite/values.yaml" \
  -f "${ROOT}/deploy/helm/runkite/values-supported.yaml" \
  -f "${ROOT}/deploy/kind/values-net.yaml" \
  --set secrets.existingSecret=runkite-creds \
  --set image.repository=runkite \
  --set image.tag=kind-smoke \
  --set image.pullPolicy=IfNotPresent \
  --set runner.image.repository=runkite-runner \
  --set runner.image.tag=kind-smoke \
  --set runner.image.pullPolicy=IfNotPresent \
  --wait --timeout 5m

kubectl -n "${NS}" rollout status deployment/"${RELEASE}" --timeout=180s
kubectl -n "${NS}" rollout status deployment/"${RELEASE}"-runner --timeout=180s

echo "==> assert Ingress + NetworkPolicy objects"
kubectl -n "${NS}" get ingress/"${RELEASE}" >/dev/null
kubectl -n "${NS}" get networkpolicy/"${RELEASE}" >/dev/null
kubectl -n "${NS}" get networkpolicy/"${RELEASE}"-runner >/dev/null
echo "    ingress/${RELEASE} + networkpolicies present"

wait_readyz
wait_echo

echo "==> create echo_agent run via Ingress"
auth=(-H "Authorization: Bearer ${API_KEY}" -H "Content-Type: application/json")
thread_json="$(curl_host -X POST "${BASE}/threads" "${auth[@]}" -d '{}')"
thread_id="$(echo "${thread_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("thread_id",""))')"
run_json="$(curl_host -X POST "${BASE}/threads/${thread_id}/runs" "${auth[@]}" \
  -d '{"agent_id":"echo_agent","input":{"messages":[{"role":"human","content":"kind helm net"}]}}')"
run_id="$(echo "${run_json}" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("run_id",""))')"
if [[ -z "${thread_id}" || -z "${run_id}" ]]; then
  echo "error: create failed thread=${thread_json} run=${run_json}" >&2
  exit 1
fi
echo "    thread_id=${thread_id} run_id=${run_id}"

echo "==> waiting for run success (in-cluster runner)"
deadline=$((SECONDS + 120))
status=""
while (( SECONDS < deadline )); do
  status="$(curl_host "${BASE}/threads/${thread_id}/runs/${run_id}" "${auth[@]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true)"
  if [[ "${status}" == "success" ]]; then
    break
  fi
  if [[ "${status}" == "error" || "${status}" == "timeout" || "${status}" == "interrupted" ]]; then
    echo "error: run ended with status=${status}" >&2
    exit 1
  fi
  sleep 2
done
if [[ "${status}" != "success" ]]; then
  echo "error: run did not reach success (last status=${status})" >&2
  exit 1
fi
echo "    run success via Ingress"

echo "==> NetworkPolicy: deny inbound to runner (listen + A/B probe)"
# Closed ports make "deny" indistinguishable from connection refused. Run a
# throwaway listener with the runner selector labels (same NP target), then
# prove: NP on → remote nc fails; NP deleted → remote nc succeeds.
kubectl -n "${NS}" delete pod np-listener --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl -n "${NS}" run np-listener --restart=Never --image=busybox:1.36 \
  --labels="app.kubernetes.io/name=runkite,app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=runner" \
  --command -- nc -l -p 9999
kubectl -n "${NS}" wait --for=condition=Ready pod/np-listener --timeout=60s
LISTEN_IP="$(kubectl -n "${NS}" get pod np-listener -o jsonpath='{.status.podIP}')"
if [[ -z "${LISTEN_IP}" ]]; then
  echo "error: np-listener has no pod IP" >&2
  exit 1
fi
echo "    listener up on np-listener (${LISTEN_IP}):9999 (runner labels)"

probe_connect() {
  local name="$1"
  kubectl -n "${NS}" delete pod "${name}" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  # If NP drops packets, nc -w alone can hang; wrap with busybox timeout.
  kubectl -n "${NS}" run "${name}" --restart=Never --image=busybox:1.36 \
    --command -- /bin/sh -c "timeout 5 nc -z -w 2 ${LISTEN_IP} 9999; echo EXIT:\$?"
  local deadline=$((SECONDS + 60)) phase=""
  while (( SECONDS < deadline )); do
    phase="$(kubectl -n "${NS}" get pod "${name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]]; then
      break
    fi
    sleep 1
  done
  local log
  log="$(kubectl -n "${NS}" logs "${name}" 2>/dev/null || true)"
  kubectl -n "${NS}" delete pod "${name}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  echo "${log}"
}

deny_log="$(probe_connect np-probe-deny)"
echo "    with NP: ${deny_log:-<empty>}"
if echo "${deny_log}" | grep -q 'EXIT:0'; then
  echo "error: probe connected while runner NetworkPolicy deny-inbound was active" >&2
  exit 1
fi
if ! echo "${deny_log}" | grep -q 'EXIT:'; then
  echo "error: deny probe produced no EXIT line" >&2
  exit 1
fi

kubectl -n "${NS}" delete networkpolicy "${RELEASE}-runner" --wait=true
# nc -l accepts once; restart listener after the (failed) deny probe may have
# left it in a bad state, and after NP deletion so allow can complete.
kubectl -n "${NS}" delete pod np-listener --ignore-not-found --wait=true >/dev/null 2>&1 || true
kubectl -n "${NS}" run np-listener --restart=Never --image=busybox:1.36 \
  --labels="app.kubernetes.io/name=runkite,app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=runner" \
  --command -- nc -l -p 9999
kubectl -n "${NS}" wait --for=condition=Ready pod/np-listener --timeout=60s
LISTEN_IP="$(kubectl -n "${NS}" get pod np-listener -o jsonpath='{.status.podIP}')"
allow_log="$(probe_connect np-probe-allow)"
echo "    without NP: ${allow_log:-<empty>}"
kubectl -n "${NS}" delete pod np-listener --ignore-not-found --wait=false >/dev/null 2>&1 || true
if ! echo "${allow_log}" | grep -q 'EXIT:0'; then
  echo "error: probe should connect after deleting runner NetworkPolicy (got ${allow_log:-<empty>})" >&2
  exit 1
fi
echo "    runner inbound deny observed (A/B: deny≠0, allow=0)"

cat <<EOF

PASS — kind Helm Ingress + NetworkPolicy complete.

  cluster:   ${CLUSTER} (deleted on exit unless KEEP_CLUSTER=1)
  ingress:   Host: runkite.local → http://127.0.0.1:${HOST_HTTP_PORT}
  proof:     /readyz + echo_agent success via Ingress; runner NP deny-inbound

Non-claims: not Kubernetes Supported / EKS / soak; not Ingress TLS /
cert-manager / pod mTLS; not MCP sticky Ingress; not CP "from ingress
only"; not runner egress lockdown; not external gRPC.
EOF
