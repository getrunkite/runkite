#!/usr/bin/env bash
# Qualify tenant review against two real control-plane processes. All database
# services belong to this invocation; inherited database URLs are never used.
set -euo pipefail
# Fixture passwords must not enter shell tracing even if the caller enabled it.
set +x

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

repetitions="${RUNKITE_TEST_REVIEW_REPETITIONS:-1}"
if [[ ! "$repetitions" =~ ^([1-9]|1[0-9]|20)$ ]]; then
  echo "RUNKITE_TEST_REVIEW_REPETITIONS must be an integer from 1 to 20" >&2
  exit 2
fi

for required in docker go python3 mktemp tee; do
  if ! command -v "$required" >/dev/null 2>&1; then
    echo "Required command is missing: $required" >&2
    exit 2
  fi
done
docker compose version >/dev/null

if [[ -n "${RUNKITE_TEST_REVIEW_OUTPUT:-}" ]]; then
  mkdir -p "$RUNKITE_TEST_REVIEW_OUTPUT"
  output_base="$(cd "$RUNKITE_TEST_REVIEW_OUTPUT" && pwd)"
  output_dir="$(mktemp -d "$output_base/run.XXXXXX")"
else
  output_dir="$(mktemp -d "${TMPDIR:-/tmp}/runkite-review-ha-output.XXXXXX")"
fi
scratch="$(mktemp -d "${TMPDIR:-/tmp}/runkite-review-ha.XXXXXX")"
suffix="$(basename "$scratch" | tr -cd '[:alnum:]' | tr '[:upper:]' '[:lower:]')"
project="runkite-review-ha-$$-$suffix"
database="runkite_review_ha_$$_$suffix"
override="$scratch/compose.override.yml"
compose=(docker compose --env-file /dev/null --project-name "$project" -f "$repo_root/docker-compose.test.yml" -f "$override")
compose_started=0
supervisor_pid=""

cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n "$supervisor_pid" ]]; then
    # The supervisor owns the test session and every inherited serve process.
    # Wait for that bounded cleanup before tearing down their databases.
    kill -TERM "$supervisor_pid" 2>/dev/null || true
    wait "$supervisor_pid" 2>/dev/null || true
    supervisor_pid=""
  fi
  if [[ "$compose_started" == 1 ]]; then
    "${compose[@]}" logs --no-color postgres redis >"$output_dir/compose.log" 2>&1 || true
    if ! "${compose[@]}" down --volumes --remove-orphans >"$output_dir/cleanup.log" 2>&1; then
      echo "Disposable Compose cleanup failed; inspect $output_dir/cleanup.log" >&2
      if [[ "$status" == 0 ]]; then status=1; fi
    fi
  fi
  unset RUNKITE_REVIEW_HA_PG_PASSWORD fixture_password postgres_url
  rm -rf "$scratch"
  echo "Review HA evidence: $output_dir"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# These credentials exist only for this invocation's disposable container.
# Never reuse a caller's database password or write the value into an artifact.
fixture_password="$(python3 -c 'import secrets; print(secrets.token_urlsafe(32))')"
export RUNKITE_REVIEW_HA_PG_PASSWORD="$fixture_password"

# !override replaces the base file's fixed host mappings instead of merging
# them. An explicit env file prevents Compose from loading a repository .env.
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
      POSTGRES_PASSWORD: "\${RUNKITE_REVIEW_HA_PG_PASSWORD:?fixture password is required}"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U runkite -d $database"]
  redis:
    ports: !override
      - target: 6379
        published: "0"
        host_ip: 127.0.0.1
        protocol: tcp
EOF

echo "Review HA evidence: $output_dir"
# Validate the merge before starting anything, including Compose support for
# replacement port mappings. Only these two services are ever started below.
"${compose[@]}" config --quiet >"$output_dir/compose-config-validation.log" 2>"$output_dir/compose-config-errors.log"
"${compose[@]}" config --no-interpolate >"$output_dir/compose-config.yml" 2>>"$output_dir/compose-config-errors.log"

if [[ -n "${RUNKITE_TEST_BINARY:-}" ]]; then
  if [[ ! -f "$RUNKITE_TEST_BINARY" || ! -x "$RUNKITE_TEST_BINARY" ]]; then
    echo "RUNKITE_TEST_BINARY must name an executable host binary" >&2
    exit 2
  fi
  binary="$(cd "$(dirname "$RUNKITE_TEST_BINARY")" && pwd)/$(basename "$RUNKITE_TEST_BINARY")"
else
  binary="$scratch/runkite"
  go build -o "$binary" ./cmd/ 2>&1 | tee "$output_dir/build.log"
fi

# Set the flag before up so partial startup failures still clean up this
# invocation's containers, network and volumes. No shared infra is selected.
compose_started=1
"${compose[@]}" up --detach --wait --wait-timeout 90 --no-deps postgres redis 2>&1 | tee "$output_dir/startup.log"
postgres_address="$("${compose[@]}" port postgres 5432)"
redis_address="$("${compose[@]}" port redis 6379)"
if [[ "$postgres_address" =~ ^127\.0\.0\.1:([0-9]+)$ ]]; then
  postgres_port="${BASH_REMATCH[1]}"
else
  echo "Unexpected Postgres host mapping: $postgres_address" >&2
  exit 1
fi
if [[ "$redis_address" =~ ^127\.0\.0\.1:([0-9]+)$ ]]; then
  redis_port="${BASH_REMATCH[1]}"
else
  echo "Unexpected Redis host mapping: $redis_address" >&2
  exit 1
fi

postgres_url="$(RUNKITE_REVIEW_HA_PG_PORT="$postgres_port" RUNKITE_REVIEW_HA_PG_DB="$database" python3 - <<'PY'
import os
from urllib.parse import quote, urlunsplit

username = quote("runkite", safe="")
password = quote(os.environ["RUNKITE_REVIEW_HA_PG_PASSWORD"], safe="")
host = "127.0.0.1:" + os.environ["RUNKITE_REVIEW_HA_PG_PORT"]
database = quote(os.environ["RUNKITE_REVIEW_HA_PG_DB"], safe="")
print(urlunsplit(("postgres", username + ":" + password + "@" + host, "/" + database, "sslmode=disable", "")))
PY
)"

# Preserve the caller's Go toolchain/cache environment. Test infrastructure
# variables below always come from the disposable project, never the shell.
RUNKITE_TEST_REVIEW_HA=1 \
RUNKITE_TEST_REVIEW_POSTGRES_DSN="$postgres_url" \
RUNKITE_TEST_REVIEW_REDIS_URL="redis://127.0.0.1:$redis_port/0" \
RUNKITE_TEST_BINARY="$binary" \
RUNKITE_TEST_REVIEW_REPETITIONS="$repetitions" \
RUNKITE_TEST_REVIEW_OUTPUT="$output_dir" \
  python3 scripts/review-ha-supervisor.py --log "$output_dir/test.log" -- \
    go test ./test/e2e/reviewha -v -count=1 -timeout "$((repetitions * 180 + 120))s" &
supervisor_pid=$!
test_status=0
wait "$supervisor_pid" || test_status=$?
supervisor_pid=""
exit "$test_status"
