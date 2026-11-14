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
for key in RUNKITE_TEST_BINARY RUNKITE_TEST_RUNNER_WHEEL RUNKITE_TEST_ISOLATED_CLI; do
  if [[ -z "${!key:-}" || ! -f "${!key}" ]]; then
    echo "$key must name the prepared local artifact" >&2
    exit 2
  fi
  resolved="$(cd "$(dirname "${!key}")" && pwd)/$(basename "${!key}")"
  printf -v "$key" '%s' "$resolved"
  export "$key"
done
if [[ ! -x "$RUNKITE_TEST_BINARY" || ! -x "$RUNKITE_TEST_ISOLATED_CLI" ]]; then
  echo "The prepared binary and runtime CLI must be executable" >&2
  exit 2
fi
docker compose version >/dev/null

scratch="$(mktemp -d "${TMPDIR:-/tmp}/runkite-isolated-ha.XXXXXX")"
if [[ -n "${RUNKITE_TEST_ISOLATED_HA_OUTPUT:-}" ]]; then
  mkdir -p "$RUNKITE_TEST_ISOLATED_HA_OUTPUT"
  output="$(cd "$RUNKITE_TEST_ISOLATED_HA_OUTPUT" && pwd)"
else
  output="$(mktemp -d "${TMPDIR:-/tmp}/runkite-isolated-ha-output.XXXXXX")"
fi
suffix="$(basename "$scratch" | tr -cd '[:alnum:]' | tr '[:upper:]' '[:lower:]')"
project="runkite-isolated-ha-$$-$suffix"
database="runkite_isolated_ha_$$_$suffix"
override="$scratch/override.yml"
compose=(docker compose --env-file /dev/null --project-name "$project" -f "$repo_root/docker-compose.test.yml" -f "$override")
started=0
fixture_pid=""

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n "$fixture_pid" ]]; then
    # The runtime fixture owns its test supervisor and bounded cleanup. Let it
    # finish before retiring the databases its child planes were using.
    kill -TERM "$fixture_pid" 2>/dev/null || true
    wait "$fixture_pid" 2>/dev/null || true
  fi
  if [[ "$started" == 1 ]]; then
    "${compose[@]}" logs --no-color postgres redis >"$output/backend.log" 2>&1 || true
    if ! "${compose[@]}" down --volumes --remove-orphans >"$output/backend-cleanup.log" 2>&1; then
      echo "Owned backend cleanup unconfirmed: $output/backend-cleanup.log" >&2
      if [[ "$status" == 0 ]]; then status=1; fi
    fi
  fi
  unset RUNKITE_ISOLATED_HA_PG_PASSWORD postgres_url
  rm -rf "$scratch"
  echo "Isolated HA qualification evidence: $output"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

RUNKITE_ISOLATED_HA_PG_PASSWORD="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
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
"${compose[@]}" config --quiet >"$output/backend-config.log" 2>&1
started=1
"${compose[@]}" up --detach --wait --wait-timeout 90 --no-deps postgres redis >"$output/backend-startup.log" 2>&1
postgres_address="$("${compose[@]}" port postgres 5432)"
redis_address="$("${compose[@]}" port redis 6379)"
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
postgres_url="$(RUNKITE_ISOLATED_HA_PORT="$postgres_port" RUNKITE_ISOLATED_HA_DATABASE="$database" python3 - <<'PY'
import os
from urllib.parse import quote, urlunsplit
authority = quote("runkite", safe="") + ":" + quote(os.environ["RUNKITE_ISOLATED_HA_PG_PASSWORD"], safe="")
authority += "@127.0.0.1:" + os.environ["RUNKITE_ISOLATED_HA_PORT"]
print(urlunsplit(("postgres", authority, "/" + quote(os.environ["RUNKITE_ISOLATED_HA_DATABASE"], safe=""), "sslmode=disable", "")))
PY
)"
python_command="${RUNKITE_TEST_ISOLATED_PYTHON:-python3}"
RUNKITE_TEST_ISOLATED=1 \
RUNKITE_TEST_ISOLATED_HA=1 \
RUNKITE_TEST_ISOLATED_POSTGRES_DSN="$postgres_url" \
RUNKITE_TEST_ISOLATED_REDIS_URL="redis://127.0.0.1:$redis_port/0" \
RUNKITE_TEST_ISOLATED_REPETITIONS="$repetitions" \
  "$python_command" scripts/start-isolated-fixture.py \
    --cli "$RUNKITE_TEST_ISOLATED_CLI" --output-dir "$output/runtime" \
    --docker-socket "${RUNKITE_TEST_DOCKER_SOCKET:-/var/run/docker.sock}" --tls -- \
    "$python_command" -m unittest discover -s test/e2e/isolated -p test_managed.py -v &
fixture_pid=$!
test_status=0
wait "$fixture_pid" || test_status=$?
fixture_pid=""
exit "$test_status"
