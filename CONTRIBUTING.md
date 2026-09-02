# Contributing to Runkite

Runkite is a Go control plane implementing the Agent Protocol spec, with Python and TypeScript Runner Protocol SDKs. Contributions are welcome -- especially new state/vector-store backends, new runner languages, and new framework adapters, all of which the architecture was explicitly designed to support without touching the control plane's core.

## Before you start

- **License**: Runkite is licensed under [BUSL 1.1](LICENSE) (converts to Apache 2.0 on 2030-07-27). By submitting a contribution, you agree it's licensed under the same terms as the rest of the project.
- **Small, focused PRs** are much easier to review than large ones. If you're planning a big change (a new backend, a new runner language), consider opening an issue first to discuss the approach.
- **Read the [README](README.md) and [`docs/`](docs/README.md) first.** Deep design rationale, dual-mode notes, and known limitations live in `docs/` (especially [`architecture.md`](docs/architecture.md), [`runners.md`](docs/runners.md), [`limitations.md`](docs/limitations.md)) -- most "why does it work this way?" questions are already answered there.

## Repository layout

Root holds product entrypoints and Docker build context on purpose — do not
bury `Dockerfile*` / `docker-compose*.yml` under `deploy/` (compose
`build.context: .` expects them here).

| Path | Role |
|------|------|
| `README.md`, `Makefile`, `go.mod` | Project entry |
| `Dockerfile*`, `docker-compose*.yml` | Image/build context (stay at root) |
| `deploy/` | Helm chart + nginx/NATS sidecar configs |
| `cmd/`, `internal/` | Control plane |
| `python/`, `typescript/`, `admin-ui/` | Runners + Admin UI |
| `docs/`, `examples/`, `spec/`, `runner-protocol/` | Docs, samples, contracts ([public protocol mirror](https://github.com/getrunkite/runner-protocol)) |
| `test/`, `tests/`, `bench/` | E2E / SDK / soak harnesses |

## Development setup

```bash
git clone https://github.com/getrunkite/runkite
cd runkite
make build
```

See [`docs/quickstart.md`](docs/quickstart.md) for a full walkthrough (writing an agent, running the control plane + a runner, auth, connectors). Prerequisites: Go 1.25+, Python 3.12+ (for the Python runner/adapters), Node.js (for the TypeScript runner/Admin UI), Docker (optional, for Postgres/MySQL/Redis/MongoDB/Qdrant).

## Running tests

```bash
make test           # SQLite + in-memory only -- no external services needed, run this first
make vet             # go vet

# Backend-specific conformance suites (require `make infra-up` first):
make infra-up        # starts ephemeral Postgres + MySQL + Redis + MongoDB + Qdrant via Docker
make test-pg
make test-mysql
make test-redis
make test-mongo
make test-qdrant
make test-all        # everything above, in one run
make test-e2e        # black-box: real binary + real runner + Postgres/Redis
make smoke-governance # governance announce bar on Postgres (run-bind / deny+audit / HITL)
make infra-down

# Kind Helm packaging smokes (K8s Compatible — not a cloud soak):
make kind-helm-smoke kind-helm-rotate kind-helm-reclaim kind-helm-net

# Framework × backend goldens (also nightly CI):
make test-matrix

# Runner/adapter tests:
make test-python     # Python runner unit tests
make test-ts         # TypeScript runner unit tests
make test-adapters   # CrewAI/LlamaIndex/AutoGen adapters (each needs its own isolated venv -- see python/adapters/*/README.md)
```

CI (`.github/workflows/ci.yml`) runs the full matrix on every PR. A PR that only touches one language/backend doesn't need every target run locally -- CI will catch regressions elsewhere -- but do run whatever's directly relevant to your change, including `-race` where the Makefile already uses it.

## Adding a new state backend

`internal/state/store.go` defines the `Store` interface every backend implements (agents, threads, runs, checkpoints, store items, webhook dead-letters, run cache, cron schedules, registry). `internal/state/mongo` is the reference example for a from-scratch, non-SQL-derived implementation.

1. Implement `state.Store` in a new `internal/state/<backend>/` package.
2. Wire it into `internal/state/conformance`'s shared test suite (`RunStoreSuite`) -- this is the actual acceptance bar: a backend isn't "done" until it passes the identical suite Postgres/SQLite/MySQL/MongoDB pass, not a bespoke, weaker test file.
3. Wire backend selection into `cmd/serve.go`'s `initStore` and `cmd/db.go`'s `upgrade`/`reset` commands, following the existing `POSTGRES_DSN`/`MYSQL_DSN`/`MONGO_URI` env-var precedence pattern.
4. Add a service to `docker-compose.test.yml` and a `test-<backend>` Makefile target, and wire it into CI.

Same pattern for a new vector store backend (`internal/vectorstore/vectorstore.go`'s interface, `internal/vectorstore/conformance` for the shared suite) -- `internal/vectorstore/qdrant` is the reference example there.

## Adding a new runner language

The Runner Protocol (gRPC, `proto/runner.proto`) is the only contract a new runner needs to implement -- the control plane has zero language-specific code. `typescript/runkite-runner` is the reference example of a from-scratch runner in a second language; `python/runkite_runner` is the original.

At minimum, a new runner needs: `GetJob`/`ReportStatus`/`StreamEvents`/`WatchCancels` gRPC calls, the `RunAssignment`/`RunEvent` JSON shapes (see [`runner-protocol/PROTOCOL.md`](runner-protocol/PROTOCOL.md), the [public protocol mirror](https://github.com/getrunkite/runner-protocol), and [`docs/runners.md`](docs/runners.md)), and either direct-mode or proxy-mode checkpoint/store access (proxy mode -- calling the control plane's own HTTP API -- is the simpler starting point; see Checkpoint/Store dual mode in [`docs/architecture.md`](docs/architecture.md)). Spec discussion issues may be filed on the protocol mirror; patches land in this monorepo under `runner-protocol/` (the mirror is force-published from here).

## Adding a new framework adapter (Python)

`python/runkite_runner/generic_worker.py` is a framework-agnostic poll/dispatch loop shared by the CrewAI/LlamaIndex/AutoGen/plain-LangChain adapters (`python/adapters/*/`) -- each adapter is a thin translation layer implementing exactly two methods, `load_config` and `execute`. Use one of the existing adapters as a template.

## Code style

- Go: standard `gofmt`, `go vet` clean. No linter config beyond that today.
- Comments should explain *why*, not narrate *what* the code already says -- this codebase leans heavily on comments documenting the reasoning behind a non-obvious decision, a trade-off, or a bug that was actually found and fixed, not restating adjacent code in prose.
- Match existing patterns in the file/package you're editing before introducing a new one.
- New tests should be real regression tests (they fail without the fix, pass with it), not just "does it run without crashing."

## Reporting a bug or security issue

- **Regular bugs**: open a GitHub issue with steps to reproduce.
- **Security vulnerabilities**: see [`SECURITY.md`](SECURITY.md) -- please don't open a public issue for anything exploitable.
