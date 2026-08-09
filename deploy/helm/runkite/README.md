# Runkite Helm chart

Minimal Kubernetes packaging for a multi-replica Runkite control plane
(plus an optional Python runner), matching the topology exercised by
`docker-compose.multi.yml`.

## Prerequisites

- Kubernetes 1.25+
- Helm 3.8+
- **Postgres** and **Redis** reachable from the cluster (not installed by
  this chart). That pair is Runkite's **Supported** production profile
  (see [Backend support tiers](../../../README.md#backend-support-tiers)
  or [`docs/architecture.md`](../../../docs/architecture.md#backend-support-tiers)). Redis is required for
  multi-replica; without it the in-process transport only works for
  `replicaCount: 1`.
- Container images: defaults pull
  `ghcr.io/getrunkite/runkite:latest` and
  `ghcr.io/getrunkite/runkite-runner:latest` (published on release tags).
  Pin `image.tag` / `runner.image.tag` for production. For air-gapped/kind,
  build locally and override `image.repository` / `runner.image.repository`.

## Supported profile

[`values-supported.yaml`](values-supported.yaml) pins the announce /
**Supported** posture: multi-replica control plane, runner on, TLS off,
HPA off — Postgres + Redis supplied by you. Install:

```bash
kubectl create secret generic runkite-creds \
  --from-literal=POSTGRES_DSN='...' \
  --from-literal=REDIS_URL='...' \
  --from-literal=RUNNER_TOKEN_PYTHON_LANGGRAPH='...' \
  --from-literal=RUNNER_TOKEN='...' \
  --from-literal=RUNKITE_API_KEY='...'

helm upgrade --install runkite ./deploy/helm/runkite \
  -f deploy/helm/runkite/values.yaml \
  -f deploy/helm/runkite/values-supported.yaml \
  --set secrets.existingSecret=runkite-creds
```

For pod TLS / mTLS, also `-f deploy/helm/runkite/values-tls.yaml` (and create
the TLS Secrets named there, or override the names). See
[Pod TLS / mTLS](#pod-tls--mtls).

**Proof posture:** `make smoke-multi` / `make soak-multi` on
`docker-compose.multi.yml` is the Supported correctness path (see
[`bench/soak/WRITEUP.md`](../../../bench/soak/WRITEUP.md)). `make kind-helm-smoke`
proves this overlay installs on kind (`/readyz` + one `echo_agent` run);
`make kind-helm-rotate` walks dual-token runner rotation with rolling
restarts. Those are **install / rotation smokes**, not a soak. A green EKS
soak is **not** claimed — treat cluster installs as **Compatible** until then.

## Install

`serve` refuses to start without client-facing auth (same production
admission check as a bare binary). The chart's default
`config.langgraphJson` mounts a ConfigMap at `/etc/runkite/langgraph.json`
with `auth.type=api_key` and substitutes `${RUNKITE_API_KEY}` from the
Secret — no custom image rebuild required.

Same Supported overlay with inline secrets (prefer `existingSecret` above
for real deploys):

```bash
# Optional: build and load local images instead of GHCR
# docker build -t runkite:latest -f Dockerfile .
# docker build -t runkite-runner:latest -f Dockerfile.runner .

API_KEY="$(openssl rand -hex 32)"
RUNNER_TOKEN="$(openssl rand -hex 32)"

helm upgrade --install runkite ./deploy/helm/runkite \
  -f deploy/helm/runkite/values.yaml \
  -f deploy/helm/runkite/values-supported.yaml \
  --set secrets.postgresDsn='postgres://user:pass@postgres:5432/runkite?sslmode=disable' \
  --set secrets.redisUrl='redis://redis:6379' \
  --set secrets.runnerToken="$RUNNER_TOKEN" \
  --set secrets.apiKey="$API_KEY"

# Admin UI / API: Authorization: Bearer $API_KEY
```

### Custom graphs / auth

Override `config.langgraphJson` (Helm `--set-file` or a values file) with
your real agents. Keep secrets out of the JSON — use `${ENV_VAR}`
placeholders (see `internal/config` env expansion) and put the values in
the Secret / `config.extraEnv`.

To use a config already baked into the image instead of the mount, set
`config.langgraphJson=""` and point `config.langgraphConfig` at that
path. That file **must** include an `auth` section or the pod will
crash-loop on admission. Do **not** paper over this with
`RUNKITE_ALLOW_INSECURE_SERVE=1` in production.

## What this chart creates

| Resource | Purpose |
|---|---|
| Deployment (control plane) | `runkite serve`, N replicas, `/livez` + `/readyz` probes |
| Service | ClusterIP HTTP (`2026`) + gRPC (`50051`; `h2c` plaintext or `h2` when `tls.grpc.enabled`) |
| ConfigMap (env) | Ports + `LANGGRAPH_CONFIG` path |
| ConfigMap (`*-langgraph`) | Mounted `langgraph.json` (when `config.langgraphJson` is set) |
| Secret | DSNs, runner token, `RUNKITE_API_KEY` |
| PodDisruptionBudget | `minAvailable: 1` when `replicaCount > 1` |
| Deployment (runner) | Optional Python runner talking to the Service |
| Ingress / Ingress-mcp | Optional; MCP Ingress defaults to `upstream-hash-by: $remote_addr` |
| NetworkPolicy | Optional (`networkPolicy.enabled`); ingress lockdown for CP + deny-inbound runner |

## MCP sticky routing

`/mcp` session state is in-process. Round-robin across replicas yields
`404 session not found`. Enable `ingressMcp` (ingress-nginx) or stick
`/mcp` the same way `deploy/nginx-multi.conf` does (`hash $remote_addr consistent`)
at your external load balancer. Do **not** hash on `Mcp-Session-Id` — that
ID does not exist on the first `initialize` call.

## Values of note

| Key | Default | Notes |
|---|---|---|
| `replicaCount` | `2` | Use `1` only for SQLite / in-process demos |
| `config.langgraphJson` | echo_agent + api_key auth | Mounted; set `""` to use image-baked path |
| `secrets.apiKey` | required | Becomes `RUNKITE_API_KEY` for `${…}` expansion |
| `podDisruptionBudget.enabled` | `true` | Skipped when `replicaCount` is 1 |
| `terminationGracePeriodSeconds` | `30` | Covers the control plane's drain budget |
| `autoscaling.enabled` | `false` | Needs `resources.requests.cpu` if turned on |
| `runner.enabled` | `true` | Set `false` if runners run elsewhere |
| `secrets.runnerTokenAllowlist` | `""` | CP `RUNNER_TOKEN_*` allowlist; see [Runner token allowlist](#runner-token-allowlist) |
| `networkPolicy.enabled` | `false` | Opt-in; requires a NetworkPolicy-capable CNI |
| `tls.enabled` | `false` | Pod TLS/mTLS; see [Pod TLS / mTLS](#pod-tls--mtls) |
| `resources.requests` | `100m` / `256Mi` | Production-shaped defaults; override per env |

## Runner token allowlist

Control plane `RUNNER_TOKEN_<KIND>` accepts a **comma-separated allowlist**
of equally valid tokens (fleet credentials / rotation), not unique-per-pod
secrets (Deployments share one Secret across pods).

| Value | Effect |
|---|---|
| `secrets.runnerToken` | This chart’s runner Deployment `RUNNER_TOKEN` (one secret) |
| `secrets.runnerTokenAllowlist` | When set, CP `RUNNER_TOKEN_PYTHON_LANGGRAPH`; else CP uses `runnerToken` |

**Rotation:** set allowlist to `old,new` → restart CP → roll runners to
`new` → set allowlist (or `runnerToken`) to `new` → restart CP.

**Multi-fleet:** put both fleets’ tokens in the CP allowlist; run a second
runner Deployment (or `existingSecret`) with a different `RUNNER_TOKEN`.
Revoke one fleet by dropping its token from the allowlist and restarting CP.

With `helm --set` / `--set-string`, escape commas (`tok-a\,tok-b`) or pass a
values file — Helm treats unescaped commas as list separators.

## Pod TLS / mTLS

App-level TLS is **env-driven** ([`docs/auth.md`](../../../docs/auth.md)); this
chart only mounts Secrets and sets those vars. Ingress `tls:` is edge
termination only — it does **not** encrypt control-plane ↔ runner.

**Defaults when `tls.enabled: true`:** HTTP TLS + gRPC TLS + **gRPC mTLS**;
`http.mtls` stays **false** so kubelet `httpGet` probes (`scheme: HTTPS`)
keep working. Setting `tls.http.mtls: true` **fails** `helm template`/`install`
— kubelet cannot present a client cert on those probes, so health checks
would CrashLoop forever. (App env still supports HTTP mTLS outside this
chart if you wire probes yourself.)

Create Secrets (example with openssl; use cert-manager in real deploys).
Server cert SAN must include the Service DNS name the runner dials
(e.g. `runkite` / `runkite.<ns>.svc.cluster.local`):

```bash
# After creating ca.crt/key, server tls.crt/key, client tls.crt/key:
kubectl create secret tls runkite-tls-server --cert=server.crt --key=server.key
kubectl create secret generic runkite-tls-ca --from-file=ca.crt=ca.crt
kubectl create secret tls runkite-tls-client --cert=client.crt --key=client.key

helm upgrade --install runkite ./deploy/helm/runkite \
  --set secrets.existingSecret=runkite-creds \
  --set tls.enabled=true \
  --set tls.serverSecretName=runkite-tls-server \
  --set tls.caSecretName=runkite-tls-ca \
  --set tls.clientSecretName=runkite-tls-client
```

| Value | Maps to (CP) | Maps to (runner) |
|---|---|---|
| `tls.http.enabled` | `TLS_CERT_FILE` / `TLS_KEY_FILE` | `RUNKITE_HTTP_URL=https://…` |
| `tls.http.mtls` | `TLS_CLIENT_CA_FILE` | client cert/key (if any mTLS) |
| `tls.grpc.enabled` | `GRPC_TLS_CERT_FILE` / `GRPC_TLS_KEY_FILE` | (via CA file) |
| `tls.grpc.mtls` | `GRPC_TLS_CLIENT_CA_FILE` | `RUNKITE_TLS_CLIENT_*` |
| `tls.caSecretName` | CA mount | `RUNKITE_TLS_CA_FILE` |

Chart does not mint certificates. Missing `serverSecretName` / `caSecretName`
(or `clientSecretName` when any `*.mtls`) fails `helm template`/`install`
early.
