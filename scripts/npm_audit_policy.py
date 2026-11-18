#!/usr/bin/env python3
"""Evaluate npm audit without changing its retained report or advisory severity."""

import hashlib
import json
import re
import sys
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path

ADVISORY = "GHSA-fc5q-39xc-vjpf"
OWNER = "Sharan Harsoor"
APPROVAL = "2026-11-18T06:20:46Z"
EXPIRY = "2026-11-25T00:00:00Z"
CONDITION = (
    "Activate only after the full hosted CI test suite passes and the maintainer records verified qualification."
)
MANIFEST_SHA256 = "589b51689b7fd8d0b7be89dfa6775cdf3eba53ee9f23fad771c49fd336951e84"
LOCKFILE_SHA256 = "d07e1958b224e724ec30dd498e2c02468976bb4949e1268e3bc64d84b65b3788"
# Canonical, complete npm audit v2 report reviewed on 2026-11-18. This binds all
# five high propagation nodes, exact paths/via/effects, the sole advisory source
# 1247710 and URL, ranges/fix suggestions, and complete dependency/count metadata.
# Any new or changed report field requires review, including a proposed fix.
AUDIT_SHA256 = "4bd0896e16d6ee14d7120f585000a472a6fb427cc244f4b0d831813b8287326f"
DEV_VERSIONS = {
    "@vitejs/plugin-react": "6.0.4",
    "fdir": "6.5.0",
    "picomatch": "4.0.5",
    "tinyglobby": "0.2.17",
    "vite": "8.2.4",
}
SEVERITIES = ("info", "low", "moderate", "high", "critical")
DEPENDENCY_COUNTS = {"prod", "dev", "optional", "peer", "peerOptional", "total"}
POLICY_FIELDS = {
    "schema_version",
    "status",
    "owner",
    "approval_recorded_at",
    "expires_at",
    "condition",
    "advisory",
    "manifest_sha256",
    "lockfile_sha256",
    "qualification",
}
QUALIFICATION_FIELDS = {
    "repository",
    "workflow",
    "job",
    "conclusion",
    "run_id",
    "run_attempt",
    "head_sha",
    "run_url",
    "completed_at",
    "verified_by",
    "manifest_sha256",
    "lockfile_sha256",
}


@dataclass(frozen=True)
class Decision:
    exit_code: int
    message: str
    accepted_risk: bool = False


def _timestamp(value):
    if (
        not isinstance(value, str)
        or re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?Z", value) is None
    ):
        raise ValueError("expected UTC timestamp")
    return datetime.fromisoformat(value[:-1] + "+00:00")


def _canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode("utf-8")


def _admin(cwd, repo_root):
    return Path(cwd).resolve() == (Path(repo_root).resolve() / "admin-ui").resolve()


def _admin_high_count(report):
    """Require a complete, internally consistent npm v2 report even when clean."""
    if type(report) is not dict or set(report) != {"auditReportVersion", "vulnerabilities", "metadata"}:
        return None
    if type(report["auditReportVersion"]) is not int or report["auditReportVersion"] != 2:
        return None
    vulnerabilities, metadata = report["vulnerabilities"], report["metadata"]
    if type(vulnerabilities) is not dict or type(metadata) is not dict:
        return None
    if set(metadata) != {"vulnerabilities", "dependencies"}:
        return None
    counts, dependencies = metadata["vulnerabilities"], metadata["dependencies"]
    if type(counts) is not dict or set(counts) != {*SEVERITIES, "total"}:
        return None
    if type(dependencies) is not dict or set(dependencies) != DEPENDENCY_COUNTS:
        return None
    if any(type(value) is not int or value < 0 for value in [*counts.values(), *dependencies.values()]):
        return None
    actual = dict.fromkeys(SEVERITIES, 0)
    for name, entry in vulnerabilities.items():
        if type(entry) is not dict or entry.get("name") != name:
            return None
        severity = entry.get("severity")
        if not isinstance(severity, str) or severity not in actual:
            return None
        actual[severity] += 1
    if any(counts[severity] != actual[severity] for severity in SEVERITIES):
        return None
    if counts["total"] != sum(actual.values()):
        return None
    return counts["high"] + counts["critical"]


def _active(policy, now):
    if type(policy) is not dict or set(policy) != POLICY_FIELDS:
        return False
    if (
        type(policy["schema_version"]) is not int
        or policy["schema_version"] != 1
        or policy["status"] != "active"
        or policy["owner"] != OWNER
        or policy["approval_recorded_at"] != APPROVAL
        or policy["condition"] != CONDITION
        or policy["advisory"] != ADVISORY
        or policy["manifest_sha256"] != MANIFEST_SHA256
        or policy["lockfile_sha256"] != LOCKFILE_SHA256
    ):
        return False
    approval, expiry = _timestamp(APPROVAL), _timestamp(policy["expires_at"])
    if not approval <= now < expiry <= min(_timestamp(EXPIRY), approval + timedelta(days=7)):
        return False
    proof = policy["qualification"]
    if type(proof) is not dict or set(proof) != QUALIFICATION_FIELDS:
        return False
    if (
        proof["repository"] != "getrunkite/runkite"
        or proof["workflow"] != ".github/workflows/ci.yml"
        or proof["job"] != "test-suite"
        or proof["conclusion"] != "success"
        or proof["verified_by"] != OWNER
        or proof["manifest_sha256"] != MANIFEST_SHA256
        or proof["lockfile_sha256"] != LOCKFILE_SHA256
        or type(proof["run_id"]) is not int
        or proof["run_id"] <= 0
        or type(proof["run_attempt"]) is not int
        or proof["run_attempt"] <= 0
        or not isinstance(proof["head_sha"], str)
        or re.fullmatch(r"[0-9a-f]{40}", proof["head_sha"]) is None
        or proof["run_url"] != f"https://github.com/getrunkite/runkite/actions/runs/{proof['run_id']}"
    ):
        return False
    # This is reviewed maintainer evidence, not an online GitHub verification or
    # cryptographic attestation. Current CI still independently requires its suite.
    return approval <= _timestamp(proof["completed_at"]) <= now


def _inputs_match(repo_root):
    directory = Path(repo_root).resolve() / "admin-ui"
    manifest_bytes = (directory / "package.json").read_bytes()
    lock_bytes = (directory / "package-lock.json").read_bytes()
    if hashlib.sha256(manifest_bytes).hexdigest() != MANIFEST_SHA256:
        return False
    if hashlib.sha256(lock_bytes).hexdigest() != LOCKFILE_SHA256:
        return False
    manifest, lock = json.loads(manifest_bytes), json.loads(lock_bytes)
    for name, version in DEV_VERSIONS.items():
        node = lock["packages"]["node_modules/" + name]
        if node["version"] != version or node.get("dev") is not True:
            return False
        if name in manifest.get("dependencies", {}):
            return False
    return all(name in manifest["devDependencies"] for name in ("vite", "@vitejs/plugin-react"))


def _legacy(report):
    if report is None or (type(report) is dict and report.get("error")):
        return Decision(0, "::warning::npm audit API unavailable or returned malformed JSON; continuing")
    if type(report) is not dict:
        return Decision(1, "npm audit returned an invalid report")
    metadata = report.get("metadata") or {}
    if type(metadata) is not dict:
        return Decision(1, "npm audit returned invalid vulnerability metadata")
    counts = metadata.get("vulnerabilities") or {}
    if type(counts) is not dict:
        return Decision(1, "npm audit returned invalid vulnerability metadata")
    if int(counts.get("high") or 0) + int(counts.get("critical") or 0):
        return Decision(1, "npm audit reported high/critical vulnerabilities")
    vulnerabilities = report.get("vulnerabilities")
    if isinstance(vulnerabilities, dict) and any(
        isinstance(value, dict) and value.get("severity") in ("high", "critical") for value in vulnerabilities.values()
    ):
        return Decision(1, "npm audit reported high/critical vulnerabilities")
    return Decision(0, "")


def evaluate(report, cwd, repo_root, policy, now):
    """Return a decision; only the explicit arguments and bound source files matter."""
    try:
        if not _admin(cwd, repo_root):
            return _legacy(report)
        high = _admin_high_count(report)
        if high is None:
            return Decision(1, "Admin npm audit requires a complete, consistent npm audit v2 report")
        if not high:
            if report["vulnerabilities"]:
                return Decision(1, "Admin npm audit contains findings outside the exact conditional exception")
            return Decision(0, "")
        if not isinstance(now, datetime) or now.tzinfo is None or not _active(policy, now):
            return Decision(1, "Admin npm audit reported high/critical vulnerabilities; exception is not active")
        if not _inputs_match(repo_root) or hashlib.sha256(_canonical(report)).hexdigest() != AUDIT_SHA256:
            return Decision(1, "Admin npm audit does not match the exact approved development-only inputs and report")
        return Decision(
            0,
            f"::warning::Accepted risk {ADVISORY} for Admin development/build dependencies only "
            f"until {policy['expires_at']}; 5 HIGH findings remain. This does not fix the vulnerability.",
            True,
        )
    except (OSError, ValueError, TypeError, KeyError, OverflowError):
        return Decision(1, "npm audit policy validation failed")


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def _load(raw):
    def invalid_constant(_):
        raise ValueError("invalid JSON constant")

    return json.loads(raw, object_pairs_hook=_unique_object, parse_constant=invalid_constant)


def main(argv=None):
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 2 or re.fullmatch(r"[0-9]+", args[1]) is None:
        print("npm audit policy requires one captured report and npm exit status")
        return 1
    root = Path(__file__).resolve().parents[1]
    report, raw, policy = None, "", None
    captured_report_invalid = False
    try:
        raw = Path(args[0]).read_text(encoding="utf-8")
        report = _load(raw)
        print(raw.rstrip())
    except json.JSONDecodeError:
        pass
    except (OSError, ValueError, UnicodeError):
        captured_report_invalid = True
    try:
        policy = _load((root / "scripts/admin-audit-exception.json").read_text(encoding="utf-8"))
    except (OSError, ValueError, UnicodeError):
        pass
    cwd = Path.cwd()
    if captured_report_invalid:
        decision = Decision(1, "npm audit captured report could not be safely read or parsed")
    else:
        decision = evaluate(report, cwd, root, policy, datetime.now(timezone.utc))
    if not captured_report_invalid and _admin(cwd, root):
        high = _admin_high_count(report)
        if high is None or int(args[1]) != (1 if high else 0):
            decision = Decision(1, "Admin npm audit command status or report is inconsistent")
    if decision.message:
        print(decision.message)
    return decision.exit_code


if __name__ == "__main__":
    sys.exit(main())
