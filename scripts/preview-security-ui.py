#!/usr/bin/env python3
"""Preview the actual built Admin UI with synthetic read-only responses.

Run: python scripts/preview-security-ui.py --port 8786
Open: http://127.0.0.1:8786/

This developer fixture binds only 127.0.0.1. It runs no agents, authenticates
no credentials and never proxies to a backend. All mutations are refused.
"""

from __future__ import annotations

import argparse
import html
import json
import mimetypes
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
TENANT = "demo.example.org"
CREATED = "2026-11-14T09:00:00Z"
UPDATED = "2026-11-14T09:00:15Z"
LEASE_REF = "cred-" + "a" * 56
BANNER_TEXT = "Synthetic UI fixture, no agent execution"
NOTICE = f'<aside id="fixture-notice" role="note"><strong>{BANNER_TEXT}</strong><a href="/">Read-only cases</a></aside>'
PREVIEW_STYLE = """<style>
html { --fixture-bar:56px; }
body { margin:0; padding-top:var(--fixture-bar); }
#fixture-notice { position:fixed; inset:0 0 auto; z-index:2147483647;
  box-sizing:border-box; height:var(--fixture-bar); display:flex;
  align-items:center; justify-content:space-between; gap:12px; padding:8px 16px;
  background:#422006; color:#fef3c7; border-bottom:2px solid #fbbf24;
  font:13px/1.3 system-ui,sans-serif; }
#fixture-notice a { color:#fef3c7; white-space:nowrap; text-decoration:underline; }
#root .h-screen { height:calc(100dvh - var(--fixture-bar)); }
</style>"""


def identity() -> dict:
    return {
        "issuer": "https://identity.example.org",
        "subject": "analyst@example.org",
        "audience": "runkite-demo",
        "expires_at": "2026-11-14T10:00:00Z",
        "issuer_asserted_actors": [{"subject": "assistant-service", "issuer": "https://identity.example.org"}],
    }


def workload() -> dict:
    return {
        "uri": "spiffe://example.org/runners/reporting",
        "key_sha256": "b" * 64,
        "not_after": "2026-11-14T10:00:00Z",
    }


def run_fixture(variant: str) -> dict:
    status = {"partial": "interrupted", "unknown": "error", "zero": "success"}[variant]
    usage = {
        "reporting_source": "isolated_workload",
        "reporting_status": "unknown" if variant == "unknown" else "reported",
        "complete": variant == "zero",
        "sequence": 1 if variant == "unknown" else 2,
        "reported_generation": 2,
        "reported_workload": workload(),
        "unmetered": variant != "zero",
    }
    if variant != "unknown":
        usage.update(
            prompt_tokens=12 if variant == "partial" else 0,
            completion_tokens=4 if variant == "partial" else 0,
            total_tokens=16 if variant == "partial" else 0,
            model="configured-demo-model",
            cost_usd=0.0012 if variant == "partial" else 0,
            cost_reported=True,
        )
    return {
        "incarnation_id": {"partial": "1", "unknown": "2", "zero": "3"}[variant] * 32,
        "tenant_id": TENANT,
        "run_id": "demo-" + variant,
        "thread_id": "demo-thread-" + variant,
        "agent_id": "demo-reporting-agent",
        "status": status,
        "created_at": CREATED,
        "updated_at": UPDATED,
        "metadata": {
            "run_manifest": {
                "schema_version": 1,
                "captured_at": CREATED,
                "tenant_id": TENANT,
                "agent_id": "demo-reporting-agent",
                "agent_version": 3,
                "runner_kind": "python-isolated",
                "connector_needs": ["demo-database", "demo-documents"],
                "allowed_tools": ["read_report"],
                "policy_fail_closed": True,
                "principal": {
                    "identity": "analyst@example.org",
                    "verified_identity": True,
                    "permissions": ["read"],
                    "identity_provenance": identity(),
                },
            }
        },
        "input": {"prompt": "Synthetic preview only. No model was called."},
        "output": {"answer": "Synthetic fixture. No agent ran.", "usage": usage},
    }


def action_fixture(kind: str, run: dict) -> dict:
    action = {
        "action_id": "demo-" + kind + "-action",
        "tenant_id": TENANT,
        "run_id": run["run_id"],
        "run_created_at": CREATED,
        "run_incarnation_id": run["incarnation_id"],
        "thread_id": run["thread_id"],
        "generation": 2,
        "agent_id": "demo-reporting-agent",
        "agent_version": 3,
        "principal": "analyst@example.org",
        "connector": "demo-database" if kind == "vault" else "demo-documents",
        "tool": "read_report",
        "transport": "json",
        "args_digest": "sha256:" + "c" * 64,
        "state": "outcome_unknown" if kind == "unknown" else "response_observed",
        "version": 4,
        "created_at": CREATED,
        "updated_at": UPDATED,
        "decision": "allow",
        "reason_code": "allowed",
        "rule_id": "demo-read-only-rule",
        "authorization_source": "policy",
        "policy_revision": "policy-v1-" + "d" * 64,
        "target_revision": "target-v3-" + "e" * 64,
        "tool_revision": None,
        "trace_id": "f" * 32,
        "dispatch_id": "demo-" + kind + "-dispatch",
        "dispatch_deadline_at": "2026-11-14T09:00:30Z",
        "identity_provenance": identity(),
        "workload_identity": workload(),
    }
    if kind != "unknown":
        action.update(
            response_kind="jsonrpc_result",
            http_status=200,
            response_digest="sha256:" + "f" * 64,
            response_bytes=84,
        )
    if kind == "vault":
        action.update(
            credential_ref=LEASE_REF,
            vault_issue_request_id="d67cffc1-35dd-4c91-ae9b-a956775537ea",
        )
    elif kind == "delegated":
        action["delegation_contract"] = {
            "audiences": ["reports-api"],
            "resources": ["https://reports.example.org"],
            "scopes": ["reports.read"],
            "configured_actor": "demo-reporting-service",
            "max_ttl_seconds": 120,
        }
    return action


def action_detail(action: dict) -> dict:
    events = []
    for sequence, state in enumerate(["received", "ready", "dispatch_intent", action["state"]], start=1):
        event = {
            "action_id": action["action_id"],
            "tenant_id": TENANT,
            "sequence": sequence,
            "mutation_id": f"demo-mutation-{sequence}",
            "kind": ["received", "decision", "dispatch_intent", "observation"][sequence - 1],
            "observed_at": CREATED if sequence == 1 else UPDATED,
            "observer": "control_plane",
            "state": state,
        }
        if sequence >= 2:
            event.update(decision="allow", authorization_source="policy")
        if sequence == 4 and action.get("http_status") is not None:
            event.update(http_status=200, response_kind=action["response_kind"])
        if sequence >= 3 and action.get("vault_issue_request_id"):
            event["vault_issue_request_id"] = action["vault_issue_request_id"]
        events.append(event)
    return {"action": action, "events": events, "run_available": True}


def fixtures() -> tuple[dict, dict, dict]:
    runs = {row["run_id"]: row for row in map(run_fixture, ("partial", "unknown", "zero"))}
    actions = {}
    for kind, variant in (
        ("vault", "partial"),
        ("unknown", "unknown"),
        ("delegated", "zero"),
    ):
        action = action_fixture(kind, runs["demo-" + variant])
        actions[action["action_id"]] = action_detail(action)
    leases = {
        LEASE_REF: {
            "lease_ref": LEASE_REF,
            "owner": {
                "tenant_id": TENANT,
                "run_id": "demo-partial",
                "run_incarnation_id": runs["demo-partial"]["incarnation_id"],
                "generation": 2,
                "action_id": "demo-vault-action",
                "consumer_id": "demo-database",
            },
            "binding_id": "demo-readonly-database",
            "binding_revision": "demo-vault-role-revision-1",
            "state": "revoked",
            "version": 5,
            "created_at": CREATED,
            "updated_at": UPDATED,
            "operation_deadline": "2026-11-14T09:00:30Z",
            "expires_at": "2026-11-14T09:02:00Z",
            "granted_ttl_seconds": 120,
            "renewable": False,
            "use_started_at": "2026-11-14T09:00:05Z",
            "cleanup_requested": True,
            "reason_code": "provider_confirmed_revoked",
            "vault_issue_request_id": "d67cffc1-35dd-4c91-ae9b-a956775537ea",
            "vault_last_renew_request_id": "c68fb300-3aae-4417-a85b-541d8d77723a",
        }
    }
    return runs, actions, leases


@dataclass
class Response:
    status: int
    body: bytes
    content_type: str = "application/json"
    headers: dict[str, str] = field(default_factory=dict)


def json_response(value: object, status: int = 200) -> Response:
    return Response(status, json.dumps(value, allow_nan=False).encode())


class Preview:
    def __init__(self, dist: Path):
        self.dist = dist.resolve()
        self.runs, self.actions, self.leases = fixtures()

    def catalog(self) -> bytes:
        links = [
            ("/admin/connector-actions/demo-delegated-action", "Delegated identity and registered exchange"),
            ("/admin/connector-actions/demo-vault-action", "Vault request correlation"),
            ("/admin/connector-actions/demo-unknown-action", "Unknown connector outcome"),
            (f"/admin/credential-leases/{LEASE_REF}?tenant_id={TENANT}", "Provider-confirmed credential cleanup"),
            *[
                (f"/admin/runs/demo-{variant}", f"Isolated usage: {variant}")
                for variant in ("partial", "unknown", "zero")
            ],
        ]
        items = "".join(
            f'<li><a href="{html.escape(url, quote=True)}">{html.escape(label)}</a></li>' for url, label in links
        )
        return (
            "<!doctype html><html><head><meta charset=utf-8>"
            f"<title>{BANNER_TEXT}</title><style>body{{font:17px/1.6 system-ui;"
            "max-width:850px;margin:48px auto;padding:16px;background:#111;color:#eee}"
            "a{color:#fbbf24}li{margin:14px 0}</style></head><body>"
            f"<h1>{BANNER_TEXT}</h1>"
            "<p>Read-only development preview of the actual built Admin UI. "
            "Every record and identifier below is synthetic. No agent, Vault, "
            "issuer, database, queue or production service is connected.</p>"
            "<p>Delegation and Vault use separate connector fixtures. "
            "HTTP 200 means a synthetic response observation, not a business outcome. "
            "All mutation requests are refused.</p>"
            f"<ul>{items}</ul></body></html>"
        ).encode()

    def response(self, method: str, target: str) -> Response:
        if method not in {"GET", "HEAD"}:
            result = json_response({"message": "Synthetic read-only fixture: mutations are disabled."}, 405)
            result.headers["Allow"] = "GET, HEAD"
            return result
        if len(target) > 8192:
            return json_response({"message": "Fixture URL exceeds limit."}, 414)
        try:
            parsed = urlsplit(target)
            path = unquote(parsed.path, errors="strict")
            query = parse_qs(parsed.query, keep_blank_values=True, max_num_fields=20)
        except (ValueError, UnicodeError):
            return json_response({"message": "Invalid fixture URL."}, 400)
        if (
            parsed.scheme
            or parsed.netloc
            or any(segment in {".", ".."} for segment in path.split("/"))
            or any(char in path for char in ("\\", "\0", "%"))
        ):
            return json_response({"message": "Invalid fixture path."}, 400)
        if path == "/":
            return Response(200, self.catalog(), "text/html; charset=utf-8")
        if path.startswith("/admin-api/"):
            return self.api(path.removeprefix("/admin-api"), query)
        if path == "/admin" or path.startswith("/admin/"):
            relative = path.removeprefix("/admin/") if path != "/admin" else ""
            if relative.startswith("assets/") or relative in {"favicon.svg", "logo.svg"}:
                candidate = (self.dist / relative).resolve()
                if not candidate.is_relative_to(self.dist) or not candidate.is_file():
                    return json_response({"message": "Built fixture asset not found."}, 404)
                content_type = mimetypes.guess_type(candidate.name)[0] or "application/octet-stream"
                return Response(200, candidate.read_bytes(), content_type)
            if "." in relative.rsplit("/", 1)[-1]:
                return json_response({"message": "Fixture asset not found."}, 404)
            entry = self.dist / "index.html"
            if not entry.is_file():
                return json_response({"message": "Build admin-ui before starting this preview."}, 503)
            text = entry.read_text().replace("</head>", PREVIEW_STYLE + "</head>")
            text = text.replace("<body>", "<body>" + NOTICE)
            return Response(200, text.encode(), "text/html; charset=utf-8")
        return json_response({"message": "No synthetic fixture exists for this route."}, 404)

    def api(self, path: str, query: dict[str, list[str]]) -> Response:
        if path == "/session":
            return json_response({"authenticated": True, "auth_required": False, "identity": "synthetic-preview"})
        if path == "/overview":
            return json_response(
                {
                    "total_agents": 1,
                    "total_threads": 3,
                    "threads_by_status": {"idle": 2, "interrupted": 1},
                    "total_runs": 3,
                    "runs_by_status": {"success": 1, "error": 1, "interrupted": 1},
                    "connector_count": 2,
                    "cron_schedule_count": 0,
                }
            )
        if path == "/runs":
            rows = list(self.runs.values())
            if "parent_run_id" in query:
                rows = []
            return json_response(rows)
        if path.startswith("/runs/"):
            parts = path.split("/")
            run = self.runs.get(parts[2])
            if run and len(parts) == 4 and parts[3] == "stream":
                payload = json.dumps({"status": run["status"], "fixture": True})
                return Response(200, f"event: end\ndata: {payload}\n\n".encode(), "text/event-stream")
            if run and len(parts) == 3:
                return json_response(run)
        if path == "/connector-actions":
            rows = [detail["action"] for detail in self.actions.values()]
            for name in ("tenant_id", "run_id", "connector", "state"):
                if query.get(name):
                    rows = [row for row in rows if row.get(name) == query[name][0]]
            return json_response(rows)
        if path.startswith("/connector-actions/"):
            detail = self.actions.get(path.removeprefix("/connector-actions/"))
            if detail:
                return json_response(detail)
        if path == "/credential-leases":
            return json_response(list(self.leases.values()) if query.get("tenant_id") == [TENANT] else [])
        if path.startswith("/credential-leases/"):
            record = self.leases.get(path.removeprefix("/credential-leases/"))
            if record and query.get("tenant_id") == [TENANT]:
                return json_response(record)
        if path == "/pending-actions":
            return json_response([])
        return json_response({"message": "No synthetic API fixture exists for this request."}, 404)


def make_handler(preview: Preview) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        def respond(self):
            host = self.headers.get("Host", "")
            port = self.server.server_port
            if host not in {f"127.0.0.1:{port}", f"localhost:{port}"}:
                response = json_response({"message": "Loopback fixture host required."}, 421)
            elif self.headers.get("Authorization") or self.headers.get("Cookie"):
                response = json_response({"message": "Do not send credentials to this synthetic fixture."}, 400)
            else:
                response = preview.response(self.command, self.path)
            self.send_response(response.status)
            self.send_header("Content-Type", response.content_type)
            self.send_header("Content-Length", str(len(response.body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "no-referrer")
            self.send_header(
                "Content-Security-Policy",
                "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
            )
            for name, value in response.headers.items():
                self.send_header(name, value)
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(response.body)

        do_GET = do_HEAD = do_POST = do_PUT = do_PATCH = do_DELETE = do_OPTIONS = do_TRACE = respond

        def log_message(self, _format, *args):
            # This fixture never records headers, bodies, credentials or URLs.
            pass

    return Handler


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8786, help="Loopback port, default 8786.")
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("--port must be between 1 and 65535")
    preview = Preview(ROOT / "internal/adminui/dist")
    if not (preview.dist / "index.html").is_file():
        parser.error("Build the actual Admin UI first: cd admin-ui && npm run build")
    with ThreadingHTTPServer(("127.0.0.1", args.port), make_handler(preview)) as server:
        print(f"{BANNER_TEXT}. Read-only: http://127.0.0.1:{args.port}/", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
