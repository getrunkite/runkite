"""Per-run tenant binding for direct-mode store/vector SQL and proxy headers.

The runner process holds one shared store (and optional vector store)
across concurrent jobs. Multi-tenant isolation requires the active
tenant to follow the job's RunAssignment.tenant_id for the duration of
that job -- a process-wide constant of "default" is wrong. ContextVar
is task-local across await points, so concurrent jobs do not clobber
each other.
"""

from __future__ import annotations

from contextvars import ContextVar, Token

_DEFAULT = "default"
_active: ContextVar[str] = ContextVar("runkite_tenant", default=_DEFAULT)

# Header runners send on /internal/* so proxy-mode handlers scope the
# same way as client auth (see internal/auth.HeaderTenantID).
HEADER_TENANT_ID = "X-Runkite-Tenant-Id"


def current_tenant() -> str:
    tid = _active.get()
    return tid if tid else _DEFAULT


def bind_tenant(tenant_id: str | None) -> Token[str]:
    tid = (tenant_id or "").strip() or _DEFAULT
    return _active.set(tid)


def reset_tenant(token: Token[str]) -> None:
    _active.reset(token)


def tenant_headers() -> dict[str, str]:
    """Extra headers for proxy-mode HTTP calls to the control plane."""
    return {HEADER_TENANT_ID: current_tenant()}
