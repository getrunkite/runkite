#!/usr/bin/env python3
"""Validate measured isolated HA receipts without exposing private fixture data."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import re
import stat
import subprocess
import sys
from pathlib import Path

SCOPE = "two real Postgres/Redis planes + mTLS worker identity + installed parent + OpenShell"
MAX_RECEIPT_BYTES = 1 << 20


class EvidenceError(ValueError):
    """A fixed, nonsecret qualification failure."""


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise EvidenceError("qualification receipt contains duplicate keys")
        result[key] = value
    return result


def _nonfinite(_value):
    raise EvidenceError("qualification receipt contains a nonfinite value")


def _read(path: Path) -> dict:
    try:
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as handle:
            metadata = os.fstat(handle.fileno())
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_size > MAX_RECEIPT_BYTES:
                raise EvidenceError("qualification receipt is not a bounded regular file")
            raw = handle.read(MAX_RECEIPT_BYTES + 1)
        if len(raw) > MAX_RECEIPT_BYTES:
            raise EvidenceError("qualification receipt exceeds its size bound")
        value = json.loads(raw, object_pairs_hook=_object, parse_constant=_nonfinite)
    except (OSError, UnicodeError, ValueError) as error:
        raise EvidenceError("required qualification receipt is unavailable or malformed") from error
    if not isinstance(value, dict):
        raise EvidenceError("qualification receipt must be an object")
    return value


def _cases(value: object, minimum: int) -> list[str]:
    if not isinstance(value, list) or not minimum <= len(value) <= 200:
        raise EvidenceError("qualification cases are incomplete")
    if any(not isinstance(case, str) or not case or len(case) > 4096 for case in value) or len(set(value)) != len(
        value
    ):
        raise EvidenceError("qualification cases are malformed or duplicated")
    return value


def _finite_number(value):
    return type(value) is int or (type(value) is float and math.isfinite(value))


def _source(path: Path, repetitions: int) -> dict:
    value = _read(path)
    expected = {"revision", "version", "architecture", "kernel", "repetitions", "assessment"}
    if (
        set(value) != expected
        or not isinstance(value["revision"], str)
        or not re.fullmatch(r"[0-9a-f]{40}", value["revision"])
    ):
        raise EvidenceError("source context is incomplete")
    if type(value["repetitions"]) is not int or value["repetitions"] != repetitions:
        raise EvidenceError("source context repetition count differs")
    if not isinstance(value["version"], str) or not re.fullmatch(
        r"[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?", value["version"]
    ):
        raise EvidenceError("source context version is malformed")
    for name in ("architecture", "kernel", "assessment"):
        if (
            not isinstance(value[name], str)
            or not 1 <= len(value[name]) <= 256
            or not value[name].isascii()
            or not value[name].isprintable()
        ):
            raise EvidenceError("source context fields are malformed")
    if value["assessment"] != "maintainer combined fixture, not independent review":
        raise EvidenceError("source context assessment differs")
    return value


def _artifacts(path: Path) -> dict:
    value = _read(path)
    expected = {"schema_version", "binary_sha256", "runner_wheel_sha256", "openshell_cli_sha256", "checkout_context"}
    if set(value) != expected or type(value["schema_version"]) is not int or value["schema_version"] != 1:
        raise EvidenceError("artifact context is incomplete")
    for name in ("binary_sha256", "runner_wheel_sha256", "openshell_cli_sha256"):
        if not isinstance(value[name], str) or not re.fullmatch(r"[0-9a-f]{64}", value[name]):
            raise EvidenceError("artifact hash is malformed")
    checkout = value["checkout_context"]
    if checkout is not None and (
        not isinstance(checkout, dict)
        or set(checkout) != {"revision", "dirty"}
        or not isinstance(checkout["revision"], str)
        or not re.fullmatch(r"[0-9a-f]{40}", checkout["revision"])
        or type(checkout["dirty"]) is not bool
    ):
        raise EvidenceError("checkout context is malformed")
    return value


def _measured_guards(managed: dict, repetitions: int) -> None:
    cancellations = managed.get("cancellation_observations")
    live = managed.get("live_generation_observations")
    if (
        not isinstance(cancellations, list)
        or len(cancellations) != repetitions + 1
        or not isinstance(live, list)
        or len(live) != repetitions
    ):
        raise EvidenceError("measured cancellation or live-generation guards are incomplete")
    for observations in (cancellations, live):
        seen = set()
        for observation in observations:
            if not isinstance(observation, dict):
                raise EvidenceError("measured guard observation is malformed")
            run = observation.get("run_id")
            generation = observation.get("generation")
            if (
                not isinstance(run, str)
                or not 1 <= len(run) <= 256
                or run in seen
                or type(generation) is not int
                or generation < 1
            ):
                raise EvidenceError("measured guard identity is malformed")
            seen.add(run)
    for index, observation in enumerate(cancellations):
        elapsed = observation.get("elapsed_seconds")
        if (
            observation.get("method") != "read_only_selector"
            or observation.get("observed_present_before_cancel") is not True
            or observation.get("observed_absent_before_teardown") is not True
            or not isinstance(observation.get("sandbox"), str)
            or not re.fullmatch(r"rk-[a-f0-9]{16}", observation["sandbox"])
            or not _finite_number(elapsed)
            or elapsed < 0
        ):
            raise EvidenceError("owned runtime absence was not measured before teardown")
        for field in (
            "run_live_before_presence",
            "run_live_after_presence",
            "generation_matched_before_cancel",
            "workload_activity_observed",
        ):
            if observation.get(field) is not True:
                raise EvidenceError("active-workload cancellation positive controls are incomplete")
        prompt, completion = (5, 2) if index == 0 else (7, 3)
        for field, expected in (("activity_prompt_tokens", prompt), ("activity_completion_tokens", completion)):
            if type(observation.get(field)) is not int or observation[field] != expected:
                raise EvidenceError("active-workload cancellation activity proof differs")
    for observation in live:
        if (
            type(observation.get("mismatched_generation")) is not int
            or observation["mismatched_generation"] != observation["generation"] - 1
        ):
            raise EvidenceError("mismatched generation probe is malformed")
        for field in ("live_before", "live_after", "checkpoint_unchanged", "usage_unchanged"):
            if observation.get(field) is not True:
                raise EvidenceError("live-generation positive controls did not pass")
        for field, expected in (
            ("usage_current_get", 200),
            ("usage_idempotent_post", 200),
            ("usage_mismatched_post", 403),
            ("checkpoint_current_get", 200),
            ("checkpoint_mismatched_put", 403),
        ):
            if type(observation.get(field)) is not int or observation[field] != expected:
                raise EvidenceError("live-generation request guards did not pass")


def validate_evidence(
    runtime_dir: Path,
    repetitions: int,
    *,
    typescript: Path | None = None,
    source: Path | None = None,
    artifacts: Path | None = None,
    backend_cleanup: Path | None = None,
) -> dict:
    if type(repetitions) is not int or not 1 <= repetitions <= 20:
        raise EvidenceError("requested repetitions must be from 1 to 20")
    if runtime_dir.is_symlink() or not runtime_dir.is_dir():
        raise EvidenceError("owned runtime evidence directory is unavailable")
    managed = _read(runtime_dir / "managed-evidence.json")
    runtime = _read(runtime_dir / "runtime-fixture.json")
    cleanup = _read(runtime_dir / "runtime-cleanup.json")
    gateway = _read(runtime_dir / "gateway-tls-evidence.json")
    if (
        managed.get("passed") is not True
        or managed.get("scope") != SCOPE
        or managed.get("runtime_gateway_tls") is not True
    ):
        raise EvidenceError("native two-plane qualification did not pass")
    if type(managed.get("bounded_repetitions")) is not int or managed["bounded_repetitions"] != repetitions:
        raise EvidenceError("measured repetition count differs")
    elapsed = managed.get("elapsed_seconds")
    if not _finite_number(elapsed) or elapsed <= 0:
        raise EvidenceError("measured execution duration is unavailable")
    cases = _cases(managed.get("cases"), repetitions + 1)
    cycles = [case for case in cases if case.startswith("bounded cycle ")]
    if len(cycles) != repetitions or any(
        sum(case.startswith(f"bounded cycle {index}: ") for case in cycles) != 1 for index in range(1, repetitions + 1)
    ):
        raise EvidenceError("measured failover cycles are incomplete")
    _measured_guards(managed, repetitions)
    if (
        runtime.get("gateway_tls") is not True
        or runtime.get("client_auth") != "mtls"
        or runtime.get("release") != "0.1.8"
    ):
        raise EvidenceError("native gateway TLS profile differs")
    if cleanup.get("confirmed") is not True or cleanup.get("reason_codes") != []:
        raise EvidenceError("owned runtime cleanup is unconfirmed")
    if gateway.get("passed") is not True or len(_cases(gateway.get("cases"), 5)) < 5:
        raise EvidenceError("gateway TLS qualification is incomplete")
    result = {
        "schema_version": 1,
        "passed": True,
        "native_ha": True,
        "bounded_repetitions": repetitions,
        "managed_cases": len(cases),
        "gateway_tls": True,
        "cleanup_confirmed": True,
        "elapsed_seconds": elapsed,
        "cancellation_observations": repetitions + 1,
        "live_generation_observations": repetitions,
    }
    if typescript is not None:
        report = _read(typescript)
        if report.get("passed") is not True:
            raise EvidenceError("packaged TypeScript qualification did not pass")
        result["typescript_cases"] = len(_cases(report.get("cases"), 4))
    if source is not None:
        result["source_context"] = _source(source, repetitions)
    if artifacts is not None:
        result["artifact_context"] = _artifacts(artifacts)
    if backend_cleanup is not None:
        backend = _read(backend_cleanup)
        if backend.get("confirmed") is not True or backend.get("reason_codes") != []:
            raise EvidenceError("owned backend cleanup is unconfirmed")
        result["backend_cleanup_confirmed"] = True
    return result


def runtime_from_base(base: Path) -> Path:
    if base.is_symlink() or not base.is_dir():
        raise EvidenceError("owned run base is unavailable")
    runs = [path for path in base.iterdir() if path.name.startswith("run.")]
    if len(runs) != 1 or runs[0].is_symlink() or not runs[0].is_dir():
        raise EvidenceError("run base must identify exactly one fresh owned run")
    return runs[0] / "runtime"


def write_report(path: Path, value: dict) -> None:
    try:
        with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "w") as handle:
            json.dump(value, handle, indent=2, allow_nan=False)
            handle.write("\n")
    except OSError as error:
        raise EvidenceError("new qualification summary could not be written") from error


def record_artifacts(path: Path, binary: Path, wheel: Path, cli: Path, checkout: Path) -> None:
    result = {"schema_version": 1, "checkout_context": None}
    for key, artifact in (("binary_sha256", binary), ("runner_wheel_sha256", wheel), ("openshell_cli_sha256", cli)):
        digest = hashlib.sha256()
        with artifact.open("rb") as handle:
            while block := handle.read(1 << 20):
                digest.update(block)
        result[key] = digest.hexdigest()
    try:
        revision = subprocess.check_output(
            ["git", "-C", str(checkout), "rev-parse", "HEAD"], stderr=subprocess.DEVNULL, text=True, timeout=5
        ).strip()
        dirty = subprocess.check_output(
            ["git", "-C", str(checkout), "status", "--porcelain"], stderr=subprocess.DEVNULL, timeout=5
        )
        if re.fullmatch(r"[0-9a-f]{40}", revision):
            result["checkout_context"] = {"revision": revision, "dirty": bool(dirty)}
    except (OSError, subprocess.SubprocessError):
        pass
    write_report(path, result)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    location = parser.add_mutually_exclusive_group(required=True)
    location.add_argument("--runtime-dir", type=Path)
    location.add_argument("--run-base", type=Path)
    parser.add_argument("--repetitions", type=int, required=True)
    parser.add_argument("--typescript", type=Path)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--artifacts", type=Path)
    parser.add_argument("--backend-cleanup", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        directory = args.runtime_dir if args.runtime_dir is not None else runtime_from_base(args.run_base)
        artifacts = args.artifacts
        backend_cleanup = args.backend_cleanup
        if args.run_base is not None:
            artifacts = artifacts or directory.parent / "artifacts.json"
            backend_cleanup = backend_cleanup or directory.parent / "backend-cleanup.json"
        result = validate_evidence(
            directory,
            args.repetitions,
            typescript=args.typescript,
            source=args.source,
            artifacts=artifacts,
            backend_cleanup=backend_cleanup,
        )
        if args.output is not None:
            write_report(args.output, result)
    except EvidenceError as error:
        print(str(error), file=sys.stderr)
        return 1
    print("Measured isolated HA evidence and cleanup validated.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
