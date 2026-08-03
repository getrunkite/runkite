# Runkite — development and test targets

# Auto-derives from the nearest git tag when one exists (e.g. "v1.0.0",
# or "v1.0.0-3-gabc1234" for commits past the last tag). With no tags
# at all, `--always` falls back to the short commit hash instead (e.g.
# "574b65b", or "574b65b-dirty" with uncommitted changes) -- NOT the
# literal string "dev"; that only happens if the `git` invocation itself
# fails (not a git repo, git not installed). Override explicitly with
# `make build VERSION=v1.0.0` if you want a specific value regardless of
# what HEAD currently resolves to.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildTime=$(BUILD_TIME)

.PHONY: build vet test test-all test-all-v test-pg test-mysql test-redis test-mongo test-qdrant test-weaviate test-pinecone test-nats test-kafka test-e2e test-matrix test-matrix-record test-protocol-fixtures test-protocol-execute test-llm-matrix test-llm-structural test-python test-ts test-adapters smoke-multi soak-multi multi-up multi-down up down dev-up dev-down logs infra-up infra-down proto-gen lint lint-go lint-python lint-ts fmt fmt-go fmt-python fmt-ts openapi openapi-check

# --- Build ---
build:
	go build -ldflags '$(LDFLAGS)' -o runkite ./cmd/

vet:
	go vet ./...

# --- Tests ---

# Default: SQLite + in-memory only (no external services needed).
# Explicitly unset backend env vars so inherited shell env doesn't leak.
test:
	POSTGRES_DSN= REDIS_URL= go test ./internal/... ./runner-protocol/tests/ -race -count=1

# Runner Protocol example fixtures (schema + lifecycle invariants).
# Does not execute a runner — see runner-protocol/tests/fixtures_test.go.
test-protocol-fixtures:
	go test ./runner-protocol/tests/ -count=1

# Runner Protocol execute goldens: real execute_run + deterministic mock
# agent, diffed against examples/*/expected_events (PROTOCOL.md §14.3).
test-protocol-execute:
	@if [ -x python/.venv/bin/python ]; then \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_protocol_execute_goldens.py; \
	else \
		PYTHONPATH=python python3 python/tests/test_protocol_execute_goldens.py; \
	fi

# Live Gemini N×N (requires .env.llm with GOOGLE_API_KEY; gitignored).
# Artifacts → bench/llm/out/ (also gitignored). See bench/llm/README.md.
test-llm-matrix:
	@test -f .env.llm || (echo "missing .env.llm — copy .env.llm.example and add GOOGLE_API_KEY"; exit 1)
	set -a && . ./.env.llm && set +a && python3 bench/llm/run_matrix.py

# Structural Runner Protocol invariants against a real Gemini agent
# (lifecycle/seq/terminal/tool signal — not exact expected_events).
# Skips cleanly if .env.llm is absent.
test-llm-structural:
	@if [ -f .env.llm ]; then set -a && . ./.env.llm && set +a; fi; \
	if [ -x python/.venv/bin/python ]; then \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_protocol_llm_structural.py; \
	else \
		PYTHONPATH=python python3 python/tests/test_protocol_llm_structural.py; \
	fi

# Postgres conformance (requires POSTGRES_DSN or infra-up)
test-pg:
	POSTGRES_DSN="postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable" \
		go test ./internal/state/postgres/ -race -count=1 -v

# MySQL conformance (requires MYSQL_DSN or infra-up). Same shared
# conformance.RunStoreSuite as Postgres/SQLite/Mongo -- see
# internal/state/mysql/mysql_test.go. Also exercises cmd's own
# initStore/db-upgrade/db-reset backend-selection wiring (cmd/
# store_selection_test.go), not just the internal/state/mysql package
# in isolation -- serve wiring and conformance testing are two
# separate things that can each be individually forgotten.
test-mysql:
	MYSQL_DSN="runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true" \
		go test ./internal/state/mysql/ ./cmd/ -race -count=1 -v

# Redis conformance (requires REDIS_URL or infra-up) -- transport triad,
# shared rate-limit Lua backend, and cmd reclaim-leader lock.
test-redis:
	REDIS_URL="redis://localhost:6380" \
		go test ./internal/transport/redis/ ./internal/ratelimit/ ./cmd/ -race -count=1 -v

# Mongo conformance (requires MONGO_URI or infra-up). Same shared
# conformance.RunStoreSuite as Postgres/SQLite -- see
# internal/state/mongo/mongo_test.go. Skips (not fails) if MONGO_URI is
# unset, same convention as test-pg's POSTGRES_DSN, so plain `make test`
# never requires Docker.
test-mongo:
	MONGO_URI="mongodb://localhost:27018/?replicaSet=rs0&directConnection=true" \
		go test ./internal/state/mongo/ -race -count=1 -v

# Qdrant vector store conformance (requires QDRANT_URL or infra-up)
test-qdrant:
	QDRANT_URL="http://localhost:6333" \
		go test ./internal/vectorstore/qdrant/ -race -count=1 -v

# Weaviate vector store conformance (requires WEAVIATE_URL or infra-up)
test-weaviate:
	WEAVIATE_URL="http://localhost:8080" \
		go test ./internal/vectorstore/weaviate/ -race -count=1 -v

# Pinecone vector store conformance, against Pinecone Local -- Pinecone's
# own official in-memory emulator, not a real paid account (requires
# PINECONE_URL or infra-up)
test-pinecone:
	PINECONE_URL="http://localhost:5080" \
		go test ./internal/vectorstore/pinecone/ -race -count=1 -v

# NATS transport conformance -- full JobQueue+EventBroker+CancelBroker
# triad (requires NATS_URL or infra-up)
test-nats:
	NATS_URL="nats://localhost:4222" \
		go test ./internal/transport/nats/ -race -count=1 -v

# Kafka transport conformance -- JobQueue only (see
# internal/transport/kafka's own package doc for why it doesn't
# implement the full triad the way Redis/NATS do). Confirmed live: this
# is meaningfully slower than every other backend's own conformance
# suite here (~3 minutes vs seconds) -- each of its ~15 sub-tests joins
# and leaves its own fresh, uniquely-namespaced Kafka consumer group
# (see kafka_test.go's own comment for why namespacing is required),
# and that join/leave protocol exchange itself, not this project's own
# code, is what's slow. Not a hang; just Kafka's own per-group-lifecycle
# overhead paid ~15 times in a row. (requires KAFKA_URL or infra-up)
test-kafka:
	KAFKA_URL="localhost:9092" \
		go test ./internal/transport/kafka/ -race -count=1 -v -timeout 400s

# All backends — SQLite + Postgres + Redis + MongoDB + Qdrant + Weaviate + Pinecone + NATS + Kafka (requires infra-up)
#
# -p 1 (serialize package test binaries, not just tests within one
# package) is required, not just a speed/safety tradeoff: both
# internal/api/vectors_test.go and internal/vectorstore/pgvector/
# pgvector_test.go DROP TABLE IF EXISTS vector_items against the same
# shared Postgres test database in their setup. Under Go's default
# parallel-package execution, one package's DROP races the other's
# CREATE/use → intermittent "relation does not exist". Confirmed via
# the DROP sites (not state/postgres TruncateAll, which never touches
# vector_items). -p 1 is the structural fix rather than papering over
# one flaky assertion; unique-per-package table names would also work.
test-all:
	POSTGRES_DSN="postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable" \
	MYSQL_DSN="runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true" \
	REDIS_URL="redis://localhost:6380" \
	MONGO_URI="mongodb://localhost:27018/?replicaSet=rs0&directConnection=true" \
	QDRANT_URL="http://localhost:6333" \
	WEAVIATE_URL="http://localhost:8080" \
	PINECONE_URL="http://localhost:5080" \
	NATS_URL="nats://localhost:4222" \
	KAFKA_URL="localhost:9092" \
		go test ./internal/... ./cmd/... -race -count=1 -p 1 -timeout 600s

# Verbose version of test-all
test-all-v:
	POSTGRES_DSN="postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable" \
	MYSQL_DSN="runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true" \
	REDIS_URL="redis://localhost:6380" \
	MONGO_URI="mongodb://localhost:27018/?replicaSet=rs0&directConnection=true" \
	QDRANT_URL="http://localhost:6333" \
	WEAVIATE_URL="http://localhost:8080" \
	PINECONE_URL="http://localhost:5080" \
	NATS_URL="nats://localhost:4222" \
	KAFKA_URL="localhost:9092" \
		go test ./internal/... ./cmd/... -race -count=1 -v -p 1 -timeout 600s

# End-to-end: builds the real binary, runs it + the real Python runner as
# subprocesses against real Postgres/Redis, and re-validates VG-001/002/003
# over plain HTTP/SSE. Requires infra-up. This is black-box -- it does not
# import any internal package -- specifically so it catches bugs that only
# exist in the fully-wired stack (e.g. middleware composition, Docker
# packaging) rather than in any single component tested in isolation.
# Tier-0 e2e only -- excludes ./test/e2e/matrix (gated by RUNKITE_RUN_MATRIX
# and `make test-matrix` / nightly Matrix workflow).
test-e2e:
	POSTGRES_DSN="postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable" \
	REDIS_URL="redis://localhost:6380" \
		go test ./test/e2e/ ./test/e2e/adapters/... -v -timeout 120s -count=1

# Cross-framework × backend test matrix: every framework runner
# (python-langgraph, typescript-langgraphjs, python-langchain/-crewai/
# -llamaindex/-autogen) against every backend combination
# (SQLite+in-process, Postgres+Redis, MySQL+in-process, Mongo+Redis),
# each running the scenarios that framework's example agents support,
# diffed against golden fixtures in test/e2e/matrix/golden/.
# ~32 real subprocess-pair start/stop cycles -- deliberately its own
# target, not folded into test-e2e's 120s budget. Nightly /
# workflow_dispatch in .github/workflows/matrix.yml; locally
# `make infra-up && make test-matrix` (needs python/.venv, adapter
# venvs, typescript/runkite-runner node_modules, and
# examples/echo_agent_ts node_modules). SQLite+in-process cells run
# without infra; other cells skip if their MATRIX_* / DSN env is unset.
#
# MATRIX_* defaults match docker-compose.test.yml host ports. CI
# overrides them to Actions service ports (5432/3306/6379/27017).
MATRIX_POSTGRES_DSN ?= postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable
MATRIX_MYSQL_DSN ?= runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true
MATRIX_REDIS_URL ?= redis://localhost:6380
MATRIX_MONGO_URI ?= mongodb://localhost:27018/?replicaSet=rs0&directConnection=true

test-matrix:
	@test -d examples/echo_agent_ts/node_modules || (cd examples/echo_agent_ts && npm ci)
	RUNKITE_RUN_MATRIX=1 \
	POSTGRES_DSN="$(MATRIX_POSTGRES_DSN)" \
	MYSQL_DSN="$(MATRIX_MYSQL_DSN)" \
	REDIS_URL="$(MATRIX_REDIS_URL)" \
	MONGO_URI="$(MATRIX_MONGO_URI)" \
		go test ./test/e2e/matrix/... -v -timeout 1200s -count=1

# Re-records every golden fixture in test/e2e/matrix/golden/ instead of
# diffing against them -- run after intentionally changing expected
# behavior, then review the resulting git diff like any other code
# change before committing the new fixtures.
test-matrix-record:
	RUNKITE_RUN_MATRIX=1 \
	RUNKITE_GOLDEN_RECORD=1 \
	POSTGRES_DSN="$(MATRIX_POSTGRES_DSN)" \
	MYSQL_DSN="$(MATRIX_MYSQL_DSN)" \
	REDIS_URL="$(MATRIX_REDIS_URL)" \
	MONGO_URI="$(MATRIX_MONGO_URI)" \
		go test ./test/e2e/matrix/... -v -timeout 1200s -count=1

# Python runner unit tests (namespace encoding / factory-graph
# classification / etc. always; dual-mode interop tests skip unless
# RUNKITE_HTTP_URL + POSTGRES_DSN point at a live stack; store-pool and
# checkpoint-concurrent-setup skip on POSTGRES_DSN alone).
test-python:
	@if [ -x python/.venv/bin/python ]; then \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_store_dual_mode.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_store_pool.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_store_batch_cross_loop.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_store_pool_recreate.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_checkpoint_pool_recreate.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_checkpoint_concurrent_setup.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_custom_app.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_custom_auth.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_factory_graph.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_vectorstore_cross_loop.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_vectorstore_dual_mode.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_generic_worker.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_langchain_adapter.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_worker_cancel_race.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_run_status.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_worker_concurrency.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_tool_call_hook.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_protocol_execute_goldens.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_a2a.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_heartbeat.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_logging_config.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_tls_utils.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_schema_introspect.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_tenant_ctx.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_tracing.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_otel_callbacks.py; \
	else \
		PYTHONPATH=python python3 python/tests/test_store_dual_mode.py && \
		PYTHONPATH=python python3 python/tests/test_store_pool.py && \
		PYTHONPATH=python python3 python/tests/test_store_batch_cross_loop.py && \
		PYTHONPATH=python python3 python/tests/test_store_pool_recreate.py && \
		PYTHONPATH=python python3 python/tests/test_checkpoint_pool_recreate.py && \
		PYTHONPATH=python python3 python/tests/test_checkpoint_concurrent_setup.py && \
		PYTHONPATH=python python3 python/tests/test_custom_app.py && \
		PYTHONPATH=python python3 python/tests/test_custom_auth.py && \
		PYTHONPATH=python python3 python/tests/test_factory_graph.py && \
		PYTHONPATH=python python3 python/tests/test_vectorstore_cross_loop.py && \
		PYTHONPATH=python python3 python/tests/test_vectorstore_dual_mode.py && \
		PYTHONPATH=python python3 python/tests/test_generic_worker.py && \
		PYTHONPATH=python python3 python/tests/test_langchain_adapter.py && \
		PYTHONPATH=python python3 python/tests/test_worker_cancel_race.py && \
		PYTHONPATH=python python3 python/tests/test_run_status.py && \
		PYTHONPATH=python python3 python/tests/test_worker_concurrency.py && \
		PYTHONPATH=python python3 python/tests/test_tool_call_hook.py && \
		PYTHONPATH=python python3 python/tests/test_protocol_execute_goldens.py && \
		PYTHONPATH=python python3 python/tests/test_a2a.py && \
		PYTHONPATH=python python3 python/tests/test_heartbeat.py && \
		PYTHONPATH=python python3 python/tests/test_logging_config.py && \
		PYTHONPATH=python python3 python/tests/test_tls_utils.py && \
		PYTHONPATH=python python3 python/tests/test_schema_introspect.py && \
		PYTHONPATH=python python3 python/tests/test_tenant_ctx.py && \
		PYTHONPATH=python python3 python/tests/test_tracing.py && \
		PYTHONPATH=python python3 python/tests/test_otel_callbacks.py; \
	fi

# CrewAI/LlamaIndex adapters each need their own isolated venv (heavy,
# framework-specific deps that must never pollute the shared
# runkite_runner venv other tests/examples depend on -- see each
# adapter's own README for the one-time `make adapter-<name>-venv`
# setup). Skips silently if a venv hasn't been created yet, same
# "optional infra, never blocks the default test run" convention as
# test-pg/test-redis.
test-adapters:
	@if [ -x python/adapters/crewai_adapter/.venv/bin/python ]; then \
		PYTHONPATH=python:python/adapters python/adapters/crewai_adapter/.venv/bin/python python/adapters/crewai_adapter/test_adapter.py; \
	else \
		echo "skip: python/adapters/crewai_adapter/.venv not set up (see python/adapters/crewai_adapter/README.md)"; \
	fi
	@if [ -x python/adapters/llamaindex_adapter/.venv/bin/python ]; then \
		PYTHONPATH=python:python/adapters python/adapters/llamaindex_adapter/.venv/bin/python python/adapters/llamaindex_adapter/test_adapter.py; \
	else \
		echo "skip: python/adapters/llamaindex_adapter/.venv not set up (see python/adapters/llamaindex_adapter/README.md)"; \
	fi
	@if [ -x python/adapters/autogen_adapter/.venv/bin/python ]; then \
		PYTHONPATH=python:python/adapters python/adapters/autogen_adapter/.venv/bin/python python/adapters/autogen_adapter/test_adapter.py; \
	else \
		echo "skip: python/adapters/autogen_adapter/.venv not set up (see python/adapters/autogen_adapter/README.md)"; \
	fi

# TypeScript runner unit tests (pure logic -- namespace encoding, event
# generation, cancel/interrupt handling -- no live control plane needed).
test-ts:
	cd typescript/runkite-runner && npm install --silent && npx tsx --test src/*.test.ts

# --- Lint / format ---
#
# One gate per language, run independently so a Python-only or
# Go-only change doesn't need every toolchain installed locally to
# check itself -- CI runs all of them together (see .github/workflows/
# ci.yml), this is the local, incremental version of the same checks.

# golangci-lint is not vendored/pinned in go.mod (it's a standalone
# tool, not a library dependency) -- `go install` it once yourself:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
lint-go:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	golangci-lint run ./...

fmt-go:
	gofmt -w $$(git ls-files '*.go' | grep -v -E '_pb\.go$$|_grpc\.pb\.go$$')

# ruff installed into python/.venv via python/requirements-dev.txt --
# see `make python-dev-deps` (or just `python/.venv/bin/pip install -r
# python/requirements-dev.txt` / `uv pip install ...` directly).
lint-python:
	python/.venv/bin/ruff check python/ python/adapters/
	python/.venv/bin/ruff format --check python/ python/adapters/

fmt-python:
	python/.venv/bin/ruff check --fix python/ python/adapters/
	python/.venv/bin/ruff format python/ python/adapters/

lint-ts:
	cd typescript/runkite-runner && npm run lint && npm run format:check

fmt-ts:
	cd typescript/runkite-runner && npm run format

lint: lint-go lint-python lint-ts

fmt: fmt-go fmt-python fmt-ts

# --- OpenAPI spec generation ---

openapi:
	python3 scripts/openapi/build.py

openapi-check:
	python3 scripts/openapi/check.py

# --- Protobuf codegen ---

# Regenerates python/runkite_runner/runner_pb2*.py from proto/runner.proto.
# Pinned to grpcio-tools 1.68.1 (bundles protobuf 5.29.x) deliberately, NOT
# whatever is newest at generation time: protobuf's generated code embeds a
# hard floor ("runtime cannot be older than gencode") -- generating with a
# too-new toolchain silently produces a package that fails to import for
# any consumer with an older-but-still-reasonably-current protobuf pinned
# (this exact failure was hit integrating with a real external project's
# venv on protobuf 6.33.6, which is not old). Uses a throwaway venv, not
# python/.venv, so this pin never leaks into the runner's own runtime
# dependencies. Regenerates a relative `from . import runner_pb2` in
# runner_pb2_grpc.py -- grpc_tools.protoc's default output uses an
# absolute import, which breaks package-relative imports; the sed fixes
# that up to match runkite_runner's actual package layout.
proto-gen:
	rm -rf /tmp/runkite-proto-gen
	python3 -m venv /tmp/runkite-proto-gen
	/tmp/runkite-proto-gen/bin/pip install --quiet "grpcio-tools==1.68.1"
	/tmp/runkite-proto-gen/bin/python -m grpc_tools.protoc \
		-I proto \
		--python_out=python/runkite_runner \
		--grpc_python_out=python/runkite_runner \
		proto/runner.proto
	sed -i '' 's/^import runner_pb2 as runner__pb2$$/from . import runner_pb2 as runner__pb2/' python/runkite_runner/runner_pb2_grpc.py
	rm -rf /tmp/runkite-proto-gen

# --- Docker (full stack) ---

up:
	docker compose up -d --build

down:
	docker compose down -v

# Dev mode (source-mounted, ephemeral storage)
dev-up:
	docker compose -f docker-compose.dev.yml up -d --build

dev-down:
	docker compose -f docker-compose.dev.yml down -v

logs:
	docker compose logs -f

# --- Test infrastructure ---

# Start ephemeral Postgres + Redis for tests
infra-up:
	docker compose -f docker-compose.test.yml up -d --wait

# Stop and remove test infrastructure
infra-down:
	docker compose -f docker-compose.test.yml down -v

# Multi-control-plane topology (3 CP replicas + nginx + 2 runners).
# Requires RUNNER_TOKEN in the environment or .env (see .env.example).
# smoke-multi leaves the stack up so you can open http://localhost:2026/admin/
multi-up:
	docker compose -f docker-compose.multi.yml up -d --build

multi-down:
	docker compose -f docker-compose.multi.yml down -v

smoke-multi:
	@bash scripts/smoke-multi.sh

# 30-minute laptop soak: 3 CP + 2 runners + webhooks/preflight + Py/JS clients.
# Override duration: SOAK_DURATION=120 make soak-multi
# Watch: http://localhost:2026/admin/
soak-multi:
	@bash bench/soak/run.sh
