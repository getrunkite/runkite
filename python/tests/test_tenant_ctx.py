"""Self-check for tenant_ctx.py's per-run ContextVar binding.

Proves:
1. Default tenant is "default" (and the proxy header matches).
2. bind_tenant / reset_tenant round-trip restores the prior value.
3. Empty / whitespace binds as "default".
4. Concurrent asyncio tasks each see their own bound tenant -- a
   ContextVar leak would clobber shared-runner multi-tenant jobs.

Usage:
    python/.venv/bin/python python/tests/test_tenant_ctx.py
"""

from __future__ import annotations

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.tenant_ctx import (  # noqa: E402
    HEADER_GENERATION,
    HEADER_RUN_ID,
    HEADER_TENANT_ID,
    bind_run,
    bind_tenant,
    checkpoint_thread_id,
    current_tenant,
    reset_run,
    reset_tenant,
    tenant_headers,
)


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_default_tenant():
    check("default tenant is 'default'", current_tenant() == "default")
    check(
        "default proxy header is 'default'",
        tenant_headers()[HEADER_TENANT_ID] == "default",
    )


def test_bind_and_reset():
    token = bind_tenant("acme")
    try:
        check("bound tenant is 'acme'", current_tenant() == "acme")
        check(
            "bound proxy header is 'acme'",
            tenant_headers()[HEADER_TENANT_ID] == "acme",
        )
    finally:
        reset_tenant(token)
    check("reset restores 'default'", current_tenant() == "default")


def test_empty_binds_default():
    token = bind_tenant("  ")
    try:
        check("whitespace binds as 'default'", current_tenant() == "default")
    finally:
        reset_tenant(token)


async def _concurrent_tenants() -> list[str]:
    seen: list[str] = []

    async def job(tid: str):
        token = bind_tenant(tid)
        try:
            await asyncio.sleep(0.01)
            seen.append(current_tenant())
        finally:
            reset_tenant(token)

    await asyncio.gather(job("a"), job("b"), job("c"))
    return sorted(seen)


def test_concurrent_jobs_do_not_clobber():
    check(
        "concurrent jobs keep distinct tenants",
        asyncio.run(_concurrent_tenants()) == ["a", "b", "c"],
    )


def test_checkpoint_thread_id_encoding():
    check("absent tenant → bare", checkpoint_thread_id(None, "t1") == "t1")
    check("default tenant → bare", checkpoint_thread_id("default", "t1") == "t1")
    check("whitespace tenant → bare", checkpoint_thread_id("  ", "t1") == "t1")
    check("acme tenant → prefixed", checkpoint_thread_id("acme", "t1") == "acme:t1")


def test_run_binding_headers():
    check("no run headers by default", HEADER_RUN_ID not in tenant_headers())
    token = bind_run("run-1", 3)
    try:
        h = tenant_headers()
        check("run id header set", h.get(HEADER_RUN_ID) == "run-1")
        check("generation header set", h.get(HEADER_GENERATION) == "3")
    finally:
        reset_run(token)
    check("run headers cleared after reset", HEADER_RUN_ID not in tenant_headers())


def main():
    test_default_tenant()
    test_bind_and_reset()
    test_empty_binds_default()
    test_concurrent_jobs_do_not_clobber()
    test_checkpoint_thread_id_encoding()
    test_run_binding_headers()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
