#!/usr/bin/env python3
"""Webhook + preflight sink for soak (host-side, reached via host.docker.internal)."""

from __future__ import annotations

import json
import sys
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9099
LOG = Path(sys.argv[2]) if len(sys.argv) > 2 else Path("/tmp/runkite-soak-webhooks.jsonl")

counts: dict[str, int] = {}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt: str, *args) -> None:
        return

    def _read_json(self) -> dict:
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        try:
            return json.loads(body) if body else {}
        except Exception:
            return {"raw": body.decode("utf-8", "replace")}

    def do_POST(self) -> None:
        ev = self._read_json()
        if self.path.startswith("/preflight"):
            counts["before_run"] = counts.get("before_run", 0) + 1
            if counts["before_run"] <= 3 or counts["before_run"] % 100 == 0:
                print(f"preflight allow total={counts['before_run']}", flush=True)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"allow":true}')
            return

        et = str(ev.get("type", "?"))
        counts[et] = counts.get(et, 0) + 1
        row = {
            "ts": datetime.now(timezone.utc).isoformat(),
            "path": self.path,
            "sig": self.headers.get("X-Runkite-Signature", ""),
            "event": ev,
        }
        with LOG.open("a") as f:
            f.write(json.dumps(row) + "\n")
        if counts[et] <= 3 or counts[et] % 50 == 0:
            print(f"webhook {et} total={counts[et]} run_id={ev.get('run_id', '')}", flush=True)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

    def do_GET(self) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"counts": counts}).encode())


if __name__ == "__main__":
    LOG.parent.mkdir(parents=True, exist_ok=True)
    LOG.write_text("")
    httpd = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"webhook+preflight sink on :{PORT} logging to {LOG}", flush=True)
    httpd.serve_forever()
