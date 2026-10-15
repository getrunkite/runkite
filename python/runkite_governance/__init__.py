"""Calls POST /governance/decide, then polls GET /governance/actions/{id}
when the effect is pending. The app executes the tool after allow.
Runkite does not execute the tool and does not consume the approval
on poll. This is not the deferred client SDK in docs/client-sdk.md.

Stdlib only. Import with PYTHONPATH=python. Same behavior as
typescript/runkite-governance.
"""

from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

DEFAULT_INTERVAL_MS = 500
DEFAULT_TIMEOUT_MS = 60_000

__all__ = ["DEFAULT_INTERVAL_MS", "DEFAULT_TIMEOUT_MS", "GovernanceClient", "GovernanceError"]


class GovernanceError(Exception):
    def __init__(self, message: str, status: int) -> None:
        super().__init__(message)
        self.status = status


class GovernanceClient:
    def __init__(
        self,
        base_url: str,
        *,
        api_key: str | None = None,
        interval_ms: int = DEFAULT_INTERVAL_MS,
        timeout_ms: int = DEFAULT_TIMEOUT_MS,
    ) -> None:
        self._base = base_url.rstrip("/")
        self._api_key = api_key
        self._interval_ms = interval_ms
        self._timeout_ms = timeout_ms

    def decide(
        self,
        *,
        agent_id: str,
        connector: str,
        tool: str,
        args: dict[str, Any] | None = None,
        run_id: str = "",
        tenant_id: str | None = None,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {
            "agent_id": agent_id,
            "connector": connector,
            "tool": tool,
            "args": {} if args is None else args,
            "run_id": run_id,
        }
        if tenant_id:
            body["tenant_id"] = tenant_id
        return _as_decision(self._request("POST", "/governance/decide", body))

    def action(self, action_id: str) -> dict[str, Any]:
        path = "/governance/actions/" + urllib.parse.quote(action_id, safe="")
        return _as_action(self._request("GET", path))

    def resolve(
        self,
        *,
        agent_id: str,
        connector: str,
        tool: str,
        args: dict[str, Any] | None = None,
        run_id: str = "",
        tenant_id: str | None = None,
        interval_ms: int | None = None,
        timeout_ms: int | None = None,
        cancelled: threading.Event | None = None,
    ) -> dict[str, Any]:
        """Poll until the row settles. Set cancelled from another thread to stop, including during the wait."""
        _raise_if_cancelled(cancelled)
        decision = self.decide(
            agent_id=agent_id,
            connector=connector,
            tool=tool,
            args=args,
            run_id=run_id,
            tenant_id=tenant_id,
        )
        if decision["effect"] != "pending":
            return _compact(
                {
                    "effect": decision["effect"],
                    "reason": decision.get("reason"),
                    "reason_code": decision.get("reason_code"),
                }
            )
        action_id = decision.get("action_id")
        if not isinstance(action_id, str) or not action_id:
            raise GovernanceError("pending decision missing action_id", 502)
        every = self._interval_ms if interval_ms is None else interval_ms
        limit = self._timeout_ms if timeout_ms is None else timeout_ms
        deadline = time.monotonic() + limit / 1000
        while True:
            _raise_if_cancelled(cancelled)
            action = self.action(action_id)
            if action["status"] != "pending":
                return _outcome(action)
            if time.monotonic() >= deadline:
                raise GovernanceError("governance poll timed out", 408)
            if cancelled is not None and cancelled.wait(every / 1000):
                raise GovernanceError("governance poll aborted", 499)
            if cancelled is None:
                time.sleep(every / 1000)

    def _request(self, method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self._api_key:
            headers["Authorization"] = "Bearer " + self._api_key
        req = urllib.request.Request(self._base + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as exc:
            raise GovernanceError(_message(exc.read(), exc.reason or ""), exc.code) from exc
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return None


def _as_decision(raw: Any) -> dict[str, Any]:
    row = _object(raw)
    effect = row.get("effect")
    if effect not in ("allow", "deny", "pending"):
        raise GovernanceError("governance decide response missing effect", 502)
    return _compact(
        {
            "effect": effect,
            "reason": _optional_str(row.get("reason")),
            "reason_code": _optional_str(row.get("reason_code")),
            "action_id": _optional_str(row.get("action_id")),
        }
    )


def _as_action(raw: Any) -> dict[str, Any]:
    row = _object(raw)
    status = row.get("status")
    if status not in ("pending", "approved", "rejected", "consumed"):
        raise GovernanceError("governance action response missing status", 502)
    action_id = _optional_str(row.get("action_id"))
    if not action_id:
        raise GovernanceError("governance action response missing action_id", 502)
    return _compact(
        {
            "action_id": action_id,
            "status": status,
            "reason": _optional_str(row.get("reason")),
            "reason_code": _optional_str(row.get("reason_code")),
        }
    )


def _outcome(action: dict[str, Any]) -> dict[str, Any]:
    return _compact(
        {
            "effect": "allow" if action["status"] == "approved" else "deny",
            "reason": action.get("reason"),
            "reason_code": action.get("reason_code"),
            "action_id": action["action_id"],
            "status": action["status"],
        }
    )


def _object(raw: Any) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise GovernanceError("governance response was not JSON", 502)
    return raw


def _optional_str(value: Any) -> str | None:
    if isinstance(value, str) and value != "":
        return value
    return None


def _compact(row: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in row.items() if value is not None}


def _raise_if_cancelled(cancelled: threading.Event | None) -> None:
    if cancelled is not None and cancelled.is_set():
        raise GovernanceError("governance poll aborted", 499)


def _message(raw: bytes, fallback: str) -> str:
    text = raw.decode("utf-8", errors="replace").strip()
    if not text:
        return fallback or "governance request failed"
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        return text
    if isinstance(parsed, dict):
        message = parsed.get("message")
        if isinstance(message, str) and message:
            return message
    return text
