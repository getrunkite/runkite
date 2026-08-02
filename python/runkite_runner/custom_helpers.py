"""Thin HTTP helpers for custom-route handlers talking back to the CP.

Custom apps are not in-process with the graph runtime. Use these to call
public Agent Protocol endpoints (store, threads, runs) with the caller's
Bearer token forwarded from the inbound request.
"""

from __future__ import annotations

from typing import Any
from urllib.parse import quote

import httpx


class ControlPlaneClient:
    """Minimal sync client for store/run/thread lookups from custom routes."""

    def __init__(self, base_url: str, authorization: str | None = None, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self._headers: dict[str, str] = {}
        if authorization:
            self._headers["Authorization"] = authorization
        self._timeout = timeout

    @classmethod
    def from_request(cls, request: Any, base_url: str | None = None) -> ControlPlaneClient:
        """Build a client that reuses the caller's Authorization header.

        base_url defaults to the Host the custom app was reached through
        (works when the app is reverse-proxied by the same CP). For
        sidecar mode, pass the control plane URL explicitly.
        """
        import os

        auth = None
        if hasattr(request, "headers"):
            auth = request.headers.get("authorization") or request.headers.get("Authorization")
        url = base_url or os.environ.get("RUNKITE_HTTP_URL") or os.environ.get("RUNKITE_URL")
        if not url and hasattr(request, "base_url"):
            # Starlette: request.base_url is the public origin as seen by
            # the app (often the CP origin when StripPrefix-proxied).
            url = str(request.base_url).rstrip("/")
        if not url:
            raise ValueError("control plane base_url required (pass base_url= or set RUNKITE_HTTP_URL)")
        return cls(url, authorization=auth)

    def _request(self, method: str, path: str, **kwargs: Any) -> Any:
        with httpx.Client(base_url=self.base_url, headers=self._headers, timeout=self._timeout) as client:
            resp = client.request(method, path, **kwargs)
            resp.raise_for_status()
            if resp.status_code == 204 or not resp.content:
                return None
            return resp.json()

    def get_thread(self, thread_id: str) -> dict:
        return self._request("GET", f"/threads/{quote(thread_id)}")

    def get_run(self, thread_id: str, run_id: str) -> dict:
        return self._request("GET", f"/threads/{quote(thread_id)}/runs/{quote(run_id)}")

    def store_get(self, namespace: list[str], key: str) -> dict | None:
        ns = ",".join(namespace)
        try:
            return self._request("GET", "/store/items", params={"namespace": ns, "key": key})
        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                return None
            raise

    def store_put(self, namespace: list[str], key: str, value: dict) -> dict:
        return self._request(
            "PUT",
            "/store/items",
            json={"namespace": namespace, "key": key, "value": value},
        )
