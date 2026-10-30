"""Native Google ADK history must be safe before any model or tool executes."""
# ruff: noqa: E402

from __future__ import annotations

import copy
import json
import math
import os
import sys
import unittest
from pathlib import Path

os.environ.setdefault("ADK_SUPPRESS_GEMINI_LITELLM_WARNINGS", "true")
ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

import httpx
from google.adk.events import Event
from google.adk.sessions import Session
from google.genai import types
from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient
from runkite_runner.tenant_ctx import bind_run, bind_runner, bind_tenant, reset_run, reset_runner, reset_tenant

from google_adk_adapter.state import FRAMEWORK, GoogleAdkState, dump_session, encode_state, state_json

IDENTITY = {"graph_id": "agent-one", "app_name": "agent-one", "user_id": "tenant-one", "session_id": "thread-one"}


def native_event(author: str, run: str, parts: list, *, usage: bool = False, **kwargs) -> Event:
    return Event(
        author=author,
        invocation_id=run,
        content=types.Content(
            role="model" if author != "user" and not any(p.function_response for p in parts) else "user", parts=parts
        ),
        model_version="wire-model" if usage else None,
        usage_metadata=types.GenerateContentResponseUsageMetadata(
            prompt_token_count=10,
            tool_use_prompt_token_count=2,
            candidates_token_count=3,
            thoughts_token_count=4,
            total_token_count=19,
        )
        if usage
        else None,
        **kwargs,
    )


def envelope(*, tools: bool = False, prior: bool = False, thinking: bool = False) -> dict:
    session = Session(
        id="thread-one", app_name="agent-one", user_id="tenant-one", state={"counter": 1}, last_update_time=10.0
    )
    messages = []
    if prior:
        session.events += [
            native_event("user", "old-run", [types.Part(text="old")]),
            native_event("agent", "old-run", [types.Part(text="Before")], usage=True),
        ]
        messages += [{"role": "user", "content": "old"}, {"role": "assistant", "content": "Before"}]
    session.events.append(native_event("user", "run-one", [types.Part(text="hello")]))
    if tools:
        session.events.append(
            native_event(
                "agent",
                "run-one",
                [
                    types.Part(text="Checking. "),
                    types.Part(function_call=types.FunctionCall(id="call", name="lookup", args={"type": "approval"})),
                ],
                usage=True,
            )
        )
        session.events.append(
            native_event(
                "agent",
                "run-one",
                [
                    types.Part(
                        function_response=types.FunctionResponse(
                            id="call",
                            name="lookup",
                            response={"result": {"type": "approval", "value": "business JSON"}},
                        )
                    )
                ],
            )
        )
    parts = [types.Part(text="Hello")]
    if thinking:
        parts.insert(0, types.Part(text="reasoning", thought=True, thought_signature=b"\xff\x00"))
        parts[-1].thought_signature = b"\x80\x01"
    session.events.append(native_event("agent", "run-one", parts, usage=True))
    text = ("Checking. " if tools else "") + "Hello"
    messages += [{"role": "user", "content": "hello"}, {"role": "assistant", "content": text}]
    calls = 2 if tools else 1
    output = {
        "messages": copy.deepcopy(messages),
        "google_adk": {"framework": FRAMEWORK, "generation": 1},
        "usage": {
            "prompt_tokens": calls * 12,
            "completion_tokens": calls * 7,
            "total_tokens": calls * 19,
            "model": "wire-model",
        },
    }
    return {
        "v": 1,
        "framework": FRAMEWORK,
        "graph_id": "agent-one",
        "history": dump_session(session),
        "messages": messages,
        "completed": {
            "run_id": "run-one",
            "status": "success",
            "input": state_json([{"role": "user", "content": "hello"}]),
            "output": output,
            "text_events": [text[:2], text[2:]],
        },
    }


class NativeStateTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.requests = []
        self.clients = []
        self.value = envelope()

    async def asyncTearDown(self):
        for client in self.clients:
            await client.aclose()

    def store(self, body: bytes, *, headers=None, status=200, put_status=204):
        def respond(request):
            self.requests.append(request)
            return (
                httpx.Response(
                    status,
                    content=body,
                    headers=headers
                    if headers is not None
                    else {"ETag": '"7"', "X-Runkite-Checkpoint-Framework": FRAMEWORK},
                )
                if request.method == "GET"
                else httpx.Response(put_status)
            )

        client = OpaqueCheckpointClient(
            http_base_url="http://plane", framework=FRAMEWORK, transport=httpx.MockTransport(respond)
        )
        self.clients.append(client)
        return GoogleAdkState(client)

    async def reject(self, value):
        with self.assertRaises(ValueError):
            encode_state(value, **IDENTITY)
        store = self.store(state_json(value).encode())
        with self.assertRaises(CompletionDeliveryError):
            await store.load("thread-one", **IDENTITY)
        before = len(self.requests)
        with self.assertRaises(CompletionDeliveryError):
            await store.save("thread-one", value, '"7"', **IDENTITY)
        self.assertEqual(before, len(self.requests))

    async def test_native_text_tools_reasoning_and_two_turn_state_roundtrip(self):
        for kwargs in ({}, {"tools": True}, {"thinking": True}, {"prior": True, "tools": True, "thinking": True}):
            with self.subTest(kwargs=kwargs):
                value = envelope(**kwargs)
                store = self.store(encode_state(value, **IDENTITY))
                loaded, etag = await store.load("thread-one", **IDENTITY)
                self.assertEqual(loaded, value)
                native = Session.model_validate_json(state_json(loaded["history"]))
                self.assertEqual(dump_session(native), value["history"])
                await store.save("thread-one", loaded, etag, **IDENTITY)
                self.assertEqual(self.requests[-1].headers["If-Match"], '"7"')
                self.assertEqual(json.loads(self.requests[-1].content), value)

    async def test_create_only_missing_state_and_cas_conflict_are_not_success(self):
        store = self.store(b"", status=404)
        self.assertEqual(await store.load("thread-one", **IDENTITY), (None, None))
        await store.save("thread-one", self.value, None, **IDENTITY)
        self.assertEqual(self.requests[-1].headers["If-None-Match"], "*")
        store = self.store(state_json(self.value).encode(), put_status=412)
        with self.assertRaises(CompletionDeliveryError):
            await store.save("thread-one", self.value, '"7"', **IDENTITY)

    async def test_missing_or_malformed_etag_and_framework_rejected_before_execution(self):
        for etag in (None, 'W/"1"', '"0"', '"01"', '"١"', '"9223372036854775808"'):
            headers = {"X-Runkite-Checkpoint-Framework": FRAMEWORK}
            if etag is not None:
                headers["ETag"] = etag
            with self.subTest(etag=etag):
                with self.assertRaises(CompletionDeliveryError):
                    await self.store(state_json(self.value).encode(), headers=headers).load("thread-one", **IDENTITY)
        for framework in (None, "pydantic-ai"):
            headers = {"ETag": '"1"'}
            if framework:
                headers["X-Runkite-Checkpoint-Framework"] = framework
            with self.assertRaises(CompletionDeliveryError):
                await self.store(state_json(self.value).encode(), headers=headers).load("thread-one", **IDENTITY)

    async def test_database_outage_and_superseded_assignment_leave_state_unconfirmed(self):
        for status in (403, 500):
            with self.assertRaises(CompletionDeliveryError):
                await self.store(b"", status=status).load("thread-one", **IDENTITY)
            with self.assertRaises(CompletionDeliveryError):
                await self.store(b"", put_status=status).save("thread-one", self.value, None, **IDENTITY)

    async def test_wrong_envelope_native_or_checkpoint_identity_fail_closed(self):
        for field, invalid in (("v", True), ("v", 2), ("framework", "openai-agents"), ("graph_id", "other")):
            value = copy.deepcopy(self.value)
            value[field] = invalid
            await self.reject(value)
        for field in ("id", "app_name", "user_id"):
            value = copy.deepcopy(self.value)
            value["history"][field] = "foreign"
            await self.reject(value)
        with self.assertRaises(CompletionDeliveryError):
            await self.store(b"", status=404).load("other-thread", **IDENTITY)

    async def test_receiptless_interrupted_or_foreign_invocation_state_rejected(self):
        for change in ("missing", "status", "run_id"):
            value = copy.deepcopy(self.value)
            if change == "missing":
                del value["completed"]
            elif change == "status":
                value["completed"]["status"] = "interrupted"
            else:
                value["completed"]["run_id"] = "foreign"
            await self.reject(value)
        value = copy.deepcopy(self.value)
        value["history"]["events"][-1]["invocation_id"] = "foreign"
        await self.reject(value)

    async def test_unknown_native_fields_and_coercible_bool_counters_are_not_silently_lost(self):
        for mutate in (
            lambda v: v["history"]["events"][-1].update(unknown="lost"),
            lambda v: v["history"]["events"][-1]["content"]["parts"][0].update(unknown="lost"),
            lambda v: v["history"]["events"][-1]["usage_metadata"].update(prompt_token_count=True),
            lambda v: v["history"]["events"][-1]["usage_metadata"].update(prompt_token_count=10.0),
        ):
            value = copy.deepcopy(self.value)
            mutate(value)
            await self.reject(value)

    async def test_unresolved_mismatched_and_partial_native_tools_rejected(self):
        value = envelope(tools=True)
        del value["history"]["events"][2]
        await self.reject(value)
        for field, changed in (("id", "other"), ("name", "other"), ("will_continue", True)):
            value = envelope(tools=True)
            value["history"]["events"][2]["content"]["parts"][0]["function_response"][field] = changed
            await self.reject(value)
        for field, changed in (("id", None), ("partial_args", []), ("will_continue", True)):
            value = envelope(tools=True)
            value["history"]["events"][1]["content"]["parts"][1]["function_call"][field] = changed
            await self.reject(value)

    async def test_partial_errors_pending_auth_approval_delegation_and_workflow_state_rejected(self):
        for field, changed in (
            ("partial", True),
            ("error_code", "ERROR"),
            ("interrupted", True),
            ("long_running_tool_ids", ["call"]),
            ("branch", "child"),
            ("output", {}),
            ("finish_reason", "MAX_TOKENS"),
        ):
            value = copy.deepcopy(self.value)
            value["history"]["events"][-1][field] = changed
            await self.reject(value)
        for field, changed in (
            ("transfer_to_agent", "other"),
            ("requested_tool_confirmations", {"call": {"confirmed": False}}),
            ("artifact_delta", {"file": 1}),
            (
                "compaction",
                {
                    "start_timestamp": 1.0,
                    "end_timestamp": 2.0,
                    "compacted_content": {"role": "model", "parts": [{"text": "hi"}]},
                },
            ),
            ("rewind_before_invocation_id", "old"),
            ("agent_state", {}),
            ("route", "next"),
            ("set_model_response", {}),
        ):
            value = copy.deepcopy(self.value)
            value["history"]["events"][-1]["actions"][field] = changed
            await self.reject(value)

    async def test_shared_and_temporary_scopes_cannot_be_persisted(self):
        for prefix in ("app:", "user:", "temp:"):
            value = copy.deepcopy(self.value)
            value["history"]["state"][prefix + "x"] = 1
            await self.reject(value)
            value = copy.deepcopy(self.value)
            value["history"]["events"][-1]["actions"]["state_delta"][prefix + "x"] = 1
            await self.reject(value)

    async def test_native_model_usage_includes_thoughts_and_tool_prompt_counts(self):
        value = envelope(prior=True, tools=True)
        self.assertEqual(value["completed"]["output"]["usage"]["prompt_tokens"], 24)
        self.assertEqual(value["completed"]["output"]["usage"]["completion_tokens"], 14)
        encode_state(value, **IDENTITY)
        for field, changed in (
            ("prompt_tokens", 36),
            ("completion_tokens", 6),
            ("total_tokens", True),
            ("model", "configured-alias"),
        ):
            bad = copy.deepcopy(value)
            bad["completed"]["output"]["usage"][field] = changed
            await self.reject(bad)
        bad = copy.deepcopy(value)
        bad["history"]["events"][-1]["usage_metadata"]["total_token_count"] = 999
        await self.reject(bad)

    async def test_mixed_models_unpriced_and_missing_native_usage_explicit_zero_allowed(self):
        value = envelope(tools=True)
        value["history"]["events"][1]["model_version"] = "another"
        await self.reject(value)
        del value["completed"]["output"]["usage"]["model"]
        encode_state(value, **IDENTITY)
        value = envelope()
        value["history"]["events"][-1]["usage_metadata"] = None
        value["completed"]["output"]["usage"] = {
            "prompt_tokens": 0,
            "completion_tokens": 0,
            "total_tokens": 0,
            "unmetered": True,
        }
        encode_state(value, **IDENTITY)

    async def test_history_input_output_and_text_receipts_cannot_disagree(self):
        for mutate in (
            lambda v: v["messages"][-1].update(content="fake"),
            lambda v: v["completed"].update(input=state_json([{"role": "user", "content": "different"}])),
            lambda v: v["completed"].update(text_events=["fake"]),
            lambda v: v["completed"]["output"]["google_adk"].update(generation=True),
            lambda v: v["completed"]["output"].update(result={"fake": True}),
        ):
            value = copy.deepcopy(self.value)
            mutate(value)
            await self.reject(value)

    async def test_duplicate_json_keys_and_noncanonical_input_rejected(self):
        raw = state_json(self.value).replace('"v":1', '"v":1,"v":1', 1).encode()
        with self.assertRaises(CompletionDeliveryError):
            await self.store(raw).load("thread-one", **IDENTITY)
        value = copy.deepcopy(self.value)
        value["completed"]["input"] = '[{"role":"user","role":"user","content":"hello"}]'
        await self.reject(value)

    async def test_nonfinite_cycles_nonjson_keys_invalid_unicode_and_permissive_native_serializer_rejected(self):
        for value in (math.nan, {1: "lost"}, {"x": b"binary"}, {"x": "\ud800"}):
            with self.assertRaises((ValueError, UnicodeError)):
                state_json(value)
        cycle = []
        cycle.append(cycle)
        with self.assertRaises(ValueError):
            state_json(cycle)
        session = Session.model_validate_json(state_json(self.value["history"]))
        for bad in (lambda: None, math.inf):
            session.events[-1].actions.state_delta = {"bad": bad}
            with self.assertRaises(ValueError):
                dump_session(session)

    async def test_bounded_receipt_output_and_checkpoint_preflight(self):
        value = copy.deepcopy(self.value)
        value["completed"]["text_events"] = ["x" * (3 << 20)]
        with self.assertRaisesRegex(ValueError, "event limit"):
            encode_state(value, **IDENTITY)
        value = copy.deepcopy(self.value)
        value["completed"]["text_events"] = ["x" * (1 << 20)] * 9
        with self.assertRaisesRegex(ValueError, "8 MiB"):
            encode_state(value, **IDENTITY)
        value = copy.deepcopy(self.value)
        value["history"]["state"]["large"] = "x" * (16 << 20)
        with self.assertRaisesRegex(ValueError, "16 MiB"):
            encode_state(value, **IDENTITY)

    async def test_bound_tenant_run_generation_and_credentials_reach_cas(self):
        tokens = [bind_tenant("tenant-one"), bind_run("run-one", 3), bind_runner("python-google-adk", "secret")]
        try:
            store = self.store(state_json(self.value).encode())
            await store.load("thread-one", **IDENTITY)
            await store.save("thread-one", self.value, '"7"', **IDENTITY)
            headers = self.requests[-1].headers
            self.assertEqual(headers["X-Runkite-Tenant-Id"], "tenant-one")
            self.assertEqual(headers["X-Runkite-Run-Id"], "run-one")
            self.assertEqual(headers["X-Runkite-Generation"], "3")
            self.assertEqual(headers["X-Runner-Token"], "secret")
        finally:
            reset_runner(tokens[2])
            reset_run(tokens[1])
            reset_tenant(tokens[0])


if __name__ == "__main__":
    unittest.main()
