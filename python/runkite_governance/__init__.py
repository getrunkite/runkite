"""Calls POST /governance/decide, then polls GET /governance/actions/{id}
when the effect is pending. The app executes the tool after allow.
Runkite does not execute the tool and does not consume the approval
on poll. This is not the deferred client SDK in docs/client-sdk.md.

Stdlib only. Import with PYTHONPATH=python. Same behavior as
typescript/runkite-governance.
"""

from __future__ import annotations

import json
import math
import queue
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

DEFAULT_INTERVAL_MS = 500
DEFAULT_TIMEOUT_MS = 60_000

# Bound abandoned resolver/socket operations as well as the caller wait.
_HTTP_WORKERS = threading.BoundedSemaphore(32)

__all__ = ["DEFAULT_INTERVAL_MS", "DEFAULT_TIMEOUT_MS", "GovernanceClient", "GovernanceError"]


class GovernanceError(Exception):
    def __init__(self, message: str, status: int) -> None:
        super().__init__(message)
        self.status = status


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # A decision must come from the configured plane. Do not forward its
        # bearer credential or accept authorization from a redirected endpoint.
        return None


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
        self._opener = urllib.request.build_opener(_NoRedirect())

    def decide(
        self,
        *,
        agent_id: str,
        connector: str,
        tool: str,
        args: dict[str, Any] | None = None,
        run_id: str = "",
        tenant_id: str | None = None,
        cancelled: threading.Event | None = None,
        timeout_ms: float | None = None,
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
        return _as_decision(
            self._request("POST", "/governance/decide", body, cancelled=cancelled, timeout_ms=timeout_ms)
        )

    def action(
        self, action_id: str, *, cancelled: threading.Event | None = None, timeout_ms: float | None = None
    ) -> dict[str, Any]:
        path = "/governance/actions/" + urllib.parse.quote(action_id, safe="")
        return _as_action(self._request("GET", path, cancelled=cancelled, timeout_ms=timeout_ms))

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
        limit = self._timeout_ms if timeout_ms is None else timeout_ms
        if not math.isfinite(limit) or limit <= 0:
            raise GovernanceError("governance poll timed out", 408)
        deadline = time.monotonic() + limit / 1000
        decision = self.decide(
            agent_id=agent_id,
            connector=connector,
            tool=tool,
            args=args,
            run_id=run_id,
            tenant_id=tenant_id,
            cancelled=cancelled,
            timeout_ms=_remaining(deadline, cancelled) * 1000,
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
        if not math.isfinite(every) or every < 0:
            raise GovernanceError("interval_ms must be non-negative", 400)
        while True:
            action = self.action(action_id, cancelled=cancelled, timeout_ms=_remaining(deadline, cancelled) * 1000)
            if action["status"] != "pending":
                return _outcome(action)
            pause = min(every / 1000, _remaining(deadline, cancelled))
            if cancelled is not None and cancelled.wait(pause):
                raise GovernanceError("governance poll aborted", 499)
            if cancelled is None:
                time.sleep(pause)

    def _request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        *,
        cancelled: threading.Event | None = None,
        timeout_ms: float | None = None,
    ) -> Any:
        """Bound the caller's entire request, including headers and response body.

        urllib is blocking. A daemon transport worker lets cancellation stop
        the caller immediately. Its socket deadline and chunk checks bound
        cleanup without replaying a decision that the plane may have received.
        """
        limit = self._timeout_ms if timeout_ms is None else timeout_ms
        if not math.isfinite(limit) or limit <= 0:
            raise GovernanceError("governance poll timed out", 408)
        deadline = time.monotonic() + limit / 1000
        _remaining(deadline, cancelled)
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self._api_key:
            headers["Authorization"] = "Bearer " + self._api_key
        req = urllib.request.Request(self._base + path, data=data, headers=headers, method=method)
        results: queue.Queue = queue.Queue(maxsize=1)
        workers = _HTTP_WORKERS

        def read_body(resp) -> bytes:
            chunks = []
            while True:
                _remaining(deadline, cancelled)
                chunk = resp.read1(64 * 1024)
                if not chunk:
                    return b"".join(chunks)
                chunks.append(chunk)

        def transport() -> None:
            try:
                with self._opener.open(req, timeout=_remaining(deadline, cancelled)) as resp:
                    if not 200 <= resp.status < 300:
                        raise GovernanceError(f"governance request failed: HTTP {resp.status}", resp.status)
                    raw = read_body(resp)
                results.put((True, json.loads(raw) if raw else None))
            except urllib.error.HTTPError as exc:
                # Error bodies are subject to the same outer wall-clock limit.
                try:
                    message = _message(read_body(exc), exc.reason or "")
                    results.put((False, GovernanceError(message, exc.code)))
                except Exception as error:
                    results.put((False, error))
                finally:
                    exc.close()
            except json.JSONDecodeError:
                results.put((True, None))
            except Exception as exc:
                results.put((False, exc))
            finally:
                workers.release()

        while not workers.acquire(timeout=min(_remaining(deadline, cancelled), 0.025)):
            pass
        try:
            _remaining(deadline, cancelled)
            threading.Thread(target=transport, daemon=True, name="runkite-governance-http").start()
        except BaseException:
            workers.release()
            raise
        while True:
            remaining = _remaining(deadline, cancelled)
            try:
                ok, value = results.get(timeout=min(remaining, 0.025))
            except queue.Empty:
                continue
            _remaining(deadline, cancelled)
            if ok:
                return value
            if (
                isinstance(value, (TimeoutError,))
                or isinstance(value, urllib.error.URLError)
                and isinstance(value.reason, TimeoutError)
            ):
                raise GovernanceError("governance poll timed out", 408) from value
            raise value


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


def _remaining(deadline: float, cancelled: threading.Event | None) -> float:
    _raise_if_cancelled(cancelled)
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise GovernanceError("governance poll timed out", 408)
    return remaining


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
