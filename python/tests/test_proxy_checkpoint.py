"""HTTP round-trip self-check for ProxyCheckpointSaver.

Exercises put / get / list / aput_writes against an in-memory MockTransport
that speaks the control-plane /internal/checkpoints contract — no live CP
required. Also asserts tenant-prefixed configurable.thread_id is stripped
before the path (storage_thread_id) and that alist(filter=...) fails loud.

Usage:
    python/.venv/bin/python python/tests/test_proxy_checkpoint.py
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from datetime import datetime, timezone
from urllib.parse import quote, unquote, urlparse

import httpx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.proxy_checkpoint import ProxyCheckpointSaver  # noqa: E402
from runkite_runner.tenant_ctx import bind_tenant, reset_tenant  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def _mock_transport() -> tuple[httpx.MockTransport, dict[tuple[str, str], bytes]]:
    """Minimal CP: PUT/GET/DELETE/LIST/latest with ETag CAS."""
    store: dict[tuple[str, str], bytes] = {}
    meta: dict[tuple[str, str], dict] = {}
    versions: dict[tuple[str, str], int] = {}

    def _ns_of(cid: str) -> str:
        return cid.split("\x1f", 1)[0] if "\x1f" in cid else ""

    def handler(request: httpx.Request) -> httpx.Response:
        path = unquote(urlparse(str(request.url)).path)
        prefix = "/internal/checkpoints/"
        if not path.startswith(prefix):
            return httpx.Response(404, text="not found")
        rest = path[len(prefix) :]
        parts = rest.split("/", 1)
        thread_id = parts[0]
        checkpoint_id = parts[1] if len(parts) > 1 else None

        if request.method == "GET" and checkpoint_id == "latest":
            ns = request.url.params.get("ns", "")
            for k in reversed(list(store.keys())):
                if k[0] != thread_id:
                    continue
                key_ns = k[1].split("\x1f", 1)[0] if "\x1f" in k[1] else ""
                if key_ns != ns:
                    continue
                blob = store[k]
                ver = versions.get(k, 1)
                headers = {
                    "ETag": f'"{ver}"',
                    "X-Runkite-Checkpoint-Id": quote(k[1], safe=""),
                }
                fw = meta.get(k, {}).get("framework")
                if fw:
                    headers["X-Runkite-Checkpoint-Framework"] = fw
                return httpx.Response(200, content=blob, headers=headers)
            return httpx.Response(404, text="missing")

        if request.method == "PUT" and checkpoint_id:
            key = (thread_id, checkpoint_id)
            if_none = request.headers.get("If-None-Match", "").strip()
            if if_none == "*" and key in store:
                return httpx.Response(412, text="already exists")
            if_match = request.headers.get("If-Match", "").strip().strip('"')
            cur = versions.get(key)
            if if_match and cur is not None and if_match != str(cur):
                return httpx.Response(412, text="version mismatch")
            body = request.content
            store[key] = body
            new_ver = (cur or 0) + 1
            versions[key] = new_ver
            meta[key] = {
                "checkpoint_id": checkpoint_id,
                "created_at": datetime.now(timezone.utc).isoformat(),
                "framework": request.headers.get("X-Runkite-Checkpoint-Framework", ""),
                "size_bytes": len(body),
                "version": new_ver,
            }
            return httpx.Response(204, headers={"ETag": f'"{new_ver}"'})

        if request.method == "GET" and checkpoint_id:
            blob = store.get((thread_id, checkpoint_id))
            if blob is None:
                return httpx.Response(404, text="missing")
            ver = versions.get((thread_id, checkpoint_id), 1)
            headers = {"ETag": f'"{ver}"'}
            fw = meta.get((thread_id, checkpoint_id), {}).get("framework")
            if fw:
                headers["X-Runkite-Checkpoint-Framework"] = fw
            return httpx.Response(200, content=blob, headers=headers)

        if request.method == "GET" and checkpoint_id is None:
            items = [meta[k] for k in reversed(list(store.keys())) if k[0] == thread_id]
            limit = int(request.url.params.get("limit", "1000"))
            return httpx.Response(
                200,
                content=json.dumps(items[:limit]).encode(),
                headers={"Content-Type": "application/json"},
            )

        if request.method == "DELETE" and checkpoint_id:
            store.pop((thread_id, checkpoint_id), None)
            meta.pop((thread_id, checkpoint_id), None)
            versions.pop((thread_id, checkpoint_id), None)
            return httpx.Response(204)

        return httpx.Response(405, text="method not allowed")

    return httpx.MockTransport(handler), store


def _checkpoint(cid: str, *, values: dict | None = None) -> dict:
    return {
        "v": 1,
        "id": cid,
        "ts": "2026-01-01T00:00:00+00:00",
        "channel_values": values or {"messages": ["hi"]},
        "channel_versions": {"messages": 1},
        "versions_seen": {},
    }


async def test_round_trip():
    transport, store = _mock_transport()
    saver = ProxyCheckpointSaver(
        http_base_url="http://cp.test",
        runner_token="tok",
        transport=transport,
    )
    token = bind_tenant("acme")
    try:
        # LangGraph-style prefixed thread_id; HTTP path must use bare id.
        config = {"configurable": {"thread_id": "acme:thr-1", "checkpoint_ns": ""}}
        cp = _checkpoint("cp-a")
        out = await saver.aput(config, cp, {"step": 1, "source": "loop", "writes": {}, "parents": {}}, {})
        check("aput returns checkpoint_id", out["configurable"]["checkpoint_id"] == "cp-a")
        check("aput keeps prefixed thread_id in config", out["configurable"]["thread_id"] == "acme:thr-1")
        check("HTTP path used bare thread_id", ("thr-1", "cp-a") in store)
        check("tenant prefix not used as storage key", ("acme:thr-1", "cp-a") not in store)

        got = await saver.aget_tuple(
            {
                "configurable": {
                    "thread_id": "acme:thr-1",
                    "checkpoint_ns": "",
                    "checkpoint_id": "cp-a",
                }
            }
        )
        check("aget_tuple finds blob", got is not None)
        assert got is not None
        check("checkpoint values round-trip", got.checkpoint["channel_values"]["messages"] == ["hi"])
        check("aget keeps prefixed thread_id", got.config["configurable"]["thread_id"] == "acme:thr-1")

        write_cfg = {
            "configurable": {
                "thread_id": "acme:thr-1",
                "checkpoint_ns": "",
                "checkpoint_id": "cp-a",
            }
        }
        await saver.aput_writes(write_cfg, [("messages", "extra")], task_id="task-1")
        got2 = await saver.aget_tuple(write_cfg)
        assert got2 is not None
        check(
            "aput_writes appends pending_writes",
            got2.pending_writes is not None
            and len(got2.pending_writes) == 1
            and got2.pending_writes[0][0] == "task-1"
            and got2.pending_writes[0][2] == "extra",
        )

        # Second checkpoint + list "latest"
        await saver.aput(
            {"configurable": {"thread_id": "acme:thr-1", "checkpoint_ns": "", "checkpoint_id": "cp-a"}},
            _checkpoint("cp-b", values={"messages": ["bye"]}),
            {"step": 2, "source": "loop", "writes": {}, "parents": {}},
            {},
        )
        latest = await saver.aget_tuple({"configurable": {"thread_id": "acme:thr-1", "checkpoint_ns": ""}})
        check("aget latest returns newest", latest is not None and latest.checkpoint["id"] == "cp-b")

        listed = []
        async for item in saver.alist(
            {"configurable": {"thread_id": "acme:thr-1", "checkpoint_ns": ""}},
            limit=10,
        ):
            listed.append(item.checkpoint["id"])
        check("alist returns both newest-first", listed == ["cp-b", "cp-a"])

        # Subgraph namespace folded into key
        await saver.aput(
            {"configurable": {"thread_id": "acme:thr-1", "checkpoint_ns": "node:sub"}},
            _checkpoint("cp-ns"),
            {"step": 1, "source": "loop", "writes": {}, "parents": {}},
            {},
        )
        check(
            "ns folded into path key",
            any(k[1] == "node:sub\x1fcp-ns" for k in store),
        )
        sub = await saver.aget_tuple(
            {
                "configurable": {
                    "thread_id": "acme:thr-1",
                    "checkpoint_ns": "node:sub",
                    "checkpoint_id": "cp-ns",
                }
            }
        )
        check("aget with ns round-trips", sub is not None and sub.checkpoint["id"] == "cp-ns")

        raised = False
        try:
            async for _ in saver.alist(
                {"configurable": {"thread_id": "acme:thr-1"}},
                filter={"source": "loop"},
            ):
                pass
        except NotImplementedError:
            raised = True
        check("alist(filter=...) raises NotImplementedError", raised)

        no_id = False
        try:
            await saver.aput_writes(
                {"configurable": {"thread_id": "acme:thr-1", "checkpoint_ns": ""}},
                [("messages", "orphan")],
                task_id="t-missing",
            )
        except ValueError:
            no_id = True
        check("aput_writes without checkpoint_id raises", no_id)

        # LangGraph Pregel calls aput_writes for a *new* checkpoint_id
        # before aput — 404 must create a shell (If-None-Match:*), not raise.
        await saver.aput_writes(
            {
                "configurable": {
                    "thread_id": "acme:thr-1",
                    "checkpoint_ns": "",
                    "checkpoint_id": "cp-missing",
                }
            },
            [("messages", "orphan")],
            task_id="t-404",
        )
        pre = await saver.aget_tuple(
            {
                "configurable": {
                    "thread_id": "acme:thr-1",
                    "checkpoint_ns": "",
                    "checkpoint_id": "cp-missing",
                }
            }
        )
        assert pre is not None and pre.pending_writes is not None
        check(
            "aput_writes on missing blob creates shell (Pregel order)",
            any(w[0] == "t-404" for w in pre.pending_writes),
        )

        # aput must preserve pending writes already on the blob (LangGraph
        # may call aput after aput_writes within the same superstep).
        await saver.aput_writes(write_cfg, [("messages", "keep-me")], task_id="task-keep")
        await saver.aput(
            write_cfg,
            _checkpoint("cp-a", values={"messages": ["hi", "kept"]}),
            {"step": 1, "source": "loop", "writes": {}, "parents": {}},
            {},
        )
        after_aput = await saver.aget_tuple(write_cfg)
        assert after_aput is not None
        keep_ok = after_aput.pending_writes is not None and any(w[0] == "task-keep" for w in after_aput.pending_writes)
        check("aput preserves existing pending_writes", keep_ok)
    finally:
        reset_tenant(token)
        await saver.aclose()


async def test_concurrent_pending_writes_cas():
    """Two saver instances (no shared lock) racing aput_writes — both channels survive."""
    transport, _store = _mock_transport()
    s1 = ProxyCheckpointSaver(http_base_url="http://cp.test", runner_token="tok", transport=transport)
    s2 = ProxyCheckpointSaver(http_base_url="http://cp.test", runner_token="tok", transport=transport)
    token = bind_tenant("default")
    try:
        cfg = {"configurable": {"thread_id": "thr-cas", "checkpoint_ns": ""}}
        await s1.aput(
            cfg,
            _checkpoint("cp-cas"),
            {"step": 1, "source": "loop", "writes": {}, "parents": {}},
            {},
        )
        write_cfg = {
            "configurable": {
                "thread_id": "thr-cas",
                "checkpoint_ns": "",
                "checkpoint_id": "cp-cas",
            }
        }

        async def write(saver, channel, task):
            await saver.aput_writes(write_cfg, [(channel, channel)], task_id=task)

        await asyncio.gather(
            write(s1, "a", "task-a"),
            write(s2, "b", "task-b"),
        )
        got = await s1.aget_tuple(write_cfg)
        assert got is not None and got.pending_writes is not None
        channels = {w[1] for w in got.pending_writes}
        check(
            "concurrent aput_writes CAS keeps both channels",
            channels == {"a", "b"},
        )
    finally:
        reset_tenant(token)
        await s1.aclose()
        await s2.aclose()


async def test_aput_create_only_preserves_writes_shell():
    """Cross-saver race: aput_writes shell must survive a peer aput on 404.

    Two savers (no shared lock). Both see a missing blob; whichever create-
    only PUT wins first, the loser 412-retries and merges — never an
    unconditional aput that wipes pending writes.
    """
    transport, _store = _mock_transport()
    s1 = ProxyCheckpointSaver(http_base_url="http://cp.test", runner_token="tok", transport=transport)
    s2 = ProxyCheckpointSaver(http_base_url="http://cp.test", runner_token="tok", transport=transport)
    token = bind_tenant("default")
    try:
        cfg = {"configurable": {"thread_id": "thr-race", "checkpoint_ns": ""}}
        write_cfg = {
            "configurable": {
                "thread_id": "thr-race",
                "checkpoint_ns": "",
                "checkpoint_id": "cp-race",
            }
        }

        async def do_writes():
            await s1.aput_writes(write_cfg, [("messages", "from-writes")], task_id="task-w")

        async def do_aput():
            await s2.aput(
                cfg,
                _checkpoint("cp-race", values={"messages": ["from-aput"]}),
                {"step": 1, "source": "loop", "writes": {}, "parents": {}},
                {},
            )

        await asyncio.gather(do_writes(), do_aput())
        got = await s1.aget_tuple(write_cfg)
        assert got is not None
        check(
            "race: checkpoint values from aput present",
            got.checkpoint["channel_values"]["messages"] == ["from-aput"],
        )
        check(
            "race: pending writes from aput_writes shell preserved",
            got.pending_writes is not None and any(w[0] == "task-w" for w in got.pending_writes),
        )
    finally:
        reset_tenant(token)
        await s1.aclose()
        await s2.aclose()


async def test_soft_noop_on_run_not_inflight():
    """Late put/putWrites after cancel must not raise (parity with TS)."""

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            403,
            content=json.dumps({"error": "run_not_inflight"}).encode(),
            headers={"Content-Type": "application/json"},
        )

    saver = ProxyCheckpointSaver(
        http_base_url="http://cp.test",
        runner_token="tok",
        transport=httpx.MockTransport(handler),
    )
    token = bind_tenant("acme")
    try:
        out = await saver.aput(
            {"configurable": {"thread_id": "thr-x", "checkpoint_ns": ""}},
            _checkpoint("cp-dead"),
            {"step": 1, "source": "loop", "writes": {}, "parents": {}},
            {},
        )
        check("aput soft-no-op returns config", out["configurable"]["checkpoint_id"] == "cp-dead")
        await saver.aput_writes(
            {
                "configurable": {
                    "thread_id": "thr-x",
                    "checkpoint_ns": "",
                    "checkpoint_id": "cp-dead",
                }
            },
            [("messages", "late")],
            task_id="t-late",
        )
        check("aput_writes soft-no-op returns", True)
    finally:
        reset_tenant(token)
        await saver.aclose()


def main():
    asyncio.run(test_round_trip())
    asyncio.run(test_concurrent_pending_writes_cas())
    asyncio.run(test_aput_create_only_preserves_writes_shell())
    asyncio.run(test_soft_noop_on_run_not_inflight())
    print("all proxy checkpoint checks passed")


if __name__ == "__main__":
    main()
