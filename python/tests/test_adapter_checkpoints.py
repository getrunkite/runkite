"""Self-check for adapter opaque checkpoint helpers + LlamaIndex merge.

Usage:
    python/.venv/bin/python python/tests/test_adapter_checkpoints.py
"""

from __future__ import annotations

import asyncio
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.adapter_checkpoint import (  # noqa: E402
    context_prompt_from_messages,
    decode_messages_checkpoint,
    encode_messages_checkpoint,
    merge_messages_input,
)
from runkite_runner.generic_worker import _execute_with_opaque_checkpoint  # noqa: E402
from runkite_runner.opaque_checkpoint import (  # noqa: E402
    ADAPTER_CHECKPOINT_ID,
    OpaqueCheckpointClient,
    adapter_supports_checkpoints,
)


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_messages_blob_roundtrip():
    msgs = [{"role": "human", "content": "hi"}, {"role": "ai", "content": "hello"}]
    blob = encode_messages_checkpoint(msgs)
    check("decode roundtrip", decode_messages_checkpoint(blob) == msgs)
    check("empty decode", decode_messages_checkpoint(None) == [])


def test_merge_prepends_when_client_sends_new_turn_only():
    prior = [{"role": "human", "content": "a"}, {"role": "ai", "content": "b"}]
    incoming = {"messages": [{"role": "human", "content": "c"}]}
    merged = merge_messages_input(prior, incoming)
    check(
        "prepend prior",
        merged["messages"]
        == [
            {"role": "human", "content": "a"},
            {"role": "ai", "content": "b"},
            {"role": "human", "content": "c"},
        ],
    )


def test_merge_prefers_longer_client_history():
    prior = [{"role": "human", "content": "a"}]
    incoming = {
        "messages": [
            {"role": "human", "content": "a"},
            {"role": "ai", "content": "b"},
            {"role": "human", "content": "c"},
        ]
    }
    merged = merge_messages_input(prior, incoming)
    check("prefer client full history", merged["messages"] == incoming["messages"])


def test_merge_equal_length_prefers_client():
    """Equal length must not double-prepend (client already has full history)."""
    prior = [{"role": "human", "content": "a"}, {"role": "ai", "content": "b"}]
    incoming = {"messages": [{"role": "human", "content": "a"}, {"role": "ai", "content": "b"}]}
    merged = merge_messages_input(prior, incoming)
    check("equal length uses client", merged["messages"] == incoming["messages"])
    check("no double length", len(merged["messages"]) == 2)


def test_context_prompt_includes_prior():
    msgs = [
        {"role": "human", "content": "hi"},
        {"role": "ai", "content": "hello"},
        {"role": "human", "content": "more"},
    ]
    text = context_prompt_from_messages(msgs)
    check("includes prior", "Previous conversation:" in text and "more" in text)


def test_adapter_supports_checkpoints():
    class Yes:
        checkpoint_framework = "llamaindex"

    class No:
        pass

    check("yes", adapter_supports_checkpoints(Yes()))
    check("no", not adapter_supports_checkpoints(No()))


class _FakeTransport:
    """Minimal responder via httpx MockTransport.

    Stores per checkpoint_id. GET .../latest returns the lexicographically
    greatest id (mirrors Postgres ORDER BY checkpoint_id DESC) so tests can
    prove adapters must not use /latest when a LangGraph UUID also exists.
    """

    def __init__(self):
        self.store: dict[str, bytes] = {}
        self.get_paths: list[str] = []

    def handler(self, request):
        import httpx

        path = request.url.path
        if request.method == "GET":
            self.get_paths.append(path)
            cid = path.rsplit("/", 1)[-1]
            if cid == "latest":
                if not self.store:
                    return httpx.Response(404, text="missing")
                # Lexicographic max — LangGraph UUIDs often win over adapter-state.
                best = max(self.store.keys())
                return httpx.Response(200, content=self.store[best])
            if cid in self.store:
                return httpx.Response(200, content=self.store[cid])
            return httpx.Response(404, text="missing")
        if request.method == "PUT":
            cid = path.rsplit("/", 1)[-1]
            self.store[cid] = request.content
            return httpx.Response(204)
        return httpx.Response(500, text="unexpected")


def test_opaque_client_get_put():
    import httpx

    fake = _FakeTransport()
    transport = httpx.MockTransport(fake.handler)

    async def _run():
        from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

        tt = bind_tenant("default")
        rt = bind_run("run-1", 1)
        try:
            client = OpaqueCheckpointClient(
                http_base_url="http://cp.test",
                framework="llamaindex",
                transport=transport,
            )
            check("miss", await client.get("thr_1") is None)
            blob = encode_messages_checkpoint([{"role": "human", "content": "x"}])
            await client.put("thr_1", blob)
            check("id reserved name", ADAPTER_CHECKPOINT_ID != "latest")
            got = await client.get("thr_1")
            check("hit", decode_messages_checkpoint(got)[0]["content"] == "x")
            check("get used adapter-state", fake.get_paths[-1].endswith("/" + ADAPTER_CHECKPOINT_ID))
            await client.aclose()
        finally:
            reset_run(rt)
            reset_tenant(tt)

    asyncio.run(_run())


def test_get_ignores_lexicographically_newer_langgraph_blob():
    """Regression: shared thread with LangGraph UUID must not poison adapter load."""
    import httpx

    fake = _FakeTransport()
    # UUID sorts above "adapter-state" under checkpoint_id DESC.
    fake.store["f47ac10b-58cc-4372-a567-0e02b2c3d479"] = b"\xfflanggraph-binary-not-json"
    adapter_blob = encode_messages_checkpoint([{"role": "human", "content": "adapter-ok"}])
    fake.store[ADAPTER_CHECKPOINT_ID] = adapter_blob
    transport = httpx.MockTransport(fake.handler)

    async def _run():
        from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

        tt = bind_tenant("default")
        rt = bind_run("run-1", 1)
        try:
            client = OpaqueCheckpointClient(
                http_base_url="http://cp.test",
                framework="llamaindex",
                transport=transport,
            )
            # /latest would return the LangGraph blob (lexicographic max).
            # Adapter get() must return adapter-state only.
            got = await client.get("thr_shared")
            check("got adapter blob not LG", decode_messages_checkpoint(got)[0]["content"] == "adapter-ok")
            check("path was adapter-state", "/adapter-state" in fake.get_paths[-1])
            check("never hit /latest", not any(p.endswith("/latest") for p in fake.get_paths))
            await client.aclose()
        finally:
            reset_run(rt)
            reset_tenant(tt)

    asyncio.run(_run())


def test_execute_soft_fails_corrupt_prepare():
    """Corrupt blob / prepare raising must not kill the run."""
    import httpx

    fake = _FakeTransport()
    fake.store[ADAPTER_CHECKPOINT_ID] = b"\xffnot-json"
    transport = httpx.MockTransport(fake.handler)

    class FragileAdapter:
        checkpoint_framework = "crewai"
        saw_input = None

        def prepare_checkpoint_input(self, prior, input_data):
            # Same path adapters use: decode then merge — raises on binary LG blob.
            return merge_messages_input(decode_messages_checkpoint(prior), input_data)

        def serialize_checkpoint(self, assignment, values, status):
            return None

        async def load_config(self, config_path: str) -> None:
            return None

        async def execute(self, assignment, event_callback, cancel_event):
            FragileAdapter.saw_input = assignment.get("input")
            await event_callback({"method": "end", "data": {"status": "success"}, "seq": 1})
            return "success"

    async def _run():
        from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

        def resolve(_http):
            return "http://cp.test"

        def client_factory(**kwargs):
            return OpaqueCheckpointClient(**kwargs, transport=transport)

        tt = bind_tenant("default")
        rt = bind_run("run-c", 1)
        try:
            raw = {"messages": [{"role": "human", "content": "only-new"}]}
            a = {"run_id": "run-c", "thread_id": "thr_y", "graph_id": "g", "input": raw}

            async def send(_e):
                return None

            st = await _execute_with_opaque_checkpoint(
                FragileAdapter(),
                a,
                send,
                asyncio.Event(),
                http_address="http://cp.test",
                runner_kind="python-crewai",
                runner_token="",
                resolve_http_url=resolve,
                OpaqueCheckpointClient=client_factory,
                adapter_supports_checkpoints=adapter_supports_checkpoints,
            )
            check("status success despite corrupt prior", st == "success")
            check("raw client input kept", FragileAdapter.saw_input == raw)
            check("original assignment input unchanged", a["input"] == raw)
        finally:
            reset_run(rt)
            reset_tenant(tt)

    asyncio.run(_run())


def test_execute_with_opaque_two_turns():
    """generic_worker load/save path: turn2 sees turn1 history with new message only."""
    import httpx

    fake = _FakeTransport()
    transport = httpx.MockTransport(fake.handler)

    class EchoAdapter:
        checkpoint_framework = "llamaindex"

        def prepare_checkpoint_input(self, prior, input_data):
            return merge_messages_input(decode_messages_checkpoint(prior), input_data)

        def serialize_checkpoint(self, assignment, values, status):
            msgs = values.get("messages")
            return encode_messages_checkpoint(msgs) if msgs else None

        async def load_config(self, config_path: str) -> None:
            return None

        async def execute(self, assignment, event_callback, cancel_event):
            messages = list((assignment.get("input") or {}).get("messages") or [])
            reply = f"echo:{messages[-1]['content']}|n={len(messages)}"
            out = messages + [{"role": "ai", "content": reply}]
            await event_callback({"method": "values", "data": {"messages": out}, "seq": 1})
            await event_callback({"method": "end", "data": {"status": "success"}, "seq": 2})
            return "success"

    async def _run():
        from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

        adapter = EchoAdapter()
        events1: list = []
        events2: list = []

        async def send1(e):
            events1.append(e)

        async def send2(e):
            events2.append(e)

        def resolve(_http):
            return "http://cp.test"

        def client_factory(**kwargs):
            return OpaqueCheckpointClient(**kwargs, transport=transport)

        tt = bind_tenant("default")
        rt = bind_run("run-a", 1)
        try:
            a1 = {
                "run_id": "run-a",
                "thread_id": "thr_x",
                "graph_id": "g",
                "input": {"messages": [{"role": "human", "content": "one"}]},
            }
            st1 = await _execute_with_opaque_checkpoint(
                adapter,
                a1,
                send1,
                asyncio.Event(),
                http_address="http://cp.test",
                runner_kind="python-llamaindex",
                runner_token="",
                resolve_http_url=resolve,
                OpaqueCheckpointClient=client_factory,
                adapter_supports_checkpoints=adapter_supports_checkpoints,
            )
            check("turn1 status", st1 == "success")
            check("turn1 reply len", any("n=1" in json.dumps(e) for e in events1))
            check("put adapter-state", ADAPTER_CHECKPOINT_ID in fake.store)
        finally:
            reset_run(rt)
            reset_tenant(tt)

        tt = bind_tenant("default")
        rt = bind_run("run-b", 1)
        try:
            a2 = {
                "run_id": "run-b",
                "thread_id": "thr_x",
                "graph_id": "g",
                "input": {"messages": [{"role": "human", "content": "two"}]},
            }
            st2 = await _execute_with_opaque_checkpoint(
                adapter,
                a2,
                send2,
                asyncio.Event(),
                http_address="http://cp.test",
                runner_kind="python-llamaindex",
                runner_token="",
                resolve_http_url=resolve,
                OpaqueCheckpointClient=client_factory,
                adapter_supports_checkpoints=adapter_supports_checkpoints,
            )
            check("turn2 status", st2 == "success")
            # After merge: prior human+ai + new human => n=3 before ai append => values has 4
            vals = [e for e in events2 if e.get("method") == "values"]
            check("turn2 has values", bool(vals))
            n_msgs = len(vals[0]["data"]["messages"])
            check("turn2 restored history (4 msgs after reply)", n_msgs == 4)
            check("turn2 reply sees n=3 input turns", "n=3" in vals[0]["data"]["messages"][-1]["content"])
            check("loads used adapter-state not latest", any("/adapter-state" in p for p in fake.get_paths))
            check("never used /latest for load", not any(p.endswith("/latest") for p in fake.get_paths))
        finally:
            reset_run(rt)
            reset_tenant(tt)

    asyncio.run(_run())


def test_execute_opaque_survives_fresh_client():
    """Restart proof: turn2 uses a new HTTP client; history comes only from CP store.

    Same process may keep a live OpaqueCheckpointClient across turns in production.
    This test drops the client after turn1 and builds a new one against the same
    fake store so continuity cannot be explained by in-process cache.
    """
    import httpx

    fake = _FakeTransport()

    class EchoAdapter:
        checkpoint_framework = "crewai"

        def prepare_checkpoint_input(self, prior, input_data):
            return merge_messages_input(decode_messages_checkpoint(prior), input_data)

        def serialize_checkpoint(self, assignment, values, status):
            msgs = values.get("messages")
            return encode_messages_checkpoint(msgs) if msgs else None

        async def load_config(self, config_path: str) -> None:
            return None

        async def execute(self, assignment, event_callback, cancel_event):
            messages = list((assignment.get("input") or {}).get("messages") or [])
            reply = f"echo:{messages[-1]['content']}|n={len(messages)}"
            out = messages + [{"role": "ai", "content": reply}]
            await event_callback({"method": "values", "data": {"messages": out}, "seq": 1})
            await event_callback({"method": "end", "data": {"status": "success"}, "seq": 2})
            return "success"

    async def _run():
        from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

        adapter = EchoAdapter()
        events2: list = []

        def resolve(_http):
            return "http://cp.test"

        # --- turn 1: client A ---
        transport_a = httpx.MockTransport(fake.handler)
        clients_a: list = []

        def client_factory_a(**kwargs):
            c = OpaqueCheckpointClient(**kwargs, transport=transport_a)
            clients_a.append(c)
            return c

        tt = bind_tenant("default")
        rt = bind_run("run-restart-a", 1)
        try:

            async def send1(_e):
                return None

            st1 = await _execute_with_opaque_checkpoint(
                adapter,
                {
                    "run_id": "run-restart-a",
                    "thread_id": "thr_restart",
                    "graph_id": "g",
                    "input": {"messages": [{"role": "human", "content": "one"}]},
                },
                send1,
                asyncio.Event(),
                http_address="http://cp.test",
                runner_kind="python-crewai",
                runner_token="",
                resolve_http_url=resolve,
                OpaqueCheckpointClient=client_factory_a,
                adapter_supports_checkpoints=adapter_supports_checkpoints,
            )
            check("restart turn1 status", st1 == "success")
            check("restart blob in store", ADAPTER_CHECKPOINT_ID in fake.store)
        finally:
            reset_run(rt)
            reset_tenant(tt)
            for c in clients_a:
                await c.aclose()
            clients_a.clear()

        # Simulate runner death: drop transport A; only bytes in fake.store remain.
        blob_after_t1 = fake.store[ADAPTER_CHECKPOINT_ID]
        fake.get_paths.clear()

        # --- turn 2: brand-new client B (no shared httpx client) ---
        transport_b = httpx.MockTransport(fake.handler)

        def client_factory_b(**kwargs):
            return OpaqueCheckpointClient(**kwargs, transport=transport_b)

        async def send2(e):
            events2.append(e)

        tt = bind_tenant("default")
        rt = bind_run("run-restart-b", 1)
        try:
            st2 = await _execute_with_opaque_checkpoint(
                adapter,
                {
                    "run_id": "run-restart-b",
                    "thread_id": "thr_restart",
                    "graph_id": "g",
                    "input": {"messages": [{"role": "human", "content": "two"}]},
                },
                send2,
                asyncio.Event(),
                http_address="http://cp.test",
                runner_kind="python-crewai",
                runner_token="",
                resolve_http_url=resolve,
                OpaqueCheckpointClient=client_factory_b,
                adapter_supports_checkpoints=adapter_supports_checkpoints,
            )
            check("restart turn2 status", st2 == "success")
            vals = [e for e in events2 if e.get("method") == "values"]
            check("restart turn2 has values", bool(vals))
            n_msgs = len(vals[0]["data"]["messages"])
            check("restart restored history (4 msgs)", n_msgs == 4)
            check(
                "restart reply sees n=3 from store only",
                "n=3" in vals[0]["data"]["messages"][-1]["content"],
            )
            check(
                "restart load hit adapter-state",
                any(p.endswith("/" + ADAPTER_CHECKPOINT_ID) for p in fake.get_paths),
            )
            # Turn1 blob was durable in the store before turn2 (not client-local).
            check(
                "restart turn1 blob was messages",
                len(decode_messages_checkpoint(blob_after_t1)) == 2,
            )
            after = decode_messages_checkpoint(fake.store[ADAPTER_CHECKPOINT_ID])
            check("restart put wrote 4 msgs", len(after) == 4)
        finally:
            reset_run(rt)
            reset_tenant(tt)

    asyncio.run(_run())


if __name__ == "__main__":
    test_messages_blob_roundtrip()
    test_merge_prepends_when_client_sends_new_turn_only()
    test_merge_prefers_longer_client_history()
    test_merge_equal_length_prefers_client()
    test_context_prompt_includes_prior()
    test_adapter_supports_checkpoints()
    test_opaque_client_get_put()
    test_get_ignores_lexicographically_newer_langgraph_blob()
    test_execute_soft_fails_corrupt_prepare()
    test_execute_with_opaque_two_turns()
    test_execute_opaque_survives_fresh_client()
    print("all adapter checkpoint checks passed")
