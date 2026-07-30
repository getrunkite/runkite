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

.PHONY: build vet test test-all test-all-v test-pg test-mysql test-redis test-mongo test-e2e test-python test-ts test-adapters up down dev-up dev-down logs infra-up infra-down proto-gen

# --- Build ---
build:
	go build -ldflags '$(LDFLAGS)' -o runkite ./cmd/

vet:
	go vet ./...

# --- Tests ---

# Default: SQLite + in-memory only (no external services needed).
# Explicitly unset backend env vars so inherited shell env doesn't leak.
test:
	POSTGRES_DSN= REDIS_URL= go test ./internal/... -race -count=1

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

# Redis conformance (requires REDIS_URL or infra-up)
test-redis:
	REDIS_URL="redis://localhost:6380" \
		go test ./internal/transport/redis/ -race -count=1 -v

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

# All backends — SQLite + Postgres + Redis + MongoDB + Qdrant (requires infra-up)
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
		go test ./internal/... ./cmd/... -race -count=1 -p 1

# Verbose version of test-all
test-all-v:
	POSTGRES_DSN="postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable" \
	MYSQL_DSN="runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true" \
	REDIS_URL="redis://localhost:6380" \
	MONGO_URI="mongodb://localhost:27018/?replicaSet=rs0&directConnection=true" \
	QDRANT_URL="http://localhost:6333" \
		go test ./internal/... ./cmd/... -race -count=1 -v -p 1

# End-to-end: builds the real binary, runs it + the real Python runner as
# subprocesses against real Postgres/Redis, and re-validates VG-001/002/003
# over plain HTTP/SSE. Requires infra-up. This is black-box -- it does not
# import any internal package -- specifically so it catches bugs that only
# exist in the fully-wired stack (e.g. middleware composition, Docker
# packaging) rather than in any single component tested in isolation.
test-e2e:
	POSTGRES_DSN="postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable" \
	REDIS_URL="redis://localhost:6380" \
		go test ./test/e2e/... -v -timeout 120s -count=1

# Python runner unit tests (namespace encoding / factory-graph
# classification / etc. always; dual-mode interop tests skip unless
# RUNKITE_HTTP_URL + POSTGRES_DSN point at a live stack; store-pool and
# checkpoint-concurrent-setup skip on POSTGRES_DSN alone).
test-python:
	@if [ -x python/.venv/bin/python ]; then \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_store_dual_mode.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_store_pool.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_checkpoint_concurrent_setup.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_custom_app.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_factory_graph.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_vectorstore_dual_mode.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_generic_worker.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_langchain_adapter.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_worker_cancel_race.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_worker_concurrency.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_tool_call_hook.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_a2a.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_heartbeat.py && \
		PYTHONPATH=python python/.venv/bin/python python/tests/test_logging_config.py; \
	else \
		PYTHONPATH=python python3 python/tests/test_store_dual_mode.py && \
		PYTHONPATH=python python3 python/tests/test_store_pool.py && \
		PYTHONPATH=python python3 python/tests/test_checkpoint_concurrent_setup.py && \
		PYTHONPATH=python python3 python/tests/test_custom_app.py && \
		PYTHONPATH=python python3 python/tests/test_factory_graph.py && \
		PYTHONPATH=python python3 python/tests/test_vectorstore_dual_mode.py && \
		PYTHONPATH=python python3 python/tests/test_generic_worker.py && \
		PYTHONPATH=python python3 python/tests/test_langchain_adapter.py && \
		PYTHONPATH=python python3 python/tests/test_worker_cancel_race.py && \
		PYTHONPATH=python python3 python/tests/test_worker_concurrency.py && \
		PYTHONPATH=python python3 python/tests/test_tool_call_hook.py && \
		PYTHONPATH=python python3 python/tests/test_a2a.py && \
		PYTHONPATH=python python3 python/tests/test_heartbeat.py && \
		PYTHONPATH=python python3 python/tests/test_logging_config.py; \
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
