#!/usr/bin/env python3
"""Reference bring-your-own policy decision point (PDP) for Runkite.

Stdlib only. Implements the sync policy.webhook contract:
  POST /decide  JSON body type=policy.decide
  Optional HMAC: X-Runkite-Signature: sha256=<hex>

Env:
  PORT           listen port (default 8099)
  SECRET         shared HMAC secret (empty = unsigned; CP must omit secret too)
  DENY_TOOLS     comma-separated tools to hard-deny on tool.call (default: delete_repo)
  PENDING_TOOLS  comma-separated tools to return effect=pending (default: transfer_funds)

If a tool is in both lists, deny wins.

This is not a product — it proves: verify signature → decide → respond.
Swap the body of decide() for OPA, Cedar, or your internal ABAC service.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


PORT = int(os.environ.get("PORT", "8099"))
SECRET = os.environ.get("SECRET", "dev-policy-secret")
DENY_TOOLS = {
    t.strip()
    for t in os.environ.get("DENY_TOOLS", "delete_repo").split(",")
    if t.strip()
}
PENDING_TOOLS = {
    t.strip()
    for t in os.environ.get("PENDING_TOOLS", "transfer_funds").split(",")
    if t.strip()
}


def verify_signature(secret: str, body: bytes, header: str | None) -> bool:
    """Return True if secret is empty (unsigned mode) or HMAC matches."""
    if not secret:
        return True
    if not header or not header.startswith("sha256="):
        return False
    got = header.removeprefix("sha256=")
    want = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(got, want)


def decide(event: dict[str, Any]) -> dict[str, Any]:
    """Map one policy.decide request to an effect payload."""
    stage = str(event.get("stage") or "")
    tool = str(event.get("tool") or (event.get("data") or {}).get("tool") or "")

    if stage == "tool.call":
        if tool in DENY_TOOLS:
            return {
                "effect": "deny",
                "reason": f"tool {tool!r} blocked by reference PDP",
                "reason_code": "byo_deny_tool",
                "rule_id": f"deny-{tool}",
            }
        if tool in PENDING_TOOLS:
            return {
                "effect": "pending",
                "reason": f"tool {tool!r} requires operator approval",
                "reason_code": "policy_pending",
                "rule_id": f"hitl-{tool}",
            }

    # connector.session, run.create, unknown stages, or non-listed tools
    return {"effect": "allow", "reason": "reference PDP default allow", "rule_id": "byo-default"}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt: str, *args: object) -> None:
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def do_GET(self) -> None:
        body = json.dumps(
            {
                "ok": True,
                "service": "runkite-reference-pdp",
                "deny_tools": sorted(DENY_TOOLS),
                "pending_tools": sorted(PENDING_TOOLS),
                "signed": bool(SECRET),
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:
        if self.path.rstrip("/") not in ("/decide", ""):
            self.send_error(404, "use POST /decide")
            return
        n = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(n)
        if not verify_signature(SECRET, raw, self.headers.get("X-Runkite-Signature")):
            self._json(401, {"effect": "deny", "reason": "invalid signature", "reason_code": "byo_bad_hmac"})
            return
        try:
            event = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            self._json(400, {"effect": "deny", "reason": "invalid JSON", "reason_code": "byo_bad_json"})
            return
        if event.get("type") and event.get("type") != "policy.decide":
            self._json(400, {"effect": "deny", "reason": "expected type=policy.decide"})
            return
        out = decide(event)
        print(
            f"decide stage={event.get('stage')} tool={event.get('tool')} "
            f"effect={out.get('effect')} rule_id={out.get('rule_id')}",
            flush=True,
        )
        self._json(200, out)

    def _json(self, code: int, obj: dict[str, Any]) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main() -> None:
    httpd = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(
        f"reference PDP on http://127.0.0.1:{PORT}/decide "
        f"deny={sorted(DENY_TOOLS)} pending={sorted(PENDING_TOOLS)} "
        f"signed={bool(SECRET)}",
        flush=True,
    )
    httpd.serve_forever()


if __name__ == "__main__":
    main()
