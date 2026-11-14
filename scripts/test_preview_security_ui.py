"""Exercise synthetic preview routing without opening a network listener."""

import importlib.util
import io
import json
import sys
import tempfile
import unittest
from email.message import Message
from pathlib import Path
from types import SimpleNamespace

SPEC = importlib.util.spec_from_file_location("preview_security_ui", Path(__file__).with_name("preview-security-ui.py"))
preview = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = preview
SPEC.loader.exec_module(preview)


class PreviewTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dist = Path(self.temp.name) / "dist"
        self.dist.mkdir()
        self.entry = b'<html><head><script src="/admin/assets/actual.js"></script></head><body><div id="root"></div></body></html>'
        (self.dist / "index.html").write_bytes(self.entry)
        (self.dist / "assets").mkdir()
        (self.dist / "assets/actual.js").write_bytes(b"/* actual build fixture */")
        self.app = preview.Preview(self.dist)

    def test_serves_built_bundle_unchanged_and_labels_every_spa_route(self):
        asset = self.app.response("GET", "/admin/assets/actual.js")
        self.assertEqual(asset.status, 200)
        self.assertEqual(asset.body, b"/* actual build fixture */")
        for target in (
            "/admin",
            "/admin/connector-actions/demo-delegated-action",
            f"/admin/credential-leases/{preview.LEASE_REF}?tenant_id={preview.TENANT}",
            "/admin/runs/demo-partial",
        ):
            with self.subTest(target=target):
                result = self.app.response("GET", target)
                self.assertEqual(result.status, 200)
                self.assertIn(preview.BANNER_TEXT.encode(), result.body)
                self.assertIn(b"/admin/assets/actual.js", result.body)
        self.assertEqual((self.dist / "index.html").read_bytes(), self.entry)

    def test_refuses_all_mutations_and_unknown_api_routes(self):
        for method in ("POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE"):
            for path in ("/admin-api/session", "/admin-api/runs/demo-zero/cancel", "/"):
                result = self.app.response(method, path)
                self.assertEqual(result.status, 405)
                self.assertEqual(result.headers["Allow"], "GET, HEAD")
        result = self.app.response("GET", "/admin-api/production-proxy")
        self.assertEqual(result.status, 404)

    def test_filesystem_escape_and_malformed_urls_are_refused(self):
        outside = Path(self.temp.name) / "outside.txt"
        outside.write_text("must not be served")
        (self.dist / "assets/escape.js").symlink_to(outside)
        for target in (
            "/admin/assets/../index.html",
            "/admin/assets/%2e%2e/index.html",
            "/admin/assets/%252e%252e/outside.txt",
            "/admin/assets/%00private",
            "/admin/assets/%ff",
            "https://example.org/admin",
            "//example.org/admin",
            "/admin/assets/escape.js",
        ):
            with self.subTest(target=target):
                result = self.app.response("GET", target)
                self.assertIn(result.status, (400, 404))
                self.assertNotIn(b"must not be served", result.body)
        self.assertEqual(self.app.response("GET", "/" + "x" * 8193).status, 414)

    def test_usage_fixtures_preserve_unknown_partial_and_measured_zero(self):
        rows = {}
        for variant in ("partial", "unknown", "zero"):
            response = self.app.response("GET", "/admin-api/runs/demo-" + variant)
            self.assertEqual(response.status, 200)
            rows[variant] = json.loads(response.body)["output"]["usage"]
        self.assertNotIn("prompt_tokens", rows["unknown"])
        self.assertTrue(rows["unknown"]["unmetered"])
        self.assertFalse(rows["partial"]["complete"])
        self.assertTrue(rows["partial"]["unmetered"])
        self.assertEqual(rows["partial"]["prompt_tokens"], 12)
        self.assertTrue(rows["zero"]["complete"])
        self.assertFalse(rows["zero"]["unmetered"])
        self.assertEqual(rows["zero"]["prompt_tokens"], 0)
        self.assertEqual(rows["zero"]["cost_usd"], 0)
        stream = self.app.response("GET", "/admin-api/runs/demo-partial/stream")
        self.assertEqual(stream.content_type, "text/event-stream")
        self.assertIn(b"event: end", stream.body)
        self.assertIn(b'"status": "interrupted"', stream.body)

    def test_distinct_delegation_and_vault_fixtures_have_coherent_references(self):
        delegated = json.loads(self.app.response("GET", "/admin-api/connector-actions/demo-delegated-action").body)
        vault = json.loads(self.app.response("GET", "/admin-api/connector-actions/demo-vault-action").body)
        self.assertIn("delegation_contract", delegated["action"])
        self.assertNotIn("credential_ref", delegated["action"])
        self.assertNotIn("delegation_contract", vault["action"])
        lease = json.loads(
            self.app.response(
                "GET",
                f"/admin-api/credential-leases/{preview.LEASE_REF}?tenant_id={preview.TENANT}",
            ).body
        )
        self.assertEqual(vault["action"]["credential_ref"], lease["lease_ref"])
        self.assertEqual(vault["action"]["action_id"], lease["owner"]["action_id"])
        self.assertEqual(vault["action"]["run_incarnation_id"], lease["owner"]["run_incarnation_id"])
        self.assertEqual(vault["action"]["vault_issue_request_id"], lease["vault_issue_request_id"])
        self.assertEqual([row["sequence"] for row in vault["events"]], [1, 2, 3, 4])
        self.assertEqual(lease["state"], "revoked")
        self.assertEqual(
            self.app.response("GET", f"/admin-api/credential-leases/{preview.LEASE_REF}?tenant_id=foreign").status,
            404,
        )
        self.assertEqual(
            json.loads(self.app.response("GET", "/admin-api/connector-actions?tenant_id=foreign").body),
            [],
        )

    def test_fixture_metadata_contains_no_credential_or_provider_handle_fields(self):
        forbidden = {
            "password",
            "username",
            "token",
            "sso_token",
            "access_token",
            "protected_handle",
            "lease_id",
            "_runkite_delegated_jwt",
        }

        def inspect(value):
            if isinstance(value, dict):
                self.assertFalse(forbidden.intersection(value))
                for child in value.values():
                    inspect(child)
            elif isinstance(value, list):
                for child in value:
                    inspect(child)

        inspect(preview.fixtures())
        for collection in preview.fixtures():
            inspect(collection)
        session = json.loads(self.app.response("GET", "/admin-api/session").body)
        self.assertFalse(session["auth_required"])
        self.assertNotIn("csrf_token", session)

    def test_http_boundary_refuses_foreign_hosts_and_credentials_without_binding(self):
        handler_type = preview.make_handler(self.app)
        for headers, expected in (
            ({"Host": "127.0.0.1:8786"}, 200),
            ({"Host": "evil.example.org:8786"}, 421),
            ({"Host": "127.0.0.1:8786", "Authorization": "Bearer synthetic"}, 400),
            ({"Host": "127.0.0.1:8786", "Cookie": "synthetic=1"}, 400),
        ):
            handler = object.__new__(handler_type)
            handler.server = SimpleNamespace(server_port=8786)
            handler.command, handler.path = "GET", "/admin-api/session"
            handler.headers = Message()
            for name, value in headers.items():
                handler.headers[name] = value
            response_headers = {}
            statuses = []
            handler.send_response = statuses.append
            handler.send_header = response_headers.__setitem__
            handler.end_headers = lambda: None
            handler.wfile = io.BytesIO()
            handler.respond()
            self.assertEqual(statuses, [expected])
            self.assertEqual(response_headers["Cache-Control"], "no-store")
            self.assertIn("connect-src 'self'", response_headers["Content-Security-Policy"])


if __name__ == "__main__":
    unittest.main()
