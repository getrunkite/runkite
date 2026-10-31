"""Self-check for runkite_governance: decide, then poll.

Hits a real local HTTP server, not a mocked client.

Usage:
    PYTHONPATH=python python3 python/tests/test_governance.py
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_governance import GovernanceClient, GovernanceError  # noqa: E402

CALL = {
    "agent_id": "partner",
    "connector": "workspace",
    "tool": "uploadFile",
    "args": {"name": "a.txt"},
    "run_id": "turn-1",
}


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class _Handler(BaseHTTPRequestHandler):
    def _hit(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b""
        body = json.loads(raw) if raw else None
        self.server.hits.append(
            {
                "method": self.command,
                "path": self.path,
                "body": body,
                "auth": self.headers.get("Authorization", ""),
            }
        )

    def do_POST(self):
        self._hit()
        self.server.respond(self)

    def do_GET(self):
        self._hit()
        self.server.respond(self)

    def log_message(self, fmt, *args):
        return


def serve(respond):
    server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    server.hits = []
    server.respond = respond
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_address[1]}"
    return server, url


def stop(server):
    server.shutdown()
    server.server_close()


def reply(handler, status, body):
    raw = json.dumps(body).encode()
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(raw)))
    handler.end_headers()
    handler.wfile.write(raw)


def test_decide_allow():
    def respond(handler):
        reply(handler, 200, {"effect": "allow", "reason": "grant matched", "reason_code": "grant"})

    server, url = serve(respond)
    try:
        client = GovernanceClient(url, api_key="rk_test")
        decision = client.decide(**CALL)
        check(
            "allow decision",
            decision == {"effect": "allow", "reason": "grant matched", "reason_code": "grant"},
        )
        check("one request", len(server.hits) == 1)
        hit = server.hits[0]
        check("posts decide", hit["method"] == "POST" and hit["path"] == "/governance/decide")
        check("bearer", hit["auth"] == "Bearer rk_test")
        check(
            "body",
            hit["body"]
            == {
                "agent_id": "partner",
                "connector": "workspace",
                "tool": "uploadFile",
                "args": {"name": "a.txt"},
                "run_id": "turn-1",
            },
        )
    finally:
        stop(server)


def test_resolve_approved():
    polls = {"n": 0}

    def respond(handler):
        if handler.path == "/governance/decide":
            reply(
                handler,
                200,
                {
                    "effect": "pending",
                    "reason": "hold",
                    "reason_code": "mandatory_hitl",
                    "action_id": "pending-1",
                },
            )
            return
        polls["n"] += 1
        if polls["n"] == 1:
            reply(
                handler,
                200,
                {"action_id": "pending-1", "status": "pending", "reason": "hold", "reason_code": "mandatory_hitl"},
            )
            return
        reply(handler, 200, {"action_id": "pending-1", "status": "approved", "reason": "ok", "reason_code": "approved"})

    server, url = serve(respond)
    try:
        client = GovernanceClient(url + "/", interval_ms=5, timeout_ms=1000)
        outcome = client.resolve(**CALL)
        check(
            "approved is allow",
            outcome
            == {
                "effect": "allow",
                "reason": "ok",
                "reason_code": "approved",
                "action_id": "pending-1",
                "status": "approved",
            },
        )
        gets = [hit for hit in server.hits if hit["method"] == "GET"]
        check("two polls", len(gets) == 2)
    finally:
        stop(server)


def test_rejected_and_consumed():
    def respond(handler):
        if handler.path == "/governance/decide":
            action_id = "pending-no" if len(handler.server.hits) == 1 else "pending-used"
            reply(handler, 200, {"effect": "pending", "action_id": action_id})
            return
        if handler.path == "/governance/actions/pending-no":
            reply(handler, 200, {"action_id": "pending-no", "status": "rejected", "reason_code": "pending_expired"})
            return
        reply(handler, 200, {"action_id": "pending-used", "status": "consumed"})

    server, url = serve(respond)
    try:
        client = GovernanceClient(url, interval_ms=5, timeout_ms=1000)
        rejected = client.resolve(**CALL)
        check("rejected is deny", rejected["effect"] == "deny" and rejected["status"] == "rejected")
        check("expired reason", rejected["reason_code"] == "pending_expired")
        consumed = client.resolve(**CALL)
        check("consumed is deny", consumed["effect"] == "deny" and consumed["status"] == "consumed")
    finally:
        stop(server)


def test_timeout():
    def respond(handler):
        if handler.path == "/governance/decide":
            reply(handler, 200, {"effect": "pending", "action_id": "pending-wait"})
            return
        reply(handler, 200, {"action_id": "pending-wait", "status": "pending"})

    server, url = serve(respond)
    try:
        client = GovernanceClient(url, interval_ms=50, timeout_ms=20)
        try:
            client.resolve(**CALL)
            check("timeout raises", False)
        except GovernanceError as exc:
            check("timeout status", exc.status == 408 and str(exc) == "governance poll timed out")
    finally:
        stop(server)


def test_cancel_before_call():
    def respond(handler):
        reply(handler, 200, {"effect": "allow"})

    server, url = serve(respond)
    try:
        cancelled = threading.Event()
        cancelled.set()
        client = GovernanceClient(url)
        try:
            client.resolve(**CALL, cancelled=cancelled)
            check("cancel before call raises", False)
        except GovernanceError as exc:
            check("cancel before call", exc.status == 499 and str(exc) == "governance poll aborted")
        check("cancel before call makes no request", len(server.hits) == 0)
    finally:
        stop(server)


def test_cancel_during_poll():
    def respond(handler):
        if handler.path == "/governance/decide":
            reply(handler, 200, {"effect": "pending", "action_id": "pending-wait"})
            return
        reply(handler, 200, {"action_id": "pending-wait", "status": "pending"})

    server, url = serve(respond)
    cancelled = threading.Event()
    caught = {}

    def run():
        client = GovernanceClient(url, interval_ms=400, timeout_ms=10_000)
        try:
            client.resolve(**CALL, cancelled=cancelled)
        except GovernanceError as exc:
            caught["error"] = exc

    worker = threading.Thread(target=run)
    try:
        worker.start()
        deadline = time.time() + 2
        while time.time() < deadline and not any(hit["method"] == "GET" for hit in server.hits):
            time.sleep(0.01)
        check("poll started", any(hit["method"] == "GET" for hit in server.hits))
        cancelled.set()
        worker.join(2)
        check("cancel stopped the thread", not worker.is_alive())
        error = caught.get("error")
        check("cancel during poll", isinstance(error, GovernanceError) and error.status == 499)
        check("one poll", len([hit for hit in server.hits if hit["method"] == "GET"]) == 1)
    finally:
        cancelled.set()
        worker.join(2)
        stop(server)


def test_http_error():
    def respond(handler):
        reply(handler, 403, {"message": "governance:decide permission required"})

    server, url = serve(respond)
    try:
        client = GovernanceClient(url)
        try:
            client.decide(**CALL)
            check("403 raises", False)
        except GovernanceError as exc:
            check("403 message", exc.status == 403 and str(exc) == "governance:decide permission required")
    finally:
        stop(server)


def test_redirect_does_not_change_decision_authority():
    destination, target = serve(lambda handler: reply(handler, 200, {"effect": "allow"}))

    def redirect(handler):
        handler.send_response(302)
        handler.send_header("Location", target + "/fake-allow")
        handler.end_headers()

    origin, base = serve(redirect)
    try:
        client = GovernanceClient(base, api_key="redirect-test-key")
        try:
            client.decide(**CALL)
            check("redirect cannot return authorization", False)
        except GovernanceError as exc:
            check("redirect fails as HTTP error", exc.status == 302)
        check("configured plane receives one request", len(origin.hits) == 1)
        check("redirect target never receives bearer key", len(destination.hits) == 0)
    finally:
        stop(origin)
        stop(destination)


if __name__ == "__main__":
    test_redirect_does_not_change_decision_authority()
    test_decide_allow()
    test_resolve_approved()
    test_rejected_and_consumed()
    test_timeout()
    test_cancel_before_call()
    test_cancel_during_poll()
    test_http_error()
    print("ok")
