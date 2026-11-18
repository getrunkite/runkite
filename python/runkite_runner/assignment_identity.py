"""Exact original run-record identity shared by dispatch and native receipts."""

from __future__ import annotations

import hashlib


class AssignmentIdentityError(ValueError):
    """The original dispatched record cannot be proven. Do not execute."""


def valid_run_incarnation(value: object) -> bool:
    return type(value) is str and 0 < len(value) <= 256 and all(0x21 <= ord(char) <= 0x7E for char in value)


def require_run_incarnation(value: object) -> str:
    if not valid_run_incarnation(value):
        raise AssignmentIdentityError(
            "assignment requires a nonempty original run record identity (printable ASCII, at most 256 bytes)"
        )
    return value


def receipt_matches_assignment(receipt: dict, run_id: str, run_incarnation_id: str) -> bool:
    """Legacy history is readable, but cannot prove replay of the same public ID."""
    require_run_incarnation(run_incarnation_id)
    if receipt.get("run_id") != run_id:
        return False
    original = receipt.get("run_incarnation_id")
    if original is None:
        raise AssignmentIdentityError("committed receipt has ambiguous original run identity for this run ID")
    return require_run_incarnation(original) == run_incarnation_id


def native_turn_id(run_id: str, run_incarnation_id: str) -> str:
    """Stable SDK invocation identity across reclaim, distinct after ID reuse."""
    proof = require_run_incarnation(run_incarnation_id)
    framed = b"runkite/native-turn/v1\x00"
    for value in (run_id, proof):
        data = value.encode("utf-8")
        framed += len(data).to_bytes(8, "big") + data
    return "rk-" + hashlib.sha256(framed).hexdigest()


def receipt_native_turn_id(receipt: dict) -> str:
    proof = receipt.get("run_incarnation_id")
    return receipt["run_id"] if proof is None else native_turn_id(receipt["run_id"], proof)
