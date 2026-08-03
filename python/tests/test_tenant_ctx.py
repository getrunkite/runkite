"""Unit tests for per-run tenant ContextVar binding."""

from __future__ import annotations

import asyncio

from runkite_runner.tenant_ctx import (
    HEADER_TENANT_ID,
    bind_tenant,
    current_tenant,
    reset_tenant,
    tenant_headers,
)


def test_default_tenant():
    assert current_tenant() == "default"
    assert tenant_headers()[HEADER_TENANT_ID] == "default"


def test_bind_and_reset():
    token = bind_tenant("acme")
    try:
        assert current_tenant() == "acme"
        assert tenant_headers()[HEADER_TENANT_ID] == "acme"
    finally:
        reset_tenant(token)
    assert current_tenant() == "default"


def test_empty_binds_default():
    token = bind_tenant("  ")
    try:
        assert current_tenant() == "default"
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
    assert asyncio.run(_concurrent_tenants()) == ["a", "b", "c"]
