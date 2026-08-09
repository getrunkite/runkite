#!/usr/bin/env bash
# Governance announce-bar smoke (security Phases 0–2 exit criteria).
#
# Boots Postgres (+ Redis) from docker-compose.test.yml when POSTGRES_DSN
# is unset, then runs TestGovernanceAnnounceBar:
#   Phase 0 — unbound store/session/vector → run_binding_required
#   Phase 1 — tenant B denied on connector Y + durable audit row
#   Phase 2 — Admin audit query + mandatory HITL approve one-shot
#
# Usage (from repo root):
#   make smoke-governance
#   POSTGRES_DSN=... bash scripts/smoke-governance.sh
#
# Leaves test infra up (tear down with: make infra-down).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

COMPOSE=(docker compose -f docker-compose.test.yml)
DEFAULT_DSN='postgres://runkite:runkite@127.0.0.1:5433/runkite_test?sslmode=disable'
STARTED_INFRA=0

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: missing prerequisite '$1'" >&2
    exit 1
  fi
}

need docker
need go

if [[ -z "${POSTGRES_DSN:-}" ]]; then
  echo "==> ensuring test Postgres + Redis"
  "${COMPOSE[@]}" up -d --wait postgres redis
  STARTED_INFRA=1
  export POSTGRES_DSN="${DEFAULT_DSN}"
fi

echo "==> Governance announce bar"
echo "    Phase 0: run-binding reject + assignment tenant"
echo "    Phase 1: tenant B connector deny + audit write"
echo "    Phase 2: Admin audit query + HITL approve one-shot"

set +e
go test ./internal/api/ -run TestGovernanceAnnounceBar -count=1 -timeout 120s
rc=$?
set -e

if (( rc != 0 )); then
  echo "error: TestGovernanceAnnounceBar failed (exit ${rc})" >&2
  exit "${rc}"
fi

cat <<EOF

PASS — governance announce bar (Phases 0–2).

  proof:     make smoke-governance / TestGovernanceAnnounceBar
  docs:      docs/trust-governance.md
  infra:     left up$(if (( STARTED_INFRA )); then echo " (started by this script; make infra-down to tear down)"; fi)

Non-claims: not Mongo governance parity; not in-graph / AuthorizeTool;
not paid EKS soak; not FinOps dashboards; not Vault secret_ref.
EOF
