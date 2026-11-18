#!/usr/bin/env bash
# Fail on high/critical findings using the captured report once. The Admin
# exception requires reviewed qualification and exact source-bound eligibility.
set -u
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

set +e
npm audit --json --audit-level=high >"$tmp" 2>/dev/null
npm_status=$?
python3 "$script_dir/npm_audit_policy.py" "$tmp" "$npm_status"
status=$?
set -e
exit "$status"
