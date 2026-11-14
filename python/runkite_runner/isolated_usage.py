"""Validated cumulative workload claims and their durable run-bound receipts.

An acknowledged report is retained before the child receives usage_ack. It is
not a provider bill or a mechanism for enforcing hostile-workload spend caps.
"""

from __future__ import annotations

import asyncio
import json
import math
from urllib.parse import quote

import httpx

from .completion import CompletionDeliveryError
from .tenant_ctx import runner_headers
from .tls_utils import httpx_tls_kwargs

MAX_TOKENS = (1 << 53) - 1
FIELDS = {"schema_version", "sequence", "status", "complete", "prompt_tokens", "completion_tokens", "cost_usd", "model"}


def validate_report(value):
    if (
        not isinstance(value, dict)
        or set(value) - FIELDS
        or not {"schema_version", "sequence", "status", "complete"} <= set(value)
    ):
        raise ValueError("invalid cumulative usage report")
    if (
        type(value["schema_version"]) is not int
        or value["schema_version"] != 1
        or type(value["sequence"]) is not int
        or not 1 <= value["sequence"] <= 1_000_000
        or type(value["complete"]) is not bool
    ):
        raise ValueError("invalid cumulative usage report")
    if value["status"] == "unknown":
        if value["complete"] or set(value) - {"schema_version", "sequence", "status", "complete"}:
            raise ValueError("unknown usage cannot claim known values")
        return dict(value)
    if value["status"] != "reported":
        raise ValueError("invalid usage reporting status")
    for key in ("prompt_tokens", "completion_tokens"):
        if type(value.get(key)) is not int or not 0 <= value[key] <= MAX_TOKENS:
            raise ValueError("usage tokens must be nonnegative safe integers")
    if value["prompt_tokens"] + value["completion_tokens"] > MAX_TOKENS:
        raise ValueError("usage token sum exceeds limit")
    if "cost_usd" in value:
        cost = value["cost_usd"]
        if type(cost) not in (int, float) or not math.isfinite(cost) or not 0 <= cost <= 1e12:
            raise ValueError("invalid reported usage cost")
    model = value.get("model", "")
    if not isinstance(model, str) or len(model.encode()) > 256 or any(char in model for char in "\x00\r\n"):
        raise ValueError("invalid usage model")
    normalized = json.loads(json.dumps(value, allow_nan=False))
    if not model:
        normalized.pop("model", None)
    return normalized


def output_usage(report, generation, workload_owner=None):
    result = {
        "reporting_status": report["status"],
        "reporting_source": "isolated_workload",
        "complete": report["complete"],
        "sequence": report["sequence"],
        "reported_generation": generation,
        "unmetered": report["status"] != "reported" or not report["complete"],
    }
    if workload_owner is not None:
        result["reported_workload"] = json.loads(json.dumps(workload_owner))
    if report["status"] == "reported":
        for key in ("prompt_tokens", "completion_tokens", "model", "cost_usd"):
            if key in report:
                result[key] = report[key]
        result["total_tokens"] = report["prompt_tokens"] + report["completion_tokens"]
        if "cost_usd" in report:
            result["cost_reported"] = True
    return result


class RunUsageClient:
    def __init__(self, base_url, runner_kind, runner_token, *, transport=None):
        self.base = base_url.rstrip("/")
        self.kind, self.token = runner_kind, runner_token
        self.client = httpx.AsyncClient(timeout=10, transport=transport, **httpx_tls_kwargs())

    async def aclose(self):
        await self.client.aclose()

    async def get(self, run_id):
        response = await self.client.get(
            self.base + "/internal/runs/" + quote(run_id, safe="") + "/usage",
            headers=runner_headers(self.token, self.kind),
        )
        response.raise_for_status()
        return response.json()["snapshot"]

    async def report(self, run_id, report):
        # The sequence identifies this mutation, so a lost HTTP acknowledgement
        # can safely retry the exact body without issuing model/tool work again.
        for attempt in range(3):
            try:
                response = await self.client.post(
                    self.base + "/internal/runs/" + quote(run_id, safe="") + "/usage",
                    headers=runner_headers(self.token, self.kind),
                    json=report,
                )
                response.raise_for_status()
                return response.json()["snapshot"]
            except (httpx.TimeoutException, httpx.TransportError):
                if attempt == 2:
                    raise
                await asyncio.sleep(0.1 * 2**attempt)


class UsageSession:
    def __init__(self, client, run_id):
        self.client, self.run_id = client, run_id
        self.snapshot = None
        self.workload_sequence = 0
        self.last_frame = None

    def accept_snapshot(self, value):
        if (
            not isinstance(value, dict)
            or value.get("source") != "isolated_workload"
            or type(value.get("reported_generation")) is not int
        ):
            raise CompletionDeliveryError("invalid durable usage acknowledgement")
        report = validate_report({key: field for key, field in value.items() if key in FIELDS})
        self.snapshot = {**report, "source": "isolated_workload", "reported_generation": value["reported_generation"]}
        if value.get("workload_owner") is not None:
            owner = value["workload_owner"]
            if (
                not isinstance(owner, dict)
                or set(owner) != {"uri", "key_sha256", "not_after"}
                or not all(isinstance(item, str) and item for item in owner.values())
            ):
                raise CompletionDeliveryError("invalid durable usage reporter identity")
            self.snapshot["workload_owner"] = json.loads(json.dumps(owner))

    async def start(self):
        value = await self.client.get(self.run_id)
        if value is None:
            value = await self.client.report(
                self.run_id, {"schema_version": 1, "sequence": 1, "status": "unknown", "complete": False}
            )
        self.accept_snapshot(value)

    async def receive(self, frame):
        if set(frame) - (FIELDS | {"type"}) or frame.get("type") != "usage":
            raise ValueError("invalid usage frame")
        value = validate_report({key: field for key, field in frame.items() if key != "type"})
        sequence = value["sequence"]
        if sequence == self.workload_sequence and value == self.last_frame:
            return sequence
        if sequence != self.workload_sequence + 1 or self.snapshot["complete"]:
            raise ValueError("workload usage sequence is not current")
        report = {**value, "sequence": self.snapshot["sequence"] + 1}
        acknowledged = await self.client.report(self.run_id, report)
        self.accept_snapshot(acknowledged)
        if self.report() != report:
            raise CompletionDeliveryError("durable usage acknowledgement changed the report")
        self.workload_sequence, self.last_frame = sequence, value
        return sequence

    def report(self):
        return {key: value for key, value in self.snapshot.items() if key in FIELDS}

    def output(self, value, *, legacy=False):
        if "usage" in value and not legacy:
            raise ValueError("output.usage is reserved; send a versioned usage frame")
        return {
            **value,
            "usage": output_usage(
                self.report(), self.snapshot["reported_generation"], self.snapshot.get("workload_owner")
            ),
        }
