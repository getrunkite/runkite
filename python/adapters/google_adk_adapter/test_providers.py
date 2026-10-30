"""Actual Gemini and google-genai HTTP/SSE codecs over an offline transport."""
# ruff: noqa: E402

from __future__ import annotations

import asyncio
import base64
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

os.environ.setdefault("ADK_SUPPRESS_GEMINI_LITELLM_WARNINGS", "true")
ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

import httpx
from google import genai
from google.adk.agents import LlmAgent
from google.adk.models import Gemini
from google.genai import types
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

from google_adk_adapter.adapter import GoogleAdkAdapter
from google_adk_adapter.state import encode_state
from google_adk_adapter.test_adapter import Plane


def wire(*, text: str = "Hello", tool: bool = False, model: str = "gemini-wire", signature: bool = False) -> bytes:
    first = {"text": text[:2]}
    if signature:
        first["thoughtSignature"] = "/wA="
    tail = [{"text": text[2:]}]
    if tool:
        tail.append({"functionCall": {"id": "reused-call", "name": "lookup", "args": {"query": "runkite"}}})
    values = [
        {"candidates": [{"index": 0, "content": {"role": "model", "parts": [first]}}], "modelVersion": model},
        {
            "candidates": [{"index": 0, "content": {"role": "model", "parts": tail}, "finishReason": "STOP"}],
            "usageMetadata": {
                "promptTokenCount": 10,
                "toolUsePromptTokenCount": 2,
                "candidatesTokenCount": 3,
                "thoughtsTokenCount": 4,
                "totalTokenCount": 19,
            },
            "modelVersion": model,
        },
    ]
    return "".join("data: " + json.dumps(value) + "\n\n" for value in values).encode()


class GeminiWireTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.plane = Plane()
        self.events = []
        self.requests = []
        self.adapter = GoogleAdkAdapter()
        self.adapter.configure(http_address="http://plane")
        original = self.adapter._state

        def store():
            value = original()
            value._client._transport = self.plane.transport()
            return value

        self.adapter._state = store
        quiet = patch("google_adk_adapter.adapter.logger.exception")
        quiet.start()
        self.addCleanup(quiet.stop)

    def model(self, replies):
        iterator = iter(replies)

        def respond(request):
            self.assertEqual(request.url.host, "provider.invalid")
            self.requests.append(json.loads(request.content))
            result = next(iterator)
            if isinstance(result, int):
                return httpx.Response(
                    result,
                    json={"error": {"code": result, "message": "synthetic provider failure", "status": "UNAVAILABLE"}},
                )
            return httpx.Response(200, headers={"Content-Type": "text/event-stream"}, content=result)

        http = httpx.AsyncClient(transport=httpx.MockTransport(respond))
        client = genai.Client(
            api_key="synthetic-not-a-secret",
            vertexai=False,
            http_options=types.HttpOptions(
                base_url="https://provider.invalid",
                httpx_async_client=http,
                retry_options=types.HttpRetryOptions(attempts=1),
            ),
        )
        self.addAsyncCleanup(http.aclose)
        self.addAsyncCleanup(client.aio.aclose)
        self.addCleanup(client.close)
        return Gemini(model="requested-alias", client=client)

    async def run_agent(self, *, run_id="run-one", cancel=None, on_event=None):
        self.events = []

        async def emit(event):
            self.events.append(event)
            if on_event is not None:
                await on_event(event)

        tenant, run = bind_tenant("tenant"), bind_run(run_id, 1)
        try:
            return await self.adapter.execute(
                {
                    "graph_id": "agent",
                    "run_id": run_id,
                    "thread_id": "thread",
                    "tenant_id": "tenant",
                    "generation": 1,
                    "input": {"messages": [{"role": "user", "content": "hello"}]},
                },
                emit,
                cancel or asyncio.Event(),
            )
        finally:
            reset_run(run)
            reset_tenant(tenant)

    def final_values(self):
        return next(event["data"] for event in reversed(self.events) if event["method"] == "values")

    async def test_gemini_wire_tools_usage_before_body_and_two_turn_native_history(self):
        model = self.model([wire(text="Checking. ", tool=True, signature=True), wire(text="Done"), wire(text="Again")])
        observed = []

        def lookup(query: str) -> dict:
            charged = self.final_values()["usage"]
            observed.append((query, charged.copy()))
            return {"type": "approval", "value": "ordinary business data"}

        self.adapter.factories["agent"] = lambda context: LlmAgent(name="agent", model=model, tools=[lookup])
        self.assertEqual(await self.run_agent(), "success")
        output = self.final_values()
        self.assertEqual(output["messages"][-1], {"role": "assistant", "content": "Checking. Done"})
        self.assertEqual(
            output["usage"], {"prompt_tokens": 24, "completion_tokens": 14, "total_tokens": 38, "model": "gemini-wire"}
        )
        self.assertEqual(
            observed,
            [("runkite", {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19, "model": "gemini-wire"})],
        )
        first = json.loads(next(iter(self.plane.blobs.values())))
        encode_state(first, graph_id="agent", app_name="agent", user_id="tenant", session_id="thread")
        self.assertEqual(len(first["history"]["events"]), 4)
        self.assertEqual(await self.run_agent(run_id="run-two"), "success")
        self.assertEqual(
            self.final_values()["usage"],
            {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19, "model": "gemini-wire"},
        )
        contents = self.requests[-1]["contents"]
        self.assertEqual(sum(any(part.get("text") == "hello" for part in content["parts"]) for content in contents), 2)
        native_call = next(part for content in contents for part in content["parts"] if part.get("functionCall"))
        self.assertEqual(native_call["functionCall"]["name"], "lookup")
        signatures = [
            part["thoughtSignature"] for content in contents for part in content["parts"] if "thoughtSignature" in part
        ]
        self.assertIn(b"\xff\x00", [base64.urlsafe_b64decode(item) for item in signatures])
        self.assertEqual(len(observed), 1)

    async def test_mixed_real_model_versions_keep_tokens_without_false_model_label(self):
        model = self.model([wire(text="Checking", tool=True, model="wire-a"), wire(text="Done", model="wire-b")])

        def lookup(query: str) -> str:
            return query

        self.adapter.factories["agent"] = lambda context: LlmAgent(name="agent", model=model, tools=[lookup])
        self.assertEqual(await self.run_agent(), "success")
        self.assertEqual(
            self.final_values()["usage"], {"prompt_tokens": 24, "completion_tokens": 14, "total_tokens": 38}
        )
        self.assertEqual(self.plane.puts, 1)

    async def test_provider_error_after_local_tool_preserves_known_usage_without_false_commit(self):
        model = self.model([wire(text="Checking", tool=True), 503])
        effects = []

        def lookup(query: str) -> str:
            effects.append(query)
            return "ok"

        self.adapter.factories["agent"] = lambda context: LlmAgent(name="agent", model=model, tools=[lookup])
        self.assertEqual(await self.run_agent(), "error")
        self.assertEqual(effects, ["runkite"])
        self.assertEqual(
            self.final_values()["usage"],
            {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19, "model": "gemini-wire"},
        )
        self.assertEqual(self.plane.puts, 0)
        self.assertFalse(self.plane.blobs)
        self.assertTrue(any(event["method"] == "error" for event in self.events))

    async def test_cancel_on_observed_wire_usage_prevents_tool_and_keeps_charged_tokens(self):
        model = self.model([wire(text="Checking", tool=True)])
        effects = []

        def lookup(query: str) -> str:
            effects.append(query)
            return "ok"

        self.adapter.factories["agent"] = lambda context: LlmAgent(name="agent", model=model, tools=[lookup])
        cancel = asyncio.Event()

        async def on_event(event):
            if event["method"] == "values" and event["data"].get("usage", {}).get("total_tokens"):
                cancel.set()

        self.assertEqual(await self.run_agent(cancel=cancel, on_event=on_event), "interrupted")
        self.assertEqual(effects, [])
        self.assertEqual(
            self.final_values()["usage"],
            {"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19, "model": "gemini-wire"},
        )
        self.assertEqual(self.plane.puts, 0)

    async def test_initial_provider_auth_failure_cannot_create_a_success_receipt(self):
        model = self.model([401])
        self.adapter.factories["agent"] = lambda context: LlmAgent(name="agent", model=model)
        self.assertEqual(await self.run_agent(), "error")
        self.assertEqual(self.plane.puts, 0)
        self.assertEqual(
            self.final_values()["usage"],
            {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "unmetered": True},
        )

    async def test_native_sdk_spans_are_recorded_without_prompt_tool_or_output_content(self):
        # Set the tracer before importing ADK. A subprocess isolates OTel's
        # process-global provider from other unittest modules and user settings.
        code = r"""
import asyncio,json
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
exporter=InMemorySpanExporter()
provider=TracerProvider()
provider.add_span_processor(SimpleSpanProcessor(exporter))
trace.set_tracer_provider(provider)
from google.adk.agents import LlmAgent
from google.adk.models import BaseLlm,LlmResponse
from google.genai import types
from google_adk_adapter.adapter import GoogleAdkAdapter
from google_adk_adapter.test_adapter import Plane
from runkite_runner.tenant_ctx import bind_tenant,bind_run,reset_tenant,reset_run
class Model(BaseLlm):
    model:str='offline-privacy'
    async def generate_content_async(self,request,stream=False):
        if not any(p.function_response for p in request.contents[-1].parts or []):
            parts=[types.Part(function_call=types.FunctionCall(id='call',name='lookup',args={'query':'PRIVATE_ARGS_SENTINEL_719'}))]
        else:
            parts=[types.Part(text='PRIVATE_ANSWER_SENTINEL_719')]
        yield LlmResponse(model_version='offline-privacy',content=types.Content(role='model',parts=parts),usage_metadata=types.GenerateContentResponseUsageMetadata(prompt_token_count=10,candidates_token_count=2,total_token_count=12))
def lookup(query:str)->dict:return {'result':'PRIVATE_TOOL_SENTINEL_719'}
async def main():
    plane=Plane();adapter=GoogleAdkAdapter();adapter.configure(http_address='http://plane')
    original=adapter._state
    def store():
        value=original();value._client._transport=plane.transport();return value
    adapter._state=store
    adapter.factories['agent']=lambda context:LlmAgent(name='privacy_agent',model=Model(),tools=[lookup])
    tenant,run=bind_tenant('tenant'),bind_run('run',1)
    async def emit(event):pass
    try:
        result=await adapter.execute({'graph_id':'agent','run_id':'run','thread_id':'thread','tenant_id':'tenant','generation':1,'input':{'messages':[{'role':'user','content':'PRIVATE_PROMPT_SENTINEL_719'}]}},emit,asyncio.Event())
        assert result=='success',result
    finally:reset_run(run);reset_tenant(tenant)
    provider.force_flush()
    spans=exporter.get_finished_spans()
    assert spans,'native SDK produced no spans'
    payload=json.dumps([{'name':s.name,'attributes':dict(s.attributes or {}),'events':[{'name':e.name,'attributes':dict(e.attributes or {})} for e in s.events]} for s in spans],default=str)
    for marker in ('PRIVATE_PROMPT_SENTINEL_719','PRIVATE_ARGS_SENTINEL_719','PRIVATE_TOOL_SENTINEL_719','PRIVATE_ANSWER_SENTINEL_719'):
        assert marker not in payload,marker+' leaked in native spans'
    print('privacy-spans='+str(len(spans)))
asyncio.run(main())
provider.shutdown()
"""
        env = {
            **os.environ,
            "PYTHONDONTWRITEBYTECODE": "1",
            "PYTHONPATH": os.pathsep.join([str(ROOT), str(ROOT / "adapters")]),
            "OTEL_SDK_DISABLED": "false",
        }
        result = await asyncio.to_thread(
            subprocess.run, [sys.executable, "-c", code], env=env, capture_output=True, text=True, timeout=30
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("privacy-spans=", result.stdout)


if __name__ == "__main__":
    unittest.main()
