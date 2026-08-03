"""Unit tests for the pre-exec status check (PROTOCOL §10.3)."""

from __future__ import annotations

import asyncio
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

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


if __name__ == "__main__":
    unittest.main()
