"""Per-run tenant binding for direct-mode store/vector SQL and proxy headers.

The runner process holds one shared store (and optional vector store)
across concurrent jobs. Multi-tenant isolation requires the active
tenant to follow the job's RunAssignment.tenant_id for the duration of
that job -- a process-wide constant of "default" is wrong. ContextVar
is task-local across await points, so concurrent jobs do not clobber
each other.

Also owns the LangGraph checkpointer thread-id encoding: non-default
tenants prefix configurable.thread_id so AsyncPostgresSaver rows do not
collide when two tenants reuse the same client-chosen thread id.
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


def checkpoint_thread_id(tenant_id: str | None, thread_id: str) -> str:
    """LangGraph checkpointer key for this assignment's logical thread.

    Empty / "default" tenants keep the bare thread_id so pre-tenancy and
    single-tenant direct-mode checkpoint rows stay reachable. Any other
    tenant gets "{tenant_id}:{thread_id}" so two tenants cannot share a
    checkpoint row when they reuse the same client-chosen thread id.
    """
    tid = (tenant_id or "").strip() or _DEFAULT
    if tid == _DEFAULT:
        return thread_id
    return f"{tid}:{thread_id}"
