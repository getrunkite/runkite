"""Native state must be safe to restore before any managed agent work starts."""

from __future__ import annotations

import copy
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

import httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from agent_framework import AgentSession, Content, Message  # noqa: E402
from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient  # noqa: E402
from runkite_runner.tenant_ctx import (  # noqa: E402
    bind_run,
    bind_runner,
    bind_tenant,
    reset_run,
    reset_runner,
    reset_tenant,
)

from agent_framework_adapter.state import FRAMEWORK, AgentFrameworkState, state_json  # noqa: E402


def envelope() -> dict:
    session = AgentSession(session_id="native-session")
    session.state = {
        "history": {
            "messages": [
                Message(role="user", contents=[Content.from_text("hello")]),
                Message(role="assistant", contents=[Content.from_text("Hello")]),
            ]
        }
    }
    messages = [{"role": "user", "content": "hello"}, {"role": "assistant", "content": "Hello"}]
    return {
        "v": 1,
        "framework": FRAMEWORK,
        "graph_id": "agent-one",
        "session": session.to_dict(),
        "messages": messages,
        "completed": {
            "run_id": "run-one",
            "input": state_json([messages[0]]),
            "status": "success",
            "output": {
                "messages": copy.deepcopy(messages),
                "agent_framework": {"framework": FRAMEWORK, "generation": 1, "session_id": session.session_id},
                "usage": {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14},
            },
            "text_events": ["Hel", "lo"],
        },
    }


class NativeStateTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.requests: list[httpx.Request] = []
        self.clients: list[OpaqueCheckpointClient] = []

    async def asyncTearDown(self) -> None:
        for client in self.clients:
            await client.aclose()

    def store(self, body: bytes, *, headers: dict[str, str] | None = None, status: int = 200) -> AgentFrameworkState:
        def respond(request: httpx.Request) -> httpx.Response:
            self.requests.append(request)
            if request.method == "GET":
                response_headers = {"ETag": '"1"', "X-Runkite-Checkpoint-Framework": FRAMEWORK}
                if headers is not None:
                    response_headers = headers
                return httpx.Response(status, content=body, headers=response_headers)
            return httpx.Response(204)

        client = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            transport=httpx.MockTransport(respond),
        )
        self.clients.append(client)
        return AgentFrameworkState(client)

    async def test_valid_native_history_restores_public_message_types(self) -> None:
        value = envelope()
        store = self.store(state_json(value).encode())
        loaded, etag = await store.load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)
        self.assertEqual(etag, '"1"')
        restored = AgentSession.from_dict(loaded["session"])
        self.assertIsInstance(restored.state["history"]["messages"][0], Message)
        self.assertEqual(restored.state["history"]["messages"][0].text, "hello")

    async def test_old_interrupted_state_without_a_success_receipt_fails_closed(self) -> None:
        value = envelope()
        del value["completed"]
        store = self.store(state_json(value).encode())
        with self.assertRaises(CompletionDeliveryError):
            await store.load("thread-one", graph_id="agent-one")

    async def test_native_approval_content_cannot_hide_behind_an_old_success_receipt(self) -> None:
        call = Content.from_function_call(call_id="tool-one", name="echo", arguments={"text": "hello"})
        for approval in (
            Content.from_function_approval_request(id="approval-one", function_call=call),
            Content.from_function_approval_response(approved=True, id="approval-one", function_call=call),
            Content.from_oauth_consent_request("https://example.invalid/consent"),
        ):
            with self.subTest(type=approval.type):
                value = envelope()
                native = AgentSession.from_dict(value["session"])
                native.state["history"]["messages"].append(Message(role="assistant", contents=[approval]))
                value["session"] = native.to_dict()
                with self.assertRaises(CompletionDeliveryError):
                    await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")

    async def test_business_json_approval_words_are_not_mistaken_for_native_control_content(self) -> None:
        value = envelope()
        value["session"]["state"]["document"] = {"type": "function_approval_request", "description": "ordinary JSON"}
        loaded, _etag = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)

    async def test_unresolved_native_tool_calls_cannot_hide_behind_a_success_receipt(self) -> None:
        value = envelope()
        session = AgentSession.from_dict(value["session"])
        session.state["history"]["messages"].append(
            Message(
                role="assistant",
                contents=[Content.from_function_call(call_id="tool-one", name="echo", arguments={})],
            )
        )
        value["session"] = session.to_dict()
        with self.assertRaises(CompletionDeliveryError):
            await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")

    async def test_resolved_and_informational_native_tool_history_remains_loadable(self) -> None:
        value = envelope()
        session = AgentSession.from_dict(value["session"])
        session.state["history"]["messages"].extend(
            [
                Message(
                    role="assistant",
                    contents=[Content.from_function_call(call_id="tool-one", name="echo", arguments={})],
                ),
                Message(role="tool", contents=[Content.from_function_result(call_id="tool-one", result={"ok": True})]),
                Message(
                    role="assistant",
                    contents=[
                        Content.from_function_call(call_id="info-one", name="provider-report", informational_only=True)
                    ],
                ),
            ]
        )
        value["session"] = session.to_dict()
        loaded, _etag = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)

    async def test_missing_state_is_the_only_case_that_has_no_etag(self) -> None:
        store = self.store(b"", headers={}, status=404)
        self.assertEqual(await store.load("thread-one", graph_id="agent-one"), (None, None))

    async def test_missing_and_invalid_etags_cannot_reach_a_state_commit(self) -> None:
        invalid = [None, "", "1", 'W/"1"', '"0"', '"01"', '"-1"', '"+1"', '"١"', '"9223372036854775808"', ' "1" ']
        for etag in invalid:
            with self.subTest(etag=etag):
                headers = {"X-Runkite-Checkpoint-Framework": FRAMEWORK}
                if etag is not None:
                    headers["ETag"] = etag
                # httpx only accepts ASCII HTTP header bytes; construct the non-ASCII case explicitly.
                if etag == '"١"':

                    class InvalidVersionClient:
                        async def get_versioned(self, _thread_id, version=etag):
                            return state_json(envelope()).encode(), version

                    store = AgentFrameworkState(InvalidVersionClient())
                else:
                    store = self.store(state_json(envelope()).encode(), headers=headers)
                with self.assertRaises(CompletionDeliveryError):
                    await store.load("thread-one", graph_id="agent-one")
        self.assertTrue(all(request.method == "GET" for request in self.requests))

    async def test_http_framework_must_match_even_for_an_otherwise_valid_body(self) -> None:
        for framework in (None, "langchain", "Agent-Framework"):
            with self.subTest(framework=framework):
                headers = {"ETag": '"1"'}
                if framework is not None:
                    headers["X-Runkite-Checkpoint-Framework"] = framework
                store = self.store(state_json(envelope()).encode(), headers=headers)
                with self.assertRaises(CompletionDeliveryError):
                    await store.load("thread-one", graph_id="agent-one")

    async def test_envelope_version_framework_and_agent_are_exact(self) -> None:
        for key, values in {
            "v": [None, True, 0, 2, "1"],
            "framework": [None, "langchain", "Agent-Framework"],
            "graph_id": [None, "another-agent", ""],
        }.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    invalid = envelope()
                    invalid[key] = value
                    store = self.store(state_json(invalid).encode())
                    with self.assertRaises(CompletionDeliveryError):
                        await store.load("thread-one", graph_id="agent-one")

    async def test_generic_message_history_cannot_be_silently_replaced_by_a_blank_native_session(self) -> None:
        value = {"v": 1, "messages": [{"role": "user", "content": "must remain available"}]}
        store = self.store(state_json(value).encode())
        with self.assertRaises(CompletionDeliveryError):
            await store.load("thread-one", graph_id="agent-one")
        self.assertEqual(len(self.requests), 1)

    async def test_missing_or_invalid_native_sessions_fail_before_execution(self) -> None:
        for session in (
            None,
            {},
            {"type": "session", "session_id": "", "state": {}},
            {"type": "other", "session_id": "native-session", "state": {}},
            {"type": "session", "session_id": "native-session", "state": []},
            {"type": "session", "session_id": "native-session", "state": {}, "service_session_id": 7},
        ):
            with self.subTest(session=session):
                value = envelope()
                value["session"] = session
                store = self.store(state_json(value).encode())
                with self.assertRaises(CompletionDeliveryError):
                    await store.load("thread-one", graph_id="agent-one")

    async def test_native_codec_errors_are_detected_before_the_factory_runs(self) -> None:
        value = envelope()
        value["session"]["state"] = {"native": {"type": "message", "role": "user", "contents": 17}}
        store = self.store(state_json(value).encode())
        with self.assertRaises(CompletionDeliveryError):
            await store.load("thread-one", graph_id="agent-one")

    async def test_invalid_receipts_cannot_be_reported_as_success(self) -> None:
        cases = []
        for key, invalid_values in {
            "status": [None, "error", "interrupted"],
            "run_id": [None, ""],
            "input": [None, "not-json", "[]", '[{"role":"assistant","content":"hello"}]'],
            "output": [None, []],
            "text_events": [None, [3]],
        }.items():
            for invalid in invalid_values:
                value = envelope()
                value["completed"][key] = invalid
                cases.append(value)
        cases.append({**envelope(), "completed": None})
        value = envelope()
        value["completed"]["text_events"] = ["a different result"]
        cases.append(value)
        value = envelope()
        value["completed"]["output"]["messages"] = []
        cases.append(value)
        for value in cases:
            with self.subTest(completed=value["completed"]):
                store = self.store(state_json(value).encode())
                with self.assertRaises(CompletionDeliveryError):
                    await store.load("thread-one", graph_id="agent-one")

    async def test_receipt_metadata_and_usage_must_match_native_state(self) -> None:
        for key, invalid in (
            ("framework", "langchain"),
            ("generation", -1),
            ("generation", True),
            ("session_id", "other"),
        ):
            with self.subTest(key=key, value=invalid):
                value = envelope()
                value["completed"]["output"]["agent_framework"][key] = invalid
                with self.assertRaises(CompletionDeliveryError):
                    await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        for usage in (
            {},
            {"prompt_tokens": -1, "completion_tokens": 1, "total_tokens": 0},
            {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 1},
        ):
            with self.subTest(usage=usage):
                value = envelope()
                value["completed"]["output"]["usage"] = usage
                with self.assertRaises(CompletionDeliveryError):
                    await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")

    async def test_malformed_unicode_non_json_numbers_and_duplicate_fields_fail_closed(self) -> None:
        for body in (b"\xff", b'{"v":1,"v":2}', b'{"value":NaN}', b'{"value":"\\ud800"}'):
            with self.subTest(body=body):
                with self.assertRaises(CompletionDeliveryError):
                    await self.store(body).load("thread-one", graph_id="agent-one")

    async def test_valid_save_uses_cas_or_create_only_and_invalid_state_never_writes(self) -> None:
        store = self.store(b"", status=404)
        await store.save("thread-one", envelope(), '"7"', graph_id="agent-one")
        await store.save("thread-one", envelope(), None, graph_id="agent-one")
        self.assertEqual(self.requests[0].headers["If-Match"], '"7"')
        self.assertNotIn("If-None-Match", self.requests[0].headers)
        self.assertEqual(self.requests[1].headers["If-None-Match"], "*")
        self.assertNotIn("If-Match", self.requests[1].headers)
        with self.assertRaises(CompletionDeliveryError):
            await store.save("thread-one", envelope(), '"7"', graph_id="another-agent")
        self.assertEqual(len(self.requests), 2)

    async def test_state_close_releases_the_owned_http_client(self) -> None:
        store = self.store(state_json(envelope()).encode())
        await store.load("thread-one", graph_id="agent-one")
        client = self.clients[-1]._client
        self.assertIsNotNone(client)
        await store.aclose()
        self.assertTrue(client.is_closed)
        self.assertIsNone(self.clients[-1]._client)

    async def test_checkpoint_limits_fail_before_decoding_or_put(self) -> None:
        store = self.store(b"x" * ((16 << 20) + 1))
        with self.assertRaises(CompletionDeliveryError) as loaded:
            await store.load("thread-one", graph_id="agent-one")
        self.assertIn("exceeds", str(loaded.exception.__cause__))
        self.requests.clear()
        value = envelope()
        value["session"]["state"]["large"] = "x" * (16 << 20)
        with self.assertRaises(CompletionDeliveryError) as saved:
            await store.save("thread-one", value, '"1"', graph_id="agent-one")
        self.assertIn("exceeds", str(saved.exception.__cause__))
        self.assertEqual(self.requests, [])

    async def test_a_version_conflict_never_retries_as_an_unconditional_write(self) -> None:
        requests = []
        client = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            transport=httpx.MockTransport(lambda request: requests.append(request) or httpx.Response(412)),
        )
        self.clients.append(client)
        store = AgentFrameworkState(client)
        with self.assertRaises(CompletionDeliveryError):
            await store.save("thread-one", envelope(), '"7"', graph_id="agent-one")
        self.assertEqual(len(requests), 1)
        self.assertEqual(requests[0].headers["If-Match"], '"7"')
        self.assertNotIn("If-None-Match", requests[0].headers)


class OpaqueCompatibilityTests(unittest.IsolatedAsyncioTestCase):
    async def test_versioned_reads_accept_the_exact_positive_int64_range(self) -> None:
        for version in ('"1"', '"9223372036854775807"'):
            with self.subTest(version=version):
                client = OpaqueCheckpointClient(
                    http_base_url="http://plane",
                    framework=FRAMEWORK,
                    transport=httpx.MockTransport(
                        lambda _request, etag=version: httpx.Response(
                            200,
                            content=b"data",
                            headers={"ETag": etag, "X-Runkite-Checkpoint-Framework": FRAMEWORK},
                        )
                    ),
                )
                try:
                    self.assertEqual(await client.get_versioned("thread-one"), (b"data", version))
                finally:
                    await client.aclose()

    async def test_legacy_raw_get_put_remain_header_permissive_and_unconditional(self) -> None:
        requests = []

        def respond(request):
            requests.append(request)
            return httpx.Response(200, content=b"legacy bytes")

        client = OpaqueCheckpointClient(
            http_base_url="http://plane", framework="legacy", transport=httpx.MockTransport(respond)
        )
        try:
            self.assertEqual(await client.get("thread-one"), b"legacy bytes")
            await client.put("thread-one", b"replacement")
            self.assertNotIn("If-Match", requests[-1].headers)
            self.assertNotIn("If-None-Match", requests[-1].headers)
        finally:
            await client.aclose()

    async def test_versioned_put_rejects_invalid_versions_without_making_requests(self) -> None:
        requests = []
        client = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            transport=httpx.MockTransport(lambda request: requests.append(request) or httpx.Response(204)),
        )
        try:
            for etag in ('"0"', '"01"', '"-1"', 'W/"1"', '"١"', '"9223372036854775808"'):
                with self.subTest(etag=etag):
                    with self.assertRaises(ValueError):
                        await client.put_cas("thread-one", b"data", etag=etag)
            with self.assertRaises(ValueError):
                await client.put_cas("thread-one", b"data", etag='"1"', create_only=True)
            self.assertEqual(requests, [])
        finally:
            await client.aclose()

    async def test_bound_runner_identity_and_explicit_credentials_keep_run_tenant_headers(self) -> None:
        requests = []
        transport = httpx.MockTransport(lambda request: requests.append(request) or httpx.Response(404))
        client = OpaqueCheckpointClient(http_base_url="http://plane", framework=FRAMEWORK, transport=transport)
        explicit = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            runner_kind="explicit-kind",
            runner_token="explicit-token",
            transport=transport,
        )
        tenant = bind_tenant("tenant-one")
        run = bind_run("run-one", 4)
        runner = bind_runner("python-agent-framework", "bound-token")
        try:
            with patch.dict("os.environ", {"RUNNER_TOKEN": "environment-token"}):
                await client.get_versioned("thread-one")
                await explicit.get_versioned("thread-one")
            for request in requests:
                self.assertEqual(request.headers["X-Runkite-Tenant-Id"], "tenant-one")
                self.assertEqual(request.headers["X-Runkite-Run-Id"], "run-one")
                self.assertEqual(request.headers["X-Runkite-Generation"], "4")
            self.assertEqual(requests[0].headers["X-Runner-Kind"], "python-agent-framework")
            self.assertEqual(requests[0].headers["X-Runner-Token"], "bound-token")
            self.assertEqual(requests[1].headers["X-Runner-Kind"], "explicit-kind")
            self.assertEqual(requests[1].headers["X-Runner-Token"], "explicit-token")
        finally:
            reset_runner(runner)
            reset_run(run)
            reset_tenant(tenant)
            await client.aclose()
            await explicit.aclose()


class StateJsonTests(unittest.TestCase):
    def test_unsupported_values_cannot_silently_change_during_serialization(self) -> None:
        cycle = []
        cycle.append(cycle)
        for value in (
            {1: "key"},
            {"tuple": (1, 2)},
            {"set": {1}},
            {"bytes": b"x"},
            {"text": "\ud800"},
            {"bad": float("nan")},
            cycle,
        ):
            with self.subTest(type=type(value).__name__):
                with self.assertRaises((ValueError, TypeError, UnicodeError)):
                    state_json(value)


if __name__ == "__main__":
    unittest.main()
