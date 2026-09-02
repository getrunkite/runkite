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
    """Minimal ASGI-like responder via httpx MockTransport."""

    def __init__(self):
        self.store: dict[str, bytes] = {}

    def handler(self, request):
        import httpx

        path = request.url.path
        if request.method == "GET" and path.endswith("/latest"):
            if not self.store:
                return httpx.Response(404, text="missing")
            # Return any blob (single key in this fake).
            data = next(iter(self.store.values()))
            return httpx.Response(200, content=data)
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
            check("miss", await client.get_latest("thr_1") is None)
            blob = encode_messages_checkpoint([{"role": "human", "content": "x"}])
            await client.put("thr_1", blob)
            check("id reserved name", ADAPTER_CHECKPOINT_ID != "latest")
            got = await client.get_latest("thr_1")
            check("hit", decode_messages_checkpoint(got)[0]["content"] == "x")
            await client.aclose()
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
        finally:
            reset_run(rt)
            reset_tenant(tt)

    asyncio.run(_run())


if __name__ == "__main__":
    test_messages_blob_roundtrip()
    test_merge_prepends_when_client_sends_new_turn_only()
    test_merge_prefers_longer_client_history()
    test_context_prompt_includes_prior()
    test_adapter_supports_checkpoints()
    test_opaque_client_get_put()
    test_execute_with_opaque_two_turns()
    print("all adapter checkpoint checks passed")
