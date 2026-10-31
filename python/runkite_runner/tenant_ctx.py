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

Also owns the historical LangGraph checkpointer thread-id encoding. Native
savers reject ambiguous tenant/thread combinations before accessing this key.
Proxy checkpoints resolve the bare thread through the plane instead.
"""

from __future__ import annotations

import base64
import os
from contextvars import ContextVar, Token
from dataclasses import dataclass

_DEFAULT = "default"
_active: ContextVar[str] = ContextVar("runkite_tenant", default=_DEFAULT)


@dataclass(frozen=True)
class _RunBinding:
    run_id: str
    generation: int


_run: ContextVar[_RunBinding | None] = ContextVar("runkite_run", default=None)
_runner: ContextVar[tuple[str, str | None]] = ContextVar("runkite_runner", default=("python-langgraph", None))

# Header runners send on /internal/* so proxy-mode handlers scope the
# same way as client auth (see internal/auth.HeaderTenantID).
HEADER_TENANT_ID = "X-Runkite-Tenant-Id"
HEADER_TENANT_ID_B64 = "X-Runkite-Tenant-Id-B64"
HEADER_RUN_ID = "X-Runkite-Run-Id"
HEADER_RUN_ID_B64 = "X-Runkite-Run-Id-B64"
HEADER_GENERATION = "X-Runkite-Generation"


def current_tenant() -> str:
    tid = _active.get()
    return tid if tid else _DEFAULT


def bind_tenant(tenant_id: str | None) -> Token[str]:
    tid = tenant_id or _DEFAULT
    return _active.set(tid)


def reset_tenant(token: Token[str]) -> None:
    _active.reset(token)


def bind_run(run_id: str | None, generation: int | None) -> Token[_RunBinding | None]:
    rid = run_id or ""
    if not rid:
        return _run.set(None)
    gen = int(generation or 0)
    return _run.set(_RunBinding(run_id=rid, generation=gen))


def reset_run(token: Token[_RunBinding | None]) -> None:
    _run.reset(token)


def tenant_headers() -> dict[str, str]:
    """Extra headers for proxy-mode HTTP calls to the control plane."""
    headers = tenant_id_headers(current_tenant())
    binding = _run.get()
    if binding is not None:
        headers.update(run_id_headers(binding.run_id))
        headers[HEADER_GENERATION] = str(binding.generation)
    return headers


def run_id_headers(run_id: str) -> dict[str, str]:
    """Retain exact run identity through HTTP's ASCII/whitespace boundary."""
    return _identity_headers(run_id, HEADER_RUN_ID, HEADER_RUN_ID_B64)


def tenant_id_headers(tenant_id: str) -> dict[str, str]:
    """A nonempty tenant is opaque; only absence selects the default tenant."""
    return _identity_headers(tenant_id or _DEFAULT, HEADER_TENANT_ID, HEADER_TENANT_ID_B64)


def _identity_headers(value: str, raw_header: str, encoded_header: str) -> dict[str, str]:
    if value == value.strip() and all(32 <= ord(char) < 127 for char in value):
        return {raw_header: value}
    encoded = base64.urlsafe_b64encode(value.encode("utf-8")).decode("ascii").rstrip("=")
    return {encoded_header: encoded}


def bind_runner(kind: str, token: str | None = None) -> Token[tuple[str, str | None]]:
    """Bind the authenticated worker identity for task-local HTTP helpers."""
    return _runner.set((kind, token or None))


def reset_runner(token: Token[tuple[str, str | None]]) -> None:
    _runner.reset(token)


def runner_headers(runner_token: str | None = None, runner_kind: str | None = None) -> dict[str, str]:
    """Run/tenant headers plus the current worker's kind-scoped credential.

    Explicit constructor options take precedence. Outside a worker binding,
    the historical LangGraph kind and RUNNER_TOKEN environment remain valid.
    """
    bound_kind, bound_token = _runner.get()
    token = runner_token if runner_token is not None else bound_token
    if token is None:
        token = os.environ.get("RUNNER_TOKEN")
    headers = tenant_headers()
    if token:
        headers["X-Runner-Kind"] = runner_kind or bound_kind
        headers["X-Runner-Token"] = token
    return headers


def checkpoint_thread_id(tenant_id: str | None, thread_id: str) -> str:
    """LangGraph checkpointer key for this assignment's logical thread.

    Empty / "default" tenants keep the bare thread_id so pre-tenancy and
    single-tenant direct-mode checkpoint rows stay reachable. Any other
    tenant gets "{tenant_id}:{thread_id}". Native execution must first validate
    this delimiter format; proxy mode uses the bare plane thread instead.
    """
    tid = tenant_id or _DEFAULT
    if tid == _DEFAULT:
        return thread_id
    return f"{tid}:{thread_id}"


def validate_native_checkpoint_identity(tenant_id: str | None, thread_id: str, *, mode: str) -> None:
    """Refuse identities that alias another tenant in native saver keys.

    Persistent named-tenant keys may contain another legacy owner's state.
    Fresh memory savers can use the disjoint delimiter domain instead. Proxy
    checkpoints use exact plane ownership and do not need this restriction.
    """
    tenant = tenant_id or _DEFAULT
    if mode not in {"direct-postgres", "memory"}:
        raise ValueError("native checkpoint identity validation requires a native saver mode")
    if mode == "direct-postgres" and tenant != _DEFAULT:
        raise ValueError(
            "ambiguous native LangGraph checkpoint ownership for named tenants in direct-postgres mode; "
            "use proxy checkpoint mode after explicitly resolving legacy native history. "
            "An existing tenant-prefix key cannot prove its original owner; no state is imported automatically"
        )
    if (
        tenant != tenant.strip()
        or any(ord(char) < 32 or ord(char) == 127 for char in tenant)
        or (tenant == _DEFAULT and ":" in thread_id)
        or (tenant != _DEFAULT and ":" in tenant)
    ):
        raise ValueError(
            "ambiguous native LangGraph checkpoint identity; native savers require delimiter-free tenant IDs "
            "and default-tenant thread IDs without ':'. Use proxy checkpoint mode after explicitly resolving "
            "any existing native history; no state is imported automatically"
        )


def storage_thread_id(config_thread_id: str) -> str:
    """Bare threads.thread_id for /internal/checkpoints HTTP paths.

    Inverse of checkpoint_thread_id for the active tenant: direct
    AsyncPostgresSaver needs the prefixed configurable.thread_id (no
    tenant column on LangGraph's tables), but proxy mode stores under the
    real threads.thread_id PK and run-binding compares against that bare
    id. Stripping here keeps both modes correct.
    """
    tenant = current_tenant()
    if tenant and tenant != _DEFAULT:
        prefix = f"{tenant}:"
        if config_thread_id.startswith(prefix):
            return config_thread_id[len(prefix) :]
    return config_thread_id
