#!/usr/bin/env python3
"""Tiny static UI server with per-port agent lock + CORS-free same-origin assets."""

from __future__ import annotations

import argparse
import json
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

UI_DIR = Path(__file__).resolve().parent
QA_MATRIX = UI_DIR.parent / "qa-matrix.json"


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, agent_id: str = "", cp: str = "", hub: bool = False, **kwargs):
        self._agent_id = agent_id
        self._cp = cp
        self._hub = hub
        super().__init__(*args, directory=str(UI_DIR), **kwargs)

    def do_GET(self):  # noqa: N802
        path = self.path.split("?", 1)[0]
        if path in ("/", "/index.html", "/index.html/") and self._hub:
            self.path = "/hub.html"
            path = "/hub.html"
        if path in ("/qa-matrix.json", "/qa-matrix.json/"):
            body = QA_MATRIX.read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)
            return
        if path in ("/config.js", "/config.js/"):
            body = (
                "window.DOGFOOD = "
                + json.dumps({"controlPlaneUrl": self._cp, "agentId": self._agent_id})
                + ";\n"
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/javascript; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)
            return
        return super().do_GET()

    def log_message(self, fmt, *args):
        # Quiet — start.sh already prints URLs.
        pass


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--agent", default="")
    p.add_argument("--cp", default="http://127.0.0.1:2026")
    p.add_argument("--hub", action="store_true", help="Serve QA hub at /")
    args = p.parse_args()

    def factory(*a, **k):
        return Handler(*a, agent_id=args.agent, cp=args.cp, hub=args.hub, **k)

    httpd = ThreadingHTTPServer(("127.0.0.1", args.port), factory)
    print(f"ui :{args.port} agent={args.agent or '*'} cp={args.cp}", flush=True)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
