# Deployment

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

## Docker

Control-plane and runner images (`Dockerfile`, `Dockerfile.runner`, `Dockerfile.runner-ts`) run as a non-root user (`uid 65532`). The control-plane image sets `DATABASE_PATH=/tmp/runkite.db` so SQLite-backed compose profiles can create the DB file without root. Every HTTP response also carries security headers (CSP, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy`, `Permissions-Policy`) -- see Known Limitations for Admin credential storage, which is a separate concern.

### Full-stack deployment

`docker-compose.yml` runs the whole system -- Postgres, Redis, the control plane, and a Python runner -- built from `Dockerfile` and `Dockerfile.runner`:

```bash
cp .env.example .env   # fill in POSTGRES_PASSWORD and RUNNER_TOKEN with real values
docker compose up -d --build
curl http://localhost:2026/health
docker compose down -v
```

`POSTGRES_PASSWORD` and `RUNNER_TOKEN` are required, on purpose -- there's no built-in fallback, so `docker compose up` fails fast with a clear error if either is unset rather than silently starting with a well-known password or a shared "change-me-in-production" token nobody actually changes. Postgres and Redis also don't publish any port to the host in this file -- only the `runkite`/`runner` services on the same compose network need to reach them by service name.

`docker-compose.dev.yml` is the zero-dependency local stack — control plane + Python runner only (SQLite + in-memory transport, no Postgres/Redis), with source-mounted `examples/` and `python/`:

```bash
docker compose -f docker-compose.dev.yml up -d --build
```

Full-stack compose uses Postgres/Redis on 5432/6379; stop local services on those ports first, or override via a compose override file.

### Kubernetes (Helm)

`deploy/helm/runkite` is a minimal chart for the same multi-replica shape as `docker-compose.multi.yml`: control-plane Deployment (N replicas, `/livez` + `/readyz` probes, PDB), Service (HTTP + gRPC), ConfigMap/Secret, optional Python runner, and an optional `/mcp` Ingress with client-IP consistent hashing. Postgres and Redis are **not** bundled — point `secrets.postgresDsn` / `secrets.redisUrl` (or `secrets.existingSecret`) at infrastructure you already run.

**Supported profile:** install with `-f values.yaml -f values-supported.yaml` (optional `-f values-tls.yaml` for pod TLS). See [`deploy/helm/runkite/README.md`](../deploy/helm/runkite/README.md#supported-profile). Compose `make smoke-multi` / `make soak-multi` is the Supported correctness proof ([`bench/soak/WRITEUP.md`](../bench/soak/WRITEUP.md)). `make kind-helm-smoke` / `make kind-helm-rotate` are kind install and runner-token rotation smokes for the same overlay (not a soak); Kubernetes/EKS remains Compatible until a cluster soak is published.

`serve`'s production admission check still requires client-facing auth: the chart mounts a default `langgraph.json` (via ConfigMap) with `auth.type=api_key` and substitutes `${RUNKITE_API_KEY}` from `secrets.apiKey`, so you do not need to rebuild the image just to boot.

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
