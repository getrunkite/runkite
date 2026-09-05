#!/usr/bin/env bash
# Fail on high/critical npm audit findings. If the audit API or registry
# is down (non-JSON / error payload), warn and continue so a flaky
# endpoint does not red-gate an otherwise-green lint+build.
set -u
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

set +e
npm audit --json --audit-level=high >"$tmp" 2>/dev/null
set -e

python3 - "$tmp" <<'PY'
import json, sys

raw = open(sys.argv[1], encoding="utf-8").read().strip()
if not raw:
    print("::warning::npm audit returned empty output; continuing")
    sys.exit(0)
try:
    data = json.loads(raw)
except json.JSONDecodeError:
    print("::warning::npm audit did not return JSON; continuing")
    sys.exit(0)
err = data.get("error")
if err:
    print(f"::warning::npm audit API unavailable ({err}); continuing")
    sys.exit(0)
meta = (data.get("metadata") or {}).get("vulnerabilities") or {}
high = int(meta.get("high") or 0) + int(meta.get("critical") or 0)
if high:
    sys.exit(1)
# npm v6-style / missing metadata: inspect per-package severity.
vulns = data.get("vulnerabilities")
if isinstance(vulns, dict):
    for v in vulns.values():
        if isinstance(v, dict) and v.get("severity") in ("high", "critical"):
            sys.exit(1)
sys.exit(0)
PY
status=$?
if [ "$status" -ne 0 ]; then
  npm audit --audit-level=high || true
  echo "npm audit reported high/critical vulnerabilities"
  exit 1
fi
