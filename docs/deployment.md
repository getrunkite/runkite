# Deployment

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md). For Supported install / reclaim / kill / secrets in one page, see the [Ops runbook](ops-runbook.md).

## Docker

Control-plane and runner images (`Dockerfile`, `Dockerfile.runner`, `Dockerfile.runner-ts`) run as a non-root user (`uid 65532`). The control-plane image sets `DATABASE_PATH=/tmp/runkite.db` so SQLite-backed compose profiles can create the DB file without root. Every HTTP response also carries security headers (CSP, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy`, `Permissions-Policy`) -- see Known Limitations for Admin credential storage, which is a separate concern.

### Full-stack deployment

`docker-compose.yml` runs five services: Postgres, Redis, the control plane, a Python runner and the pending-approval demo MCP server. Application images are built from `Dockerfile` and `Dockerfile.runner`:

```bash
cp .env.example .env   # fill in POSTGRES_PASSWORD and RUNNER_TOKEN with real values
docker compose up -d --build
curl http://localhost:2026/readyz
docker compose down
```

`POSTGRES_PASSWORD` and `RUNNER_TOKEN` are required, on purpose -- there's no built-in fallback, so `docker compose up` fails fast with a clear error if either is unset rather than silently starting with a well-known password or a shared "change-me-in-production" token nobody actually changes. Postgres and Redis also don't publish any port to the host in this file -- only the `runkite`/`runner` services on the same compose network need to reach them by service name.

Postgres data lives in the named `postgres_data` volume. Ordinary `docker compose down` retains it; adding `-v` deletes it. This example opts out of client-facing auth for a local demonstration. Configure real client auth and remove `RUNKITE_ALLOW_INSECURE_SERVE` before using it as a production deployment.

`docker-compose.dev.yml` runs three services: the control plane, Python runner and pending-approval demo MCP server. It uses SQLite and in-process transport, with no Postgres or Redis, and mounts `examples/` and `python/` from the source tree:

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

The dev SQLite file is `/tmp/runkite.db` in the control-plane container's writable layer. Restarting that same container retains it; recreating or removing the container loses it because this profile mounts no database volume. The full-stack Postgres and Redis ports are private to their Compose network, so local services on host ports 5432 and 6379 do not conflict. Both profiles publish host ports 2026 and 50051 and cannot use those same host bindings simultaneously.

### Kubernetes (Helm)

`deploy/helm/runkite` is a minimal chart for the same multi-replica shape as `docker-compose.multi.yml`: control-plane Deployment (N replicas, `/livez` + `/readyz` probes, PDB), Service (HTTP + gRPC), ConfigMap/Secret, optional Python runner, and an optional `/mcp` Ingress with client-IP consistent hashing. Postgres and Redis are **not** bundled — point `secrets.postgresDsn` / `secrets.redisUrl` (or `secrets.existingSecret`) at infrastructure you already run.

**Supported profile:** install with `-f values.yaml -f values-supported.yaml` (optional `-f values-tls.yaml` for pod TLS). See [`deploy/helm/runkite/README.md`](../deploy/helm/runkite/README.md#supported-profile). Compose `make smoke-multi` / `make soak-multi` is the Supported correctness proof ([`bench/soak/WRITEUP.md`](../bench/soak/WRITEUP.md)). Kind K0–K3 ops proofs: `make kind-helm-all` (or the individual `kind-helm-*` targets) — install, token rotation, mid-run reclaim, Ingress+NetworkPolicy ([`docs/k8s-kind-proof.md`](k8s-kind-proof.md)). Named EKS smoke/reclaim (K4) is recorded in [`k8s-eks-soak-results.md`](k8s-eks-soak-results.md) — packaging proven on a real cloud cluster; not a multi-hour multi-AZ HA soak. Compose remains the Supported multi-CP HA correctness proof.

`serve`'s production admission check still requires client-facing auth: the chart mounts a default `langgraph.json` (via ConfigMap) with `auth.type=api_key` and substitutes `${RUNKITE_API_KEY}` from `secrets.apiKey`, so you do not need to rebuild the image just to boot.

### Managed AI SDK runner (v0.5.8+)

The managed AI SDK backend is in the working tree and needs a newly built
TypeScript runner until its updated package/image is released. Register a
configuration with `runner_kind: "typescript-ai-sdk"` in the plane, and load the
matching file in the worker. Its `graphs` entries point to agent factories; the
code and its dependencies must be available in the worker image. Keep
`llm_cache` disabled for these agents. See
[the example](../examples/managed_ai_sdk/README.md) for the factory and app bridge.

For a local container connected to an already running plane:

```bash
docker build -t runkite-runner-ts:managed-local -f Dockerfile.runner-ts .
docker run --rm --network host \
  -e RUNNER_TOKEN \
  runkite-runner-ts:managed-local \
  --framework ai-sdk \
  --config /app/examples/managed_ai_sdk/langgraph.json \
  --grpc-address localhost:50051 \
  --http-address http://localhost:2026
```

This command uses host networking on Linux. For Docker Desktop, use your
configured host-networking support or replace the addresses with reachable
control-plane service addresses on a shared container network. Set
`RUNNER_TOKEN_TYPESCRIPT_AI_SDK` and `RUNNER_TENANTS_TYPESCRIPT_AI_SDK` on the
plane and present the matching `RUNNER_TOKEN` in the worker when runner auth is
enabled. Reuse the deployment's TLS settings.

The Helm chart's optional runner starts the Python image by default and exposes
no command/args override. Changing its image alone does not select the AI SDK
backend. Deploy an explicit TypeScript worker Deployment with the arguments
above, or supply an application image whose entrypoint selects them. Disable
`runner.enabled` if replacing the chart's Python fleet; keep it enabled if both
fleets are needed. The chart mounts `config.langgraphJson` only in control-plane
pods, so provide the worker's own matching config and agent code. See
[Helm runner selection](../deploy/helm/runkite/README.md#other-runner-kinds).

### Test infrastructure

`docker-compose.test.yml` starts ephemeral, tmpfs-backed Postgres + MySQL + Redis + MongoDB + Qdrant + Weaviate + [Pinecone Local](https://docs.pinecone.io/guides/operations/local-development) + NATS + Kafka for running the conformance test suite (see Development below), on non-standard ports (5433/3307/6380/27018/6333/8080/5080-5200) specifically to avoid colliding with the full-stack compose above or any local services -- NATS and Kafka are the exceptions, left on their standard ports (4222 / 9092) since nothing else in this project's compose files uses them:

```bash
docker compose -f docker-compose.test.yml up -d
make test-all
docker compose -f docker-compose.test.yml down -v
```

## Development

```bash
make test           # SQLite + in-memory only (no external deps)
make test-pg        # Postgres conformance (requires infra-up)
make test-mysql     # MySQL conformance + cmd's backend-selection wiring (requires infra-up)
make test-redis     # Redis conformance (requires infra-up)
make test-mongo     # MongoDB conformance (requires infra-up)
make test-kafka     # Kafka JobQueue conformance (requires infra-up; ~3min, see Kafka transport section for why)
make test-all       # All backends (requires infra-up)
make test-e2e       # Tier-0 black-box E2E (Python + LangChain adapter; requires infra-up)
make test-matrix    # Framework × backend golden matrix (~32 cells; nightly CI / workflow_dispatch)
make test-protocol-fixtures  # Runner Protocol examples/*.json schema + lifecycle invariants
make test-python    # Python runner unit tests
make test-ts        # TypeScript runner unit tests
make test-managed-ai-sdk  # Checkout AI SDK + UI bridge + real SQLite dispatch/recovery
make test-adapters  # CrewAI/LlamaIndex/AutoGen adapter unit tests (requires their isolated venvs, see python/adapters/*/README.md)
make infra-up       # Start ephemeral Postgres + MySQL + Redis + MongoDB + Qdrant via Docker
make infra-down     # Stop test infrastructure
make vet            # go vet
make build          # Build the binary

make lint           # gofmt/vet/golangci-lint + ruff + oxlint/prettier, all three SDKs
make fmt            # Auto-fix formatting for all three SDKs
make lint-go        # Just Go: gofmt -l + go vet + golangci-lint (go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)
make lint-python    # Just Python: ruff check + ruff format --check (python/.venv/bin/pip install -r python/requirements-dev.txt)
make lint-ts        # Just TypeScript: oxlint + prettier --check
```

PR CI (`.github/workflows/ci.yml`) runs unit/conformance/lint/Tier-0 e2e on every push. The framework × backend matrix (`.github/workflows/matrix.yml`) runs on a nightly schedule and `workflow_dispatch` -- same `make test-matrix` target, not folded into the PR budget. Same three-linter shape enforced in CI on every push/PR. Config lives in `.golangci.yml` (Go), `ruff.toml` (Python), and `typescript/runkite-runner/.oxlintrc.json` + `.prettierrc.json` (TypeScript) -- each a deliberately moderate starting rule set (golangci-lint's own curated "standard" linters, not "all"; ruff's `E`/`F`/`I`/`UP`/`B` rule groups) rather than maximally strict, so the gate catches real bugs (unused imports, unchecked errors on non-cleanup calls, suspicious constructs) without drowning contributors in day-one style nitpicks.
