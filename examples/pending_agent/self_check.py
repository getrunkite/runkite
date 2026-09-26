"""Local check for the fake MCP JSON-RPC surface (no control plane)."""

from __future__ import annotations

import json
import os
import sys
import threading
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "python"))

from mcp_server import Handler, ThreadingHTTPServer, handle_rpc


def check(name: str, cond: bool) -> None:
    print(f"[{'PASS' if cond else 'FAIL'}] {name}")
    if not cond:
        raise SystemExit(1)


def test_rpc_shapes() -> None:
    listed = handle_rpc({"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
    names = [t["name"] for t in listed["result"]["tools"]]
    check("tools/list has refund", names == ["refund"])
    called = handle_rpc(
        {
            "jsonrpc": "2.0",
            "id": 2,
            "method": "tools/call",
            "params": {"name": "refund", "arguments": {"amount": 500, "to": "vendor"}},
        }
    )
    text = called["result"]["content"][0]["text"]
    check("tools/call refund text", "500" in text and "vendor" in text)
    unknown = handle_rpc(
        {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "wire"}}
    )
    check("unknown tool errors", unknown.get("error", {}).get("code") == -32601)


def test_http_health() -> None:
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    port = httpd.server_address[1]
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2) as r:
            check("GET /health", r.status == 200 and r.read() == b"ok")
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/",
            data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=2) as r:
            body = json.loads(r.read().decode())
        check("POST tools/list", body["result"]["tools"][0]["name"] == "refund")
    finally:
        httpd.shutdown()


def test_amount_parse() -> None:
    from graph import State, _amount_from

    check("default 500", _amount_from({"messages": [{"content": "please refund"}]}) == 500.0)
    check("parses $500", _amount_from({"messages": [{"content": "Refund $500 to the vendor"}]}) == 500.0)
    check("parses 40", _amount_from({"messages": [{"content": "send 40"}]}) == 40.0)


if __name__ == "__main__":
    test_rpc_shapes()
    test_http_health()
    test_amount_parse()
    print("\nAll checks passed.")
