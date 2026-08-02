"""Portable identity helpers for custom-route ASGI apps.

The control plane authenticates the caller, then injects X-Runkite-*
headers on the reverse-proxied request (see internal/customroutes).
Custom apps SHOULD trust these headers rather than re-parsing JWT /
API keys — and MUST NOT trust them on any path that bypasses the
control plane.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Mapping


HEADER_IDENTITY = "x-runkite-identity"
HEADER_TENANT_ID = "x-runkite-tenant-id"
HEADER_PERMISSIONS = "x-runkite-permissions"
HEADER_DISPLAY_NAME = "x-runkite-display-name"
HEADER_USER_JSON = "x-runkite-user"


@dataclass(frozen=True)
class CustomUser:
    """Identity forwarded by the control plane into a custom route."""

    identity: str
    tenant_id: str = "default"
    permissions: tuple[str, ...] = ()
    display_name: str = ""
    extra: dict[str, Any] = field(default_factory=dict)

    @property
    def is_authenticated(self) -> bool:
        return bool(self.identity)


def _header(headers: Mapping[str, str], name: str) -> str:
    # ASGI / Starlette headers are lower-case; also accept canonical form.
    if hasattr(headers, "get"):
        v = headers.get(name) or headers.get(name.title()) or headers.get(name.upper())
        if v is not None:
            return v if isinstance(v, str) else str(v)
    return ""


def user_from_headers(headers: Mapping[str, str]) -> CustomUser | None:
    """Build a CustomUser from X-Runkite-* headers. Returns None if absent."""
    raw = _header(headers, HEADER_USER_JSON)
    if raw:
        try:
            data = json.loads(raw)
            if isinstance(data, dict) and data.get("identity"):
                perms = data.get("permissions") or []
                if isinstance(perms, str):
                    perms = [p for p in perms.split(",") if p]
                return CustomUser(
                    identity=str(data["identity"]),
                    tenant_id=str(data.get("tenant_id") or "default"),
                    permissions=tuple(str(p) for p in perms),
                    display_name=str(data.get("display_name") or ""),
                )
        except json.JSONDecodeError:
            pass

    identity = _header(headers, HEADER_IDENTITY)
    if not identity:
        return None
    perms_raw = _header(headers, HEADER_PERMISSIONS)
    perms = tuple(p for p in (perms_raw.split(",") if perms_raw else []) if p)
    return CustomUser(
        identity=identity,
        tenant_id=_header(headers, HEADER_TENANT_ID) or "default",
        permissions=perms,
        display_name=_header(headers, HEADER_DISPLAY_NAME),
    )


def user_from_request(request: Any) -> CustomUser | None:
    """Starlette/FastAPI convenience: user_from_headers(request.headers)."""
    return user_from_headers(request.headers)
