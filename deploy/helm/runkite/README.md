# Runkite Helm chart

Minimal Kubernetes packaging for a multi-replica Runkite control plane
(plus an optional Python runner), matching the topology exercised by
`docker-compose.multi.yml`.

## Prerequisites

- Kubernetes 1.25+
- Helm 3.8+
- **Postgres** and **Redis** reachable from the cluster (not installed by
  this chart). Redis is required for multi-replica; without it the
  in-process transport only works for `replicaCount: 1`.
- Container images built from this repo's `Dockerfile` and
  `Dockerfile.runner` (no public registry image is published yet).

## Install

```bash
# Build and load images (kind/minikube example)
docker build -t runkite:latest -f Dockerfile .
docker build -t runkite-runner:latest -f Dockerfile.runner .

helm upgrade --install runkite ./deploy/helm/runkite \
  --set secrets.postgresDsn='postgres://user:pass@postgres:5432/runkite?sslmode=disable' \
  --set secrets.redisUrl='redis://redis:6379' \
  --set secrets.runnerToken="$(openssl rand -hex 32)"
```

Prefer an existing Secret in real deployments:

```bash
kubectl create secret generic runkite-creds \
  --from-literal=POSTGRES_DSN='...' \
  --from-literal=REDIS_URL='...' \
  --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH='...' \
  --from-literal=RUNNER_TOKEN='...'   # same value; used by runner pods

helm upgrade --install runkite ./deploy/helm/runkite \
  --set secrets.existingSecret=runkite-creds \
  --set runner.enabled=true
```

## What this chart creates

| Resource | Purpose |
|---|---|
| Deployment (control plane) | `runkite serve`, N replicas, `/livez` + `/readyz` probes |
| Service | ClusterIP HTTP (`2026`) + gRPC (`50051`) |
| ConfigMap / Secret | Non-secret env + DSNs / runner token |
| PodDisruptionBudget | `minAvailable: 1` when `replicaCount > 1` |
| Deployment (runner) | Optional Python runner talking to the Service |
| Ingress / Ingress-mcp | Optional; MCP Ingress defaults to `upstream-hash-by: $remote_addr` |

## MCP sticky routing

`/mcp` session state is in-process. Round-robin across replicas yields
`404 session not found`. Enable `ingressMcp` (ingress-nginx) or stick
`/mcp` the same way `nginx-multi.conf` does (`hash $remote_addr consistent`)
at your external load balancer. Do **not** hash on `Mcp-Session-Id` — that
ID does not exist on the first `initialize` call.

## Values of note

| Key | Default | Notes |
|---|---|---|
| `replicaCount` | `2` | Use `1` only for SQLite / in-process demos |
| `podDisruptionBudget.enabled` | `true` | Skipped when `replicaCount` is 1 |
| `terminationGracePeriodSeconds` | `30` | Covers the control plane's drain budget |
| `runner.enabled` | `true` | Set `false` if runners run elsewhere |
