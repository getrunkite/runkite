#!/usr/bin/env bash
# Maintainer adversarial regression entrypoint. No registry or provider writes.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/test-identity-security.sh [--live] [--race-linux]

Runs local Go/SQLite, Python and TypeScript identity/security regressions.
Dependencies must already be installed. Go downloads are disabled.
This is maintainer qualification, not an independent assessment.

--live additionally requires ALL of:
  RUNKITE_TEST_IDENTITY_DISPOSABLE=1
  RUNKITE_TEST_IDENTITY_POSTGRES_DSN=postgres://.../runkite_identity_<suffix>
  RUNKITE_TEST_IDENTITY_REDIS_URL=redis://127.0.0.1:<port>/<database>
Both services must be disposable, dedicated and loopback-only. Tests may remove
test records. This script does not create or delete infrastructure for you.

--race-linux adds Go -race, only on a native Linux host with a C compiler.

Optional: RUNKITE_TEST_GO, RUNKITE_TEST_PYTHON, RUNKITE_TEST_NODE executable paths;
RUNKITE_TEST_IDENTITY_OUTPUT artifact parent directory; existing GOMODCACHE,
GOCACHE and GOPATH. Logs contain synthetic test data. No real credentials should
be supplied. A new child directory holds each run's log and result metadata.
USAGE
}

live=0
race=0
for option in "$@"; do
  case "$option" in
    --live) live=1 ;;
    --race-linux) race=1 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$option" >&2; usage >&2; exit 2 ;;
  esac
done

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
go_bin="${RUNKITE_TEST_GO:-go}"
python_bin="${RUNKITE_TEST_PYTHON:-python3}"
node_bin="${RUNKITE_TEST_NODE:-node}"
for executable in "$go_bin" "$python_bin" "$node_bin"; do
  if ! command -v "$executable" >/dev/null 2>&1; then
    printf 'Required executable is unavailable: %s\n' "$executable" >&2
    exit 2
  fi
done
if [[ ! -d "$root/typescript/runkite-runner/node_modules/tsx" ]]; then
  printf 'Install the locked TypeScript runner dependencies before qualification.\n' >&2
  exit 2
fi
if [[ "$race" == 1 && "$(uname -s)" != Linux ]]; then
  printf '%s\n' '--race-linux requires a native Linux test host.' >&2
  exit 2
fi
if [[ "$race" == 1 ]] && ! command -v cc >/dev/null 2>&1; then
  printf '%s\n' '--race-linux requires an installed C compiler.' >&2
  exit 2
fi

# Only explicit disposable endpoints reach live tests. Never consume ordinary
# POSTGRES_DSN/REDIS_URL or provider credentials from the caller's environment.
live_pg="${RUNKITE_TEST_IDENTITY_POSTGRES_DSN:-}"
live_redis="${RUNKITE_TEST_IDENTITY_REDIS_URL:-}"
if [[ "$live" == 1 ]]; then
  if [[ "${RUNKITE_TEST_IDENTITY_DISPOSABLE:-}" != 1 ]]; then
    printf '%s\n' '--live requires the explicit disposable-infrastructure acknowledgement.' >&2
    exit 2
  fi
  RUNKITE_TEST_IDENTITY_POSTGRES_DSN="$live_pg" RUNKITE_TEST_IDENTITY_REDIS_URL="$live_redis" "$python_bin" - <<'PY'
import os
import re
from urllib.parse import parse_qs, unquote, urlsplit

try:
    pg = urlsplit(os.environ["RUNKITE_TEST_IDENTITY_POSTGRES_DSN"])
    redis = urlsplit(os.environ["RUNKITE_TEST_IDENTITY_REDIS_URL"])
    hosts = {"127.0.0.1", "localhost", "::1"}
    valid = (
        pg.scheme in {"postgres", "postgresql"}
        and pg.hostname in hosts
        and pg.port is not None
        and re.fullmatch(r"/runkite_identity_[a-zA-Z0-9_]+", unquote(pg.path)) is not None
        and parse_qs(pg.query, keep_blank_values=True) in ({}, {"sslmode": ["disable"]})
        and not pg.fragment
        and redis.scheme == "redis"
        and redis.hostname in hosts
        and redis.port is not None
        and re.fullmatch(r"/(?:[0-9]|1[0-5])", redis.path) is not None
        and not redis.query and not redis.fragment
    )
except (KeyError, ValueError):
    valid = False
if not valid:
    raise SystemExit("Live qualification requires dedicated loopback URL endpoints, an explicit port, a runkite_identity_ Postgres database and Redis database 0..15.")
PY
fi

parent="${RUNKITE_TEST_IDENTITY_OUTPUT:-${TMPDIR:-/tmp}}"
mkdir -p "$parent"
output="$(mktemp -d "$parent/identity-security.XXXXXX")"
log="$output/qualification.log"
phase=initializing
started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
revision="$(git -C "$root" rev-parse HEAD)"
dirty=0
if [[ -n "$(git -C "$root" status --porcelain)" ]]; then dirty=1; fi
finish() {
  result=$?
  trap - EXIT
  if "$python_bin" - "$output/result.json" "$result" "$phase" "$started" "$revision" "$dirty" "$live" "$race" <<'PY'
import json
import platform
import sys
from datetime import datetime, timezone
from pathlib import Path

path, code, phase, started, revision, dirty, live, race = sys.argv[1:]
Path(path).write_text(json.dumps({
    "schema_version": 1,
    "qualification": "maintainer_adversarial_regression",
    "independent_assessment": "pending",
    "passed": int(code) == 0,
    "exit_code": int(code), "last_phase": phase,
    "started_at": started,
    "finished_at": datetime.now(timezone.utc).isoformat(),
    "revision": revision, "working_tree_dirty": dirty == "1",
    "live_postgres_redis": live == "1", "linux_race": race == "1",
    "platform": platform.platform(),
    "non_claims": ["independent review", "production soak", "live provider correctness", "OpenShell or CNI qualification", "all vulnerability or secret absence"],
}, indent=2) + "\n")
PY
  then
    :
  else
    report_result=$?
    printf 'Failed to persist qualification result metadata.\n' >&2
    if [[ "$result" == 0 ]]; then result=$report_result; fi
  fi
  printf 'Identity qualification artifacts: %s\n' "$output"
  exit "$result"
}
trap finish EXIT

run_env=(env -i "PATH=$PATH" "HOME=${HOME:?}" "TMPDIR=${TMPDIR:-/tmp}" "GOTOOLCHAIN=local" "GOPROXY=off" "GOSUMDB=off" "CGO_ENABLED=$race" "PYTHONPATH=$root/python")
for name in GOMODCACHE GOCACHE GOPATH; do
  value="${!name:-}"
  if [[ -n "$value" ]]; then run_env+=("$name=$value"); fi
done
go_args=(-count=1)
if [[ "$race" == 1 ]]; then go_args+=(-race); fi
run_logged() {
  printf 'Running phase: %s\n' "$phase" | tee -a "$log"
  "${run_env[@]}" "$@" 2>&1 | tee -a "$log"
}
cd "$root"
phase=tool_versions
run_logged "$go_bin" version
run_logged "$python_bin" --version
run_logged "$node_bin" --version
phase=go_identity_and_security
run_logged "$go_bin" test "${go_args[@]}" ./internal/auth ./internal/bridge ./internal/connector ./internal/credentials ./internal/api ./internal/state ./internal/state/sqlite ./internal/transport/inprocess
phase=go_workload_bootstrap_and_poison_recovery
run_logged "$go_bin" test "${go_args[@]}" ./cmd -run 'WorkloadIdentity|Poison'
phase=python_identity_usage_and_clients
for test in test_tls_utils.py test_heartbeat.py; do
  run_logged "$python_bin" "python/tests/$test"
done
for test in test_run_status.py test_runner_identity.py test_run_identity_headers.py test_checkpoint_identity.py test_isolated_usage.py test_isolated.py test_isolated_fixture.py; do
  run_logged "$python_bin" -m unittest discover -s python/tests -p "$test" -v
done
phase=offline_publication_recovery
run_logged "$python_bin" scripts/test_release_artifacts.py
phase=typescript_runner
cd "$root/typescript/runkite-runner"
run_logged "$node_bin" node_modules/typescript/bin/tsc -p tsconfig.json --noEmit
# node --import avoids the tsx CLI's unnecessary IPC server.
ts_tests=()
while IFS= read -r -d '' test_file; do ts_tests+=("$test_file"); done < <(find src -type f -name '*.test.ts' -print0)
if [[ "${#ts_tests[@]}" == 0 ]]; then printf 'No TypeScript runner tests found.\n' >&2; exit 2; fi
run_logged "$node_bin" --import tsx --test "${ts_tests[@]}"
cd "$root"
if [[ "$live" == 1 ]]; then
  run_env+=("POSTGRES_DSN=$live_pg" "REDIS_URL=$live_redis")
  # These suites intentionally share disposable storage. Run them sequentially
  # so one package's fixture reset cannot remove another package's active rows.
  phase=disposable_postgres_state
  run_logged "$go_bin" test "${go_args[@]}" ./internal/state/postgres
  phase=disposable_redis_queue
  run_logged "$go_bin" test "${go_args[@]}" ./internal/transport/redis
  phase=disposable_postgres_redis_api
  run_logged "$go_bin" test "${go_args[@]}" ./internal/api
fi
phase=complete
