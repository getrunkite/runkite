"""Qualification-only policy observations, not a production authorization gate."""

from __future__ import annotations

import asyncio
import contextlib
import copy
import hashlib
import json
import math
import os
import re
import stat
import tempfile
from decimal import Decimal
from pathlib import Path

MAX_BYTES = 1 << 20
CONTROL_TIMEOUT = 20
PROVER_TIMEOUT = 15
VERSION_TIMEOUT = 5
DOMAINS = {"filesystem", "network_l4", "network_rest", "process", "landlock"}
NAME = re.compile(r"rk-[a-f0-9]{16}\Z")
HASH = re.compile(r"[a-f0-9]{64}\Z")


class PolicyQualificationError(RuntimeError):
    """Fixed diagnostics exclude private policy, paths and upstream reasons."""


def require(condition, message):
    if not condition:
        raise PolicyQualificationError(message)


def canonical(value):
    return (
        json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode() + b"\n"
    )


def digest(value):
    return hashlib.sha256(value).hexdigest()


def decode(raw):
    require(isinstance(raw, bytes) and 0 < len(raw) <= MAX_BYTES, "policy response exceeds its bounded contract")

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "policy response contains duplicate fields")
            result[key] = value
        return result

    def number(value):
        parsed = float(value)
        require(
            math.isfinite(parsed) and Decimal(str(parsed)) == Decimal(value), "policy response has an inexact number"
        )
        return parsed

    def invalid(_value):
        raise PolicyQualificationError("policy response has a nonfinite number")

    try:
        result = json.loads(raw, object_pairs_hook=pairs, parse_float=number, parse_constant=invalid)
    except (ValueError, UnicodeError, RecursionError):
        raise PolicyQualificationError("policy response is not valid bounded JSON") from None
    require(isinstance(result, dict), "policy response must be an object")
    return result


def positive(value, maximum=(1 << 64) - 1):
    return type(value) is int and 0 < value <= maximum


def check_scope(value, name):
    require(
        value.get("scope") == "sandbox" and value.get("sandbox") == name, "policy response has another scope or sandbox"
    )
    require(positive(value.get("version"), (1 << 32) - 1), "policy response has an invalid revision")
    require(
        type(value.get("active_version")) is int and value["active_version"] == value["version"],
        "policy revision is not active",
    )
    require(isinstance(value.get("hash"), str) and HASH.fullmatch(value["hash"]), "policy response has an invalid hash")
    require(isinstance(value.get("policy"), dict) and value["policy"], "full policy object is missing")


def check_current(value, name):
    required = {
        "scope",
        "sandbox",
        "version",
        "active_version",
        "hash",
        "status",
        "config_revision",
        "policy_source",
        "policy",
    }
    require(
        required <= value.keys() and not value.keys() - required - {"global_policy_version"},
        "current policy metadata is unsupported",
    )
    check_scope(value, name)
    network_rules = value["policy"].get("network_policies", {})
    require(
        isinstance(network_rules, dict) and not any(name.startswith("_provider_") for name in network_rules),
        "provider-composed network policy is unsupported",
    )
    require(
        value["status"] == "effective" and value["policy_source"] == "sandbox",
        "global or ambiguous policy sources are unsupported",
    )
    require(
        "global_policy_version" not in value
        or type(value["global_policy_version"]) is int
        and value["global_policy_version"] == 0,
        "global policy composition is unsupported",
    )
    require(positive(value["config_revision"]), "current policy configuration revision is invalid")


def check_loaded(value, current, name):
    required = {"scope", "sandbox", "version", "active_version", "hash", "status", "policy", "loaded_at_ms"}
    optional = {"created_at_ms", "load_error", "provenance"}
    require(
        required <= value.keys() and not value.keys() - required - optional, "loaded policy metadata is unsupported"
    )
    check_scope(value, name)
    require(
        value["status"] == "loaded" and value.get("load_error", "") == "",
        "exact policy revision was not loaded successfully",
    )
    require(positive(value["loaded_at_ms"]), "loaded policy timestamp is invalid")
    if "created_at_ms" in value:
        require(
            positive(value["created_at_ms"]) and value["created_at_ms"] <= value["loaded_at_ms"],
            "policy creation/load timestamps contradict",
        )
    provenance = value.get("provenance", {})
    require(
        isinstance(provenance, dict)
        and all(isinstance(key, str) and isinstance(item, str) for key, item in provenance.items()),
        "policy revision provenance is unsupported",
    )
    require(provenance.get("source") != "provider-composition", "provider-composed policy is unsupported")
    require(
        value["version"] == current["version"]
        and value["hash"] == current["hash"]
        and canonical(value["policy"]) == canonical(current["policy"]),
        "loaded and current policy identities differ",
    )


class PolicyQualification:
    def __init__(self, boundary_bytes: bytes, descriptor: str, prover: str):
        require(
            isinstance(boundary_bytes, bytes) and 0 < len(boundary_bytes) <= MAX_BYTES,
            "boundary policy exceeds its bounded contract",
        )
        require(
            isinstance(descriptor, str) and HASH.fullmatch(descriptor), "configured descriptor must be a SHA-256 digest"
        )
        try:
            self._prover = Path(prover).resolve(strict=True)
            require(
                self._prover.is_file() and os.access(self._prover, os.X_OK),
                "qualified prover executable is unavailable",
            )
            self._binary_hash = self._hash_binary()
        except OSError:
            raise PolicyQualificationError("qualified prover executable is unavailable") from None
        self._boundary = boundary_bytes
        self._descriptor = descriptor
        self._attempts = {}
        self._failed = False

    def _hash_binary(self):
        hasher = hashlib.sha256()
        with self._prover.open("rb") as source:
            require(stat.S_ISREG(os.fstat(source.fileno()).st_mode), "qualified prover must be a regular file")
            size = 0
            while block := source.read(65536):
                size += len(block)
                require(size <= 512 << 20, "qualified prover executable exceeds limit")
                hasher.update(block)
        return hasher.hexdigest()

    async def _process(self, args, directory, timeout):
        require(self._hash_binary() == self._binary_hash, "qualified prover binary changed")
        environment = {
            "PATH": os.defpath,
            "HOME": str(directory),
            "TMPDIR": str(directory),
            "LANG": "C",
            "NETRC": os.devnull,
        }
        proc = await asyncio.create_subprocess_exec(
            str(self._prover),
            *args,
            stdin=asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
            env=environment,
        )
        try:
            async with asyncio.timeout(timeout):
                output = bytearray()
                while block := await proc.stdout.read(min(65536, MAX_BYTES + 1 - len(output))):
                    output.extend(block)
                    require(len(output) <= MAX_BYTES, "prover response exceeds limit")
                code = await proc.wait()
            require(self._hash_binary() == self._binary_hash, "qualified prover binary changed")
            return code, bytes(output)
        finally:
            if proc.returncode is None:
                with contextlib.suppress(ProcessLookupError):
                    proc.kill()
                async with asyncio.timeout(2):
                    await proc.communicate()

    async def _current(self, name, control):
        async with asyncio.timeout(CONTROL_TIMEOUT):
            value = decode(await control("policy", "get", name, "--full", "-o", "json"))
        check_current(value, name)
        return value

    async def _snapshot(self, name, control):
        current = await self._current(name, control)
        async with asyncio.timeout(CONTROL_TIMEOUT):
            loaded = decode(
                await control("policy", "get", name, "--rev", str(current["version"]), "--full", "-o", "json")
            )
        check_loaded(loaded, current, name)
        return current, loaded

    async def _check(self, policy):
        candidate = canonical(policy)
        require(len(candidate) <= MAX_BYTES, "full policy object exceeds limit")
        with tempfile.TemporaryDirectory(prefix="runkite-policy-qualification-") as temporary:
            directory = Path(temporary)
            paths = {"candidate": directory / "candidate.json", "boundary": directory / "boundary.yaml"}
            for key, data in (("candidate", candidate), ("boundary", self._boundary)):
                descriptor = os.open(paths[key], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(descriptor, "wb") as target:
                    target.write(data)
            code, version = await self._process(["--version"], directory, VERSION_TIMEOUT)
            require(
                code == 0 and version.strip() == b"openshell-prover 0.1.8",
                "prover version is not the qualified release",
            )
            code, raw = await self._process(
                [
                    "check",
                    str(paths["candidate"]),
                    "--boundary",
                    str(paths["boundary"]),
                    "-o",
                    "json",
                    "--timeout",
                    "10s",
                ],
                directory,
                PROVER_TIMEOUT,
            )
            result = decode(raw)
            require(
                set(result)
                == {
                    "schema_version",
                    "prover_version",
                    "check",
                    "coverage",
                    "result",
                    "exit_code",
                    "inputs",
                    "counterexample",
                    "reason_code",
                    "reason",
                },
                "prover result schema is unsupported",
            )
            require(
                type(result["schema_version"]) is int
                and result["schema_version"] == 1
                and result["prover_version"] == "0.1.8"
                and result["check"] == "boundary",
                "prover result version or check differs",
            )
            require(
                code == 0
                and type(result["exit_code"]) is int
                and result["exit_code"] == 0
                and result["result"] == "within_boundary",
                "prover did not establish configuration containment",
            )
            coverage = result["coverage"]
            require(
                isinstance(coverage, dict) and set(coverage) == {"domains"} and isinstance(coverage["domains"], list),
                "prover domain coverage is missing",
            )
            domains = coverage["domains"]
            require(
                len(domains) == 5 and all(isinstance(domain, str) for domain in domains) and set(domains) == DOMAINS,
                "prover does not cover all qualified domains",
            )
            require(
                result["inputs"] == {key: str(path) for key, path in paths.items()},
                "prover result identifies different inputs",
            )
            require(
                all(result[key] is None for key in ("counterexample", "reason_code", "reason")),
                "prover success contains contradictory failure details",
            )
        return digest(candidate)

    async def before_exec(self, name, control):
        try:
            require(
                not self._failed and isinstance(name, str) and NAME.fullmatch(name) and name not in self._attempts,
                "policy qualification attempt is invalid or already failed",
            )
            state = {"complete": False}
            self._attempts[name] = state
            current, loaded = await self._snapshot(name, control)
            candidate_hash = await self._check(current["policy"])
            require(
                canonical(await self._current(name, control)) == canonical(current),
                "current policy changed during containment checking",
            )
            state.update(current=canonical(current), loaded=canonical(loaded))
            state["evidence"] = {
                "sandbox": name,
                "configured_descriptor_sha256": self._descriptor,
                "configured_policy_sha256": digest(self._boundary),
                "policy_source": "sandbox",
                "policy_version": current["version"],
                "config_revision": current["config_revision"],
                "reported_policy_hash": current["hash"],
                "loaded_at_ms": loaded["loaded_at_ms"],
                "loaded_provenance_sha256": digest(canonical(loaded.get("provenance", {}))),
                "prover_version": "0.1.8",
                "prover_binary_sha256": self._binary_hash,
                "prover_inputs_sha256": {"candidate": candidate_hash, "boundary": digest(self._boundary)},
                "coverage_domains": sorted(DOMAINS),
                "status": "configuration_within_boundary",
            }
        except BaseException as error:
            self._failed = True
            if isinstance(error, asyncio.CancelledError):
                raise
            raise PolicyQualificationError("before-exec policy qualification failed") from None

    async def after_exec(self, name, control):
        try:
            state = self._attempts.get(name)
            require(
                not self._failed and state and not state["complete"] and "evidence" in state,
                "policy qualification attempt is incomplete or already failed",
            )
            current, loaded = await self._snapshot(name, control)
            require(
                canonical(current) == state["current"] and canonical(loaded) == state["loaded"],
                "current or loaded policy changed during probes",
            )
            state["complete"] = True
            state.pop("current")
            state.pop("loaded")
        except BaseException as error:
            self._failed = True
            if isinstance(error, asyncio.CancelledError):
                raise
            raise PolicyQualificationError("after-exec policy qualification failed") from None

    def summary(self):
        require(
            not self._failed and self._attempts and all(state["complete"] for state in self._attempts.values()),
            "policy qualification has failed or incomplete attempts",
        )
        return {"passed": True, "attempts": [copy.deepcopy(state["evidence"]) for state in self._attempts.values()]}
