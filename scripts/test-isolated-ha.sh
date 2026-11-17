#!/usr/bin/env bash
# Own disposable PG/Redis and one external runtime. Never reuse application DBs.
set -euo pipefail
set +x

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
repetitions="${RUNKITE_TEST_ISOLATED_REPETITIONS:-1}"
if [[ ! "$repetitions" =~ ^([1-9]|1[0-9]|20)$ ]]; then
  echo "RUNKITE_TEST_ISOLATED_REPETITIONS must be from 1 to 20" >&2
  exit 2
fi
for required in docker python3 openssl uv; do
  command -v "$required" >/dev/null || { echo "Missing command: $required" >&2; exit 2; }
done
python_command="$(command -v "${RUNKITE_TEST_ISOLATED_PYTHON:-python3}")"
python_command="$(cd "$(dirname "$python_command")" && pwd)/$(basename "$python_command")"
export RUNKITE_TEST_ISOLATED_PYTHON="$python_command"
work_seconds="${RUNKITE_TEST_ISOLATED_HA_TIMEOUT_SECONDS:-1500}"
if [[ ! "$work_seconds" =~ ^[1-9][0-9]{0,3}$ || "$work_seconds" -gt 3600 ]]; then
  echo "RUNKITE_TEST_ISOLATED_HA_TIMEOUT_SECONDS must be from 1 to 3600" >&2
  exit 2
fi
for key in RUNKITE_TEST_BINARY RUNKITE_TEST_RUNNER_WHEEL RUNKITE_TEST_ISOLATED_CLI; do
  if [[ -z "${!key:-}" || ! -f "${!key}" ]]; then
    echo "$key must name the prepared local artifact" >&2
    exit 2
  fi
  resolved="$(cd "$(dirname "${!key}")" && pwd)/$(basename "${!key}")"
  printf -v "$key" '%s' "$resolved"
  export "${key?}"
done
if [[ ! -x "$RUNKITE_TEST_BINARY" || ! -x "$RUNKITE_TEST_ISOLATED_CLI" ]]; then
  echo "The prepared binary and runtime CLI must be executable" >&2
  exit 2
fi
docker_socket="$("$python_command" -I - "$repo_root/scripts/start-isolated-fixture.py" "${RUNKITE_TEST_DOCKER_SOCKET:-/var/run/docker.sock}" <<'PY'
import runpy
import sys

try:
    _, selected = runpy.run_path(sys.argv[1])["local_docker_command"](sys.argv[2])
except ValueError as error:
    print(str(error), file=sys.stderr)
    raise SystemExit(2) from error
print(selected)
PY
)"
docker=(docker --host "unix://$docker_socket")

scratch="$(mktemp -d "${TMPDIR:-/tmp}/runkite-isolated-ha.XXXXXX")"
if [[ -n "${RUNKITE_TEST_ISOLATED_HA_OUTPUT:-}" ]]; then
  mkdir -p "$RUNKITE_TEST_ISOLATED_HA_OUTPUT"
  output_base="$(cd "$RUNKITE_TEST_ISOLATED_HA_OUTPUT" && pwd)"
  output="$(mktemp -d "$output_base/run.XXXXXX")"
else
  output="$(mktemp -d "${TMPDIR:-/tmp}/runkite-isolated-ha-output.XXXXXX")"
fi
suffix="$(basename "$scratch" | tr -cd '[:alnum:]' | tr '[:upper:]' '[:lower:]')"
project="runkite-isolated-ha-$$-$suffix"
database="runkite_isolated_ha_$$_$suffix"
override="$scratch/override.yml"
compose=("${docker[@]}" compose --env-file /dev/null --project-name "$project" -f "$repo_root/docker-compose.test.yml" -f "$override")
started=0
fixture_pid=""
run_owned_exec=0
run_owned_reserve=0
phase_deadline="$("$python_command" -I -c 'import sys,time; print(time.monotonic()+int(sys.argv[1]))' "$work_seconds")"
input_args=()
for input_name in RUNKITE_ISOLATED_HA_PG_PASSWORD RUNKITE_TEST_BINARY RUNKITE_TEST_RUNNER_WHEEL RUNKITE_TEST_ISOLATED_CLI RUNKITE_TEST_ISOLATED_PYTHON RUNKITE_TEST_DOCKER_SOCKET RUNKITE_TEST_DOCKER_COMPOSE_PLUGIN RUNKITE_TEST_DOCKER_BUILDX_PLUGIN RUNKITE_TEST_ISOLATED RUNKITE_TEST_ISOLATED_HA RUNKITE_TEST_ISOLATED_POSTGRES_DSN RUNKITE_TEST_ISOLATED_REDIS_URL RUNKITE_TEST_ISOLATED_REPETITIONS; do
  input_args+=(--input "$input_name")
done

# One work deadline is shared by all preparation and native execution. Cleanup
# has its own 120-second deadline so work exhaustion cannot suppress teardown.
run_owned() {
  local log="$1" limit="$2" grace="$3" remaining
  shift 3
  remaining="$("$python_command" -I - "$phase_deadline" "$limit" "$run_owned_reserve" <<'CLOCK'
import sys
import time
remaining = min(float(sys.argv[2]), float(sys.argv[1]) - time.monotonic() - float(sys.argv[3]))
if remaining <= 0:
    raise SystemExit(124)
print(remaining)
CLOCK
)" || return 124
  local invocation=("$python_command" -I "$repo_root/scripts/review-ha-supervisor.py"
    --timeout "$remaining" --timeout-grace "$grace" --log "$log" --
    "$python_command" -I "$repo_root/scripts/isolated_fixture_environment.py"
    --directory "$scratch/environment" "${input_args[@]}" -- "$@")
  # The background fixture PID must be the supervisor, not a waiting shell.
  if [[ "${run_owned_exec:-0}" == 1 ]]; then exec "${invocation[@]}"; fi
  "${invocation[@]}"
}

cleanup() {
  status=$?
  trap - EXIT INT TERM
  phase_deadline="$("$python_command" -I -c 'import time; print(time.monotonic()+120)')"
  if [[ -n "$fixture_pid" ]]; then
    kill -TERM "$fixture_pid" 2>/dev/null || true
    wait "$fixture_pid" 2>/dev/null || true
  fi
  backend_confirmed=0
  if [[ "$started" == 1 ]]; then
    # Optional logs must leave time for DOWN and the disappearance check.
    run_owned_reserve=75 run_owned "$output/backend.log" 10 2 "${compose[@]}" logs --no-color postgres redis >/dev/null 2>&1 || true
    if run_owned "$output/backend-cleanup.log" 90 2 "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1; then
      if remaining_containers="$(run_owned "$output/backend-remaining.log" 10 2 "${compose[@]}" ps --all --quiet)" && [[ -z "$remaining_containers" ]]; then
        backend_confirmed=1
      fi
    fi
    if [[ "$backend_confirmed" != 1 ]]; then
      echo "Owned backend cleanup unconfirmed. Private recovery directory: $scratch" >&2
      if [[ "$status" == 0 ]]; then status=1; fi
    fi
  fi
  "$python_command" -I - "$output/backend-cleanup.json" "$backend_confirmed" <<'REPORT'
import json
import os
import sys
with os.fdopen(os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
    confirmed = sys.argv[2] == '1'
    json.dump({'confirmed': confirmed, 'reason_codes': [] if confirmed else ['backend_removal_unconfirmed']}, handle)
    handle.write('\n')
REPORT
  if [[ "$status" == 0 ]]; then
    if ! "$python_command" -I "$repo_root/scripts/isolated_ha_evidence.py" \
      --runtime-dir "$output/runtime" --repetitions "$repetitions" \
      --artifacts "$output/artifacts.json" --backend-cleanup "$output/backend-cleanup.json" \
      --output "$output/evidence-summary.json"; then status=1; fi
  fi
  unset RUNKITE_ISOLATED_HA_PG_PASSWORD postgres_url
  if [[ "$started" == 0 || "$backend_confirmed" == 1 ]]; then
    "$python_command" -I - "$scratch" <<'REMOVE'
import shutil
import sys
shutil.rmtree(sys.argv[1])
REMOVE
  fi
  echo "Isolated HA qualification evidence: $output"
  exit "$status"
}
trap 'cleanup' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

RUNKITE_ISOLATED_HA_PG_PASSWORD="$("$python_command" -I -c 'import secrets; print(secrets.token_urlsafe(32))')"
export RUNKITE_ISOLATED_HA_PG_PASSWORD
cat >"$override" <<EOF
services:
  postgres:
    ports: !override
      - target: 5432
        published: "0"
        host_ip: 127.0.0.1
        protocol: tcp
    environment:
      POSTGRES_DB: $database
      POSTGRES_PASSWORD: "\${RUNKITE_ISOLATED_HA_PG_PASSWORD:?owned fixture password required}"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U runkite -d $database"]
  redis:
    ports: !override
      - target: 6379
        published: "0"
        host_ip: 127.0.0.1
        protocol: tcp
EOF
run_owned "$output/backend-version.log" 15 2 "${docker[@]}" compose version >/dev/null
run_owned "$output/artifacts.log" 30 2 "$python_command" -I - "$repo_root/scripts" "$output/artifacts.json" "$RUNKITE_TEST_BINARY" "$RUNKITE_TEST_RUNNER_WHEEL" "$RUNKITE_TEST_ISOLATED_CLI" "$repo_root" <<'ARTIFACTS'
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from isolated_ha_evidence import record_artifacts
record_artifacts(*(Path(value) for value in sys.argv[2:]))
ARTIFACTS
run_owned "$output/backend-config.log" 30 2 "${compose[@]}" config --quiet >/dev/null
started=1
run_owned "$output/backend-startup.log" 120 2 "${compose[@]}" up --detach --wait --wait-timeout 90 --no-deps postgres redis >/dev/null
postgres_address="$(run_owned "$output/backend-postgres-port.log" 15 2 "${compose[@]}" port postgres 5432)"
redis_address="$(run_owned "$output/backend-redis-port.log" 15 2 "${compose[@]}" port redis 6379)"
if [[ ! "$postgres_address" =~ ^127\.0\.0\.1:([0-9]+)$ ]]; then
  echo "Unexpected owned Postgres mapping" >&2
  exit 1
fi
postgres_port="${BASH_REMATCH[1]}"
if [[ ! "$redis_address" =~ ^127\.0\.0\.1:([0-9]+)$ ]]; then
  echo "Unexpected owned Redis mapping" >&2
  exit 1
fi
redis_port="${BASH_REMATCH[1]}"
postgres_url="$(RUNKITE_ISOLATED_HA_PORT="$postgres_port" RUNKITE_ISOLATED_HA_DATABASE="$database" "$python_command" -I - <<'PY'
import os
from urllib.parse import quote, urlunsplit
authority = quote("runkite", safe="") + ":" + quote(os.environ["RUNKITE_ISOLATED_HA_PG_PASSWORD"], safe="")
authority += "@127.0.0.1:" + os.environ["RUNKITE_ISOLATED_HA_PORT"]
print(urlunsplit(("postgres", authority, "/" + quote(os.environ["RUNKITE_ISOLATED_HA_DATABASE"], safe=""), "sslmode=disable", "")))
PY
)"
export RUNKITE_TEST_ISOLATED=1
export RUNKITE_TEST_ISOLATED_HA=1
export RUNKITE_TEST_ISOLATED_POSTGRES_DSN="$postgres_url"
export RUNKITE_TEST_ISOLATED_REDIS_URL="redis://127.0.0.1:$redis_port/0"
export RUNKITE_TEST_ISOLATED_REPETITIONS="$repetitions"
run_owned_exec=1 run_owned "$output/fixture-supervisor.log" "$work_seconds" 110 \
  "$python_command" -I scripts/start-isolated-fixture.py \
    --cli "$RUNKITE_TEST_ISOLATED_CLI" --output-dir "$output/runtime" \
    --docker-socket "$docker_socket" --tls -- \
    "$python_command" -I -m unittest discover -s test/e2e/isolated -p test_managed.py -v &
fixture_pid=$!
test_status=0
wait "$fixture_pid" || test_status=$?
fixture_pid=""
exit "$test_status"
