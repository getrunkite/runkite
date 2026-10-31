"""Unit tests for the pre-exec status check (PROTOCOL §10.3)."""

from __future__ import annotations

import asyncio
import base64
import unittest
from unittest.mock import AsyncMock, MagicMock, patch
from urllib.parse import quote

from runkite_runner.run_status import should_skip_run


def _run(coro):
    return asyncio.run(coro)


def _patch_get(status_code: int, body: dict | None = None):
    """Replace httpx.AsyncClient so GET returns a fixed response."""
    resp = MagicMock()
    resp.status_code = status_code
    resp.json.return_value = body or {}

    client = MagicMock()
    client.get = AsyncMock(return_value=resp)
    client.__aenter__ = AsyncMock(return_value=client)
    client.__aexit__ = AsyncMock(return_value=None)

    return patch("runkite_runner.run_status.httpx.AsyncClient", return_value=client)


class TestShouldSkipRun(unittest.TestCase):
    def test_skip_interrupted(self):
        with _patch_get(200, {"status": "interrupted"}):
            self.assertTrue(_run(should_skip_run("http://cp:2026", "run-1")))

    def test_proceed_pending(self):
        with _patch_get(200, {"status": "pending"}):
            self.assertFalse(_run(should_skip_run("http://cp:2026", "run-1")))

    def test_fail_open_on_http_error(self):
        with _patch_get(503, {}):
            self.assertFalse(_run(should_skip_run("http://cp:2026", "run-1")))

    def test_skip_404(self):
        with _patch_get(404, {}):
            self.assertTrue(_run(should_skip_run("http://cp:2026", "run-1")))

    def test_sends_tenant_header(self):
        resp = MagicMock()
        resp.status_code = 200
        resp.json.return_value = {"status": "pending"}
        client = MagicMock()
        client.get = AsyncMock(return_value=resp)
        client.__aenter__ = AsyncMock(return_value=client)
        client.__aexit__ = AsyncMock(return_value=None)
        with patch("runkite_runner.run_status.httpx.AsyncClient", return_value=client):
            self.assertFalse(_run(should_skip_run("http://cp:2026", "run-1", tenant_id="acme")))
        kwargs = client.get.await_args.kwargs
        self.assertEqual(kwargs["headers"].get("X-Runkite-Tenant-Id"), "acme")

    def test_custom_run_id_is_one_path_component(self):
        with _patch_get(200, {"status": "running"}) as factory:
            run_id = "run/child?status=success#fragment%tail"
            self.assertFalse(_run(should_skip_run("http://cp:2026/", run_id)))
            client = factory.return_value
            self.assertEqual(
                client.get.await_args.args[0],
                f"http://cp:2026/internal/runs/{quote(run_id, safe='')}/status",
            )

    def test_unicode_tenant_claim_uses_the_encoded_header(self):
        with _patch_get(200, {"status": "running"}) as factory:
            tenant = "tenant-ré-雪"
            self.assertFalse(_run(should_skip_run("http://cp:2026", "run", tenant_id=tenant)))
            headers = factory.return_value.get.await_args.kwargs["headers"]
            self.assertNotIn("X-Runkite-Tenant-Id", headers)
            encoded = headers["X-Runkite-Tenant-Id-B64"]
            self.assertEqual(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)).decode(), tenant)


if __name__ == "__main__":
    unittest.main()
