#!/usr/bin/env python3
"""Prove the reference PDP implements the Runkite policy.webhook contract.

Usage:
  SECRET=dev-policy-secret python3 pdp.py &
  SECRET=dev-policy-secret python3 self_check.py
  # expect exit 0

Does not start the control plane — only the PDP HTTP surface + HMAC.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import sys
import urllib.error
import urllib.request

from pdp import decide, verify_signature

SECRET = os.environ.get("SECRET", "dev-policy-secret")
BASE = os.environ.get("PDP_URL", "http://127.0.0.1:8099").rstrip("/")


def sign(body: bytes) -> str:
    return "sha256=" + hmac.new(SECRET.encode(), body, hashlib.sha256).hexdigest()


def post(path: str, event: dict, *, bad_sig: bool = False) -> tuple[int, dict]:
    raw = json.dumps(event).encode()
    headers = {"Content-Type": "application/json"}
    if SECRET:
        headers["X-Runkite-Signature"] = "sha256=deadbeef" if bad_sig else sign(raw)
    req = urllib.request.Request(BASE + path, data=raw, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=2) as resp:
            return resp.status, json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        try:
            return e.code, json.loads(body)
        except json.JSONDecodeError:
            return e.code, {"raw": body}


def expect(cond: bool, msg: str) -> None:
    if not cond:
        print("FAIL:", msg, file=sys.stderr)
        sys.exit(1)
    print("ok:", msg)


def main() -> None:
    # Pure unit checks (no server)
    body = b'{"type":"policy.decide"}'
    expect(verify_signature("", body, None), "unsigned mode accepts missing header")
    expect(verify_signature(SECRET, body, sign(body)), "valid HMAC accepted")
    expect(not verify_signature(SECRET, body, "sha256=00"), "bad HMAC rejected")

    d = decide({"stage": "tool.call", "tool": "delete_repo"})
    expect(d.get("effect") == "deny", "delete_repo → deny")
    p = decide({"stage": "tool.call", "tool": "transfer_funds"})
    expect(p.get("effect") == "pending", "transfer_funds → pending")
    a = decide({"stage": "tool.call", "tool": "query"})
    expect(a.get("effect") == "allow", "query → allow")
    s = decide({"stage": "connector.session", "tool": ""})
    expect(s.get("effect") == "allow", "connector.session → allow")

    # Live HTTP against a running pdp.py
    try:
        urllib.request.urlopen(BASE + "/", timeout=1)
    except Exception as e:
        print(f"SKIP live HTTP (start pdp.py first): {e}", file=sys.stderr)
        print("unit checks passed")
        return

    code, out = post(
        "/decide",
        {
            "type": "policy.decide",
            "stage": "tool.call",
            "tenant_id": "acme",
            "agent_id": "ops",
            "connector": "github",
            "tool": "delete_repo",
            "timestamp": "2026-01-01T00:00:00Z",
        },
    )
    expect(code == 200 and out.get("effect") == "deny", f"HTTP deny delete_repo got {code} {out}")

    code, out = post(
        "/decide",
        {
            "type": "policy.decide",
            "stage": "tool.call",
            "tenant_id": "acme",
            "agent_id": "ops",
            "connector": "bank",
            "tool": "transfer_funds",
            "timestamp": "2026-01-01T00:00:00Z",
        },
    )
    expect(code == 200 and out.get("effect") == "pending", f"HTTP pending got {code} {out}")

    code, out = post(
        "/decide",
        {
            "type": "policy.decide",
            "stage": "tool.call",
            "tenant_id": "acme",
            "agent_id": "ops",
            "connector": "github",
            "tool": "get_repo",
            "timestamp": "2026-01-01T00:00:00Z",
        },
    )
    expect(code == 200 and out.get("effect") == "allow", f"HTTP allow got {code} {out}")

    if SECRET:
        code, out = post(
            "/decide",
            {"type": "policy.decide", "stage": "tool.call", "tool": "query"},
            bad_sig=True,
        )
        expect(code == 401, f"bad HMAC → 401 got {code}")

    print("all checks passed")


if __name__ == "__main__":
    main()
