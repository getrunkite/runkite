"""Fake bank MCP: JSON-RPC tools/list + tools/call for refund.

Nothing moves money. The control plane holds refunds at or over 100
before this process is contacted. Bind 0.0.0.0 so Compose can reach it.
"""

from __future__ import annotations

import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


PORT = int(os.environ.get("PORT", "8091"))


def rpc_result(req_id, result):
    return {"jsonrpc": "2.0", "id": req_id, "result": result}


def handle_rpc(body: dict) -> dict:
    req_id = body.get("id")
    method = body.get("method") or ""
    params = body.get("params") if isinstance(body.get("params"), dict) else {}
    if method == "tools/list":
        return rpc_result(
            req_id,
            {
                "tools": [
                    {
                        "name": "refund",
                        "description": "Fake refund. No money moves.",
                        "inputSchema": {
                            "type": "object",
                            "properties": {
                                "amount": {"type": "number"},
                                "to": {"type": "string"},
                            },
                        },
                    }
                ]
            },
        )
    if method == "tools/call":
        name = params.get("name") or ""
        args = params.get("arguments") if isinstance(params.get("arguments"), dict) else {}
        if name != "refund":
            return {
                "jsonrpc": "2.0",
                "id": req_id,
                "error": {"code": -32601, "message": f"unknown tool {name!r}"},
            }
        amount = args.get("amount")
        dest = args.get("to") or "vendor"
        text = f"fake bank: refunded {amount} to {dest} (nothing moved)"
        return rpc_result(
            req_id,
            {"content": [{"type": "text", "text": text}]},
        )
    return rpc_result(req_id, {"protocolVersion": "2024-11-05"})


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        return

    def _send(self, code: int, payload: bytes, content_type: str) -> None:
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        if self.path.split("?", 1)[0] == "/health":
            self._send(200, b"ok", "text/plain")
            return
        self._send(404, b"not found", "text/plain")

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            body = json.loads(raw.decode("utf-8") or "{}")
        except json.JSONDecodeError:
            self._send(400, b'{"error":"invalid json"}', "application/json")
            return
        if not isinstance(body, dict):
            self._send(400, b'{"error":"object required"}', "application/json")
            return
        out = json.dumps(handle_rpc(body)).encode("utf-8")
        self._send(200, out, "application/json")


def main() -> None:
    httpd = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"pending_agent fake MCP on 0.0.0.0:{PORT}", flush=True)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
