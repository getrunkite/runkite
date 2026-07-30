"""Self-check for Store Dual Mode (store.py).

Proves the two acceptance properties that matter for a dual-mode store:
1. Namespace encoding matches the Go control plane's \\x1F-delimited scheme
   exactly (round-trip + prefix pattern), so direct-mode rows are never
   corrupted or misread relative to what proxy-mode/Go writes.
2. Direct mode (psycopg -> store_items) and proxy mode (HTTP -> control
   plane) are genuinely interoperable: a value written through one mode
   is immediately visible through the other. One store, not two.

Usage:
    RUNKITE_HTTP_URL=http://localhost:2099 \\
    POSTGRES_DSN=postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable \\
    python/.venv/bin/python python/tests/test_store_dual_mode.py

Requires a live control plane (proxy mode target) and Postgres (direct
mode target) pointed at the SAME database -- see Makefile's infra-up.
"""

import asyncio
import os
import sys
import uuid

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.store import (  # noqa: E402
    RunkiteStore,
    _ns_prefix_pattern,
    _ns_to_string,
    _string_to_ns,
)


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_namespace_encoding():
    ns = ("team-a", "users")
    encoded = _ns_to_string(ns)
    check("ns encoding uses \\x1F delimiter", encoded == "\x1fteam-a\x1fusers\x1f")
    check("ns round-trips", _string_to_ns(encoded) == ns)

    # The bug this delimiter fixes: "team-a" must never LIKE-prefix-match "team-abc".
    pattern = _ns_prefix_pattern(("team-a",))
    sibling = _ns_to_string(("team-abc", "x"))
    import fnmatch

    like_pattern = pattern.replace("%", "*")
    check("prefix pattern does not match sibling segment", not fnmatch.fnmatch(sibling, like_pattern))
    exact = _ns_to_string(("team-a", "x"))
    check("prefix pattern matches real child", fnmatch.fnmatch(exact, like_pattern))


async def test_dual_mode_interop(http_url: str, postgres_dsn: str):
    proxy = RunkiteStore(http_base_url=http_url)
    direct = RunkiteStore(postgres_dsn=postgres_dsn)
    check("proxy mode selected", proxy.mode == "proxy")
    check("direct mode selected", direct.mode == "direct")

    ns = ("interop-test", uuid.uuid4().hex[:8])
    key = "doc1"
    value = {"hello": "world", "n": 42}

    # Write via proxy (HTTP), read via direct (psycopg) -- proves direct
    # mode reads the exact same rows the Go server writes through.
    await proxy.aput(ns, key, value)
    item = await direct.aget(ns, key)
    check("direct mode reads proxy-mode write", item is not None and item.value == value)

    # Write via direct, read via proxy -- the reverse direction.
    value2 = {"hello": "again", "n": 43}
    await direct.aput(ns, key, value2)
    item2 = await proxy.aget(ns, key)
    check("proxy mode reads direct-mode write", item2 is not None and item2.value == value2)

    # Search via both modes should agree.
    results_proxy = await proxy.asearch(ns[:1])
    results_direct = await direct.asearch(ns[:1])
    check("search via proxy finds the item", any(r.key == key for r in results_proxy))
    check("search via direct finds the item", any(r.key == key for r in results_direct))

    # Delete via proxy, confirm gone via direct.
    await proxy.adelete(ns, key)
    check("direct mode sees proxy-mode delete", await direct.aget(ns, key) is None)

    # list_namespaces sees the namespace before cleanup runs its course.
    ns2 = ("interop-test", uuid.uuid4().hex[:8])
    await direct.aput(ns2, "k", {"v": 1})
    namespaces = await proxy.alist_namespaces(prefix=("interop-test",))
    check("list_namespaces via proxy includes direct-mode write", ns2 in namespaces)
    await direct.adelete(ns2, "k")

    await proxy.aclose()
    await direct.aclose()


async def test_ttl(http_url: str, postgres_dsn: str):
    """Regression test for a real bug: deepagents / LangGraph BaseStore
    called store.aput(..., ttl=...) and hard-failed with
    "TTL is not supported by RunkiteStore" because supports_ttl was never
    declared -- BaseStore.aput's own guard rejected the call before it
    reached batch/abatch. Verifies both proxy (HTTP) and direct (psycopg)
    modes, since they're two independent code paths that both needed the
    fix.
    """
    proxy = RunkiteStore(http_base_url=http_url)
    direct = RunkiteStore(postgres_dsn=postgres_dsn)
    check("supports_ttl is True", proxy.supports_ttl and direct.supports_ttl)

    for store, label in [(proxy, "proxy"), (direct, "direct")]:
        ns = ("ttl-test", uuid.uuid4().hex[:8])

        # ttl=<small> in minutes -- must not raise, and item must be
        # readable immediately after.
        await store.aput(ns, "expiring", {"v": 1}, ttl=0.02)  # 1.2s
        item = await store.aget(ns, "expiring")
        check(f"[{label}] item readable right after put with ttl", item is not None and item.value == {"v": 1})

        # After the ttl elapses, the item reads as absent -- same as a
        # never-existed key, no special "expired" error.
        await asyncio.sleep(1.5)
        item = await store.aget(ns, "expiring")
        check(f"[{label}] item absent after ttl elapses", item is None)

        # ttl=None means no expiration.
        await store.aput(ns, "permanent", {"v": 1}, ttl=None)
        await asyncio.sleep(1.5)
        item = await store.aget(ns, "permanent")
        check(f"[{label}] item with ttl=None never expires", item is not None)

        # refresh_ttl=True (the default) on a read extends the expiry --
        # read at t=0.6s (inside the original 1s window), item must
        # still be alive at t=1.3s (past the ORIGINAL deadline, inside
        # the refreshed one).
        await store.aput(ns, "refreshed", {"v": 1}, ttl=1.0 / 60.0)  # 1s
        await asyncio.sleep(0.6)
        item = await store.aget(ns, "refreshed", refresh_ttl=True)
        check(f"[{label}] refreshing read at t=0.6s succeeds", item is not None)
        await asyncio.sleep(0.7)
        item = await store.aget(ns, "refreshed", refresh_ttl=False)
        check(f"[{label}] item still alive past original ttl due to earlier refresh", item is not None)

        await store.adelete(ns, "permanent")

    await proxy.aclose()
    await direct.aclose()


async def test_sync_batch_from_running_loop(postgres_dsn: str):
    """Regression test for a real edge case in batch()'s sync wrapper: a
    direct-mode store's AsyncConnectionPool is opened under whatever
    event loop first uses it. If store.batch() (the sync BaseStore
    method, used by store.get()/put() from a sync graph node) is later
    called from a DIFFERENT already-running loop -- rather than the
    plain worker-thread-with-no-loop case LangGraph normally uses -- the
    fallback path spins up a third loop in a new OS thread and reuses
    that same pool. Empirically verified safe with psycopg_pool (it
    doesn't pin a hard reference to the loop that opened it), but this
    is exactly the kind of cross-event-loop reuse that's easy to get
    subtly wrong, so it gets a permanent check instead of only having
    been verified once by hand.
    """
    store = RunkiteStore(postgres_dsn=postgres_dsn)
    ns = ("sync-batch-test", uuid.uuid4().hex[:8])

    # Loop A: normal async call, opens store._pool.
    await store.aput(ns, "k", {"v": 1})

    # Loop B (this coroutine's own loop, already running): call the SYNC
    # wrapper directly (blocking, not via asyncio.to_thread -- to_thread's
    # worker has no loop of its own, which would take the OTHER branch and
    # not exercise this edge case at all). asyncio.get_running_loop() sees
    # loop B is running on this thread, forcing batch() to hop to a fresh
    # thread + loop C rather than raising "asyncio.run() cannot be called
    # from a running event loop".
    assert asyncio.get_running_loop() is not None
    item = store.get(ns, "k")
    check("sync batch() from a running loop reuses conn without crashing", item is not None and item.value == {"v": 1})

    await store.adelete(ns, "k")
    await store.aclose()


def main():
    test_namespace_encoding()

    http_url = os.environ.get("RUNKITE_HTTP_URL")
    postgres_dsn = os.environ.get("POSTGRES_DSN")
    if not http_url or not postgres_dsn:
        print(
            "\nSkipping live dual-mode interop test "
            "(set RUNKITE_HTTP_URL and POSTGRES_DSN to run it against a live stack)."
        )
        return
    asyncio.run(test_dual_mode_interop(http_url, postgres_dsn))
    asyncio.run(test_ttl(http_url, postgres_dsn))
    asyncio.run(test_sync_batch_from_running_loop(postgres_dsn))
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
