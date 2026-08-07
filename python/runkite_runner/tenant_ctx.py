"""Per-run tenant + run-binding for direct-mode store/vector SQL and proxy headers.

The runner process holds one shared store (and optional vector store)
across concurrent jobs. Multi-tenant isolation requires the active
tenant to follow the job's RunAssignment.tenant_id for the duration of
that job -- a process-wide constant of "default" is wrong. ContextVar
is task-local across await points, so concurrent jobs do not clobber
each other.

Proxy-mode /internal/store|vectors|connectors/{session,mcp} also require
X-Runkite-Run-Id + X-Runkite-Generation so the control plane can derive
tenant/agent from the in-flight assignment instead of trusting the
tenant header alone.

Also owns the LangGraph checkpointer thread-id encoding: non-default
tenants prefix configurable.thread_id so AsyncPostgresSaver rows do not
collide when two tenants reuse the same client-chosen thread id.
"""

from __future__ import annotations

from contextvars import ContextVar, Token
from dataclasses import dataclass

_DEFAULT = "default"
_active: ContextVar[str] = ContextVar("runkite_tenant", default=_DEFAULT)


@dataclass(frozen=True)
class _RunBinding:
    run_id: str
    generation: int


_run: ContextVar[_RunBinding | None] = ContextVar("runkite_run", default=None)

# Header runners send on /internal/* so proxy-mode handlers scope the
# same way as client auth (see internal/auth.HeaderTenantID).
HEADER_TENANT_ID = "X-Runkite-Tenant-Id"
HEADER_RUN_ID = "X-Runkite-Run-Id"
HEADER_GENERATION = "X-Runkite-Generation"


def current_tenant() -> str:
    tid = _active.get()
    return tid if tid else _DEFAULT


def bind_tenant(tenant_id: str | None) -> Token[str]:
    tid = (tenant_id or "").strip() or _DEFAULT
    return _active.set(tid)


def reset_tenant(token: Token[str]) -> None:
    _active.reset(token)


def bind_run(run_id: str | None, generation: int | None) -> Token[_RunBinding | None]:
    rid = (run_id or "").strip()
    if not rid:
        return _run.set(None)
    gen = int(generation or 0)
    return _run.set(_RunBinding(run_id=rid, generation=gen))


def reset_run(token: Token[_RunBinding | None]) -> None:
    _run.reset(token)


def tenant_headers() -> dict[str, str]:
    """Extra headers for proxy-mode HTTP calls to the control plane."""
    headers = {HEADER_TENANT_ID: current_tenant()}
    binding = _run.get()
    if binding is not None:
        headers[HEADER_RUN_ID] = binding.run_id
        headers[HEADER_GENERATION] = str(binding.generation)
    return headers


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
