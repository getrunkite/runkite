"""The native session service is run-local and keeps temporary state ephemeral."""
# ruff: noqa: E402

from __future__ import annotations

import os
import sys
import unittest
from contextlib import aclosing
from pathlib import Path

os.environ.setdefault("ADK_SUPPRESS_GEMINI_LITELLM_WARNINGS", "true")
ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from google.adk.agents import LlmAgent
from google.adk.events import Event
from google.adk.events.event_actions import EventActions
from google.adk.models import BaseLlm
from google.adk.models.llm_response import LlmResponse
from google.adk.runners import Runner
from google.adk.sessions.base_session_service import GetSessionConfig
from google.genai import types

from google_adk_adapter.sessions import RunLocalSessionService
from google_adk_adapter.state import dump_session

KEY = {"app_name": "agent", "user_id": "tenant", "session_id": "thread"}


class ServiceTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.service = RunLocalSessionService(**KEY)

    async def test_crud_scoped_identity_and_metadata_listing(self):
        self.assertIsNone(await self.service.get_session(**KEY))
        source = {"nested": {"v": 1}}
        session = await self.service.create_session(**KEY, state=source)
        source["nested"]["v"] = 2
        self.assertEqual(session.state["nested"]["v"], 1)
        self.assertIs(session, await self.service.get_session(**KEY))
        listing = await self.service.list_sessions(app_name="agent", user_id="tenant")
        self.assertEqual(len(listing.sessions), 1)
        self.assertEqual(listing.sessions[0].events, [])
        self.assertEqual(listing.sessions[0].state, {})
        with self.assertRaises(ValueError):
            await self.service.create_session(**KEY)
        for field in KEY:
            bad = {**KEY, field: "foreign"}
            with self.assertRaises(ValueError):
                await self.service.get_session(**bad)
            with self.assertRaises(ValueError):
                await self.service.delete_session(**bad)
        await self.service.delete_session(**KEY)
        self.assertIsNone(await self.service.get_session(**KEY))
        with self.assertRaises(ValueError):
            await self.service.snapshot()

    async def test_shared_state_rejected_on_create_and_before_append(self):
        for prefix in ("app:", "user:"):
            with self.assertRaises(ValueError):
                await self.service.create_session(**KEY, state={prefix + "x": 1})
        session = await self.service.create_session(**KEY)
        for prefix in ("app:", "user:"):
            event = Event(author="agent", invocation_id="run", actions=EventActions(state_delta={prefix + "x": 1}))
            with self.assertRaises(ValueError):
                await self.service.append_event(session, event)
        self.assertEqual(session.events, [])
        with self.assertRaises(NotImplementedError):
            await self.service.get_user_state(app_name="agent", user_id="tenant")

    async def test_temp_state_visible_in_invocation_but_not_snapshot_or_event_delta(self):
        session = await self.service.create_session(**KEY, state={"temp:initial": "ephemeral"})
        event = Event(
            author="agent", invocation_id="run", actions=EventActions(state_delta={"temp:x": 1, "stable": {"v": 2}})
        )
        await self.service.append_event(session, event)
        self.assertEqual(session.state["temp:x"], 1)
        self.assertEqual(session.events[0].actions.state_delta, {"stable": {"v": 2}})
        snap = await self.service.snapshot()
        self.assertEqual(snap.state, {"stable": {"v": 2}})
        snap.state["stable"]["v"] = 99
        self.assertEqual(session.state["stable"]["v"], 2)
        restored = RunLocalSessionService(**KEY, session=await self.service.snapshot())
        self.assertEqual((await restored.get_session(**KEY)).state, {"stable": {"v": 2}})

    async def test_append_equal_redelivery_idempotent_and_distinct_same_id_preserved(self):
        session = await self.service.create_session(**KEY)
        first = Event(
            id="same",
            author="user",
            invocation_id="run",
            content=types.Content(role="user", parts=[types.Part(text="one")]),
        )
        await self.service.append_event(session, first)
        await self.service.append_event(session, first.model_copy(deep=True))
        self.assertEqual(len(session.events), 1)
        other = first.model_copy(deep=True)
        other.content.parts[0].text = "two"
        await self.service.append_event(session, other)
        self.assertEqual(len(session.events), 2)

    async def test_partial_events_not_persisted_and_filtered_reads_cannot_mutate_session(self):
        session = await self.service.create_session(**KEY)
        for timestamp in (1.0, 2.0, 3.0):
            await self.service.append_event(session, Event(author="agent", invocation_id="run", timestamp=timestamp))
        partial = Event(
            author="agent", invocation_id="run", partial=True, actions=EventActions(state_delta={"not_saved": True})
        )
        await self.service.append_event(session, partial)
        self.assertNotIn("not_saved", session.state)
        self.assertEqual(len(session.events), 3)
        filtered = await self.service.get_session(
            **KEY, config=GetSessionConfig(after_timestamp=2.0, num_recent_events=1)
        )
        self.assertEqual([e.timestamp for e in filtered.events], [3.0])
        with self.assertRaises(ValueError):
            await self.service.append_event(filtered, partial.model_copy(update={"partial": False}))
        filtered.events.clear()
        self.assertEqual(len(session.events), 3)
        zero = await self.service.get_session(**KEY, config=GetSessionConfig(num_recent_events=0))
        self.assertEqual(zero.events, [])

    async def test_native_runner_multiturn_tools_and_temp_state_survive_restore_without_private_maps(self):
        calls = []

        def lookup(tool_context) -> dict:
            calls.append((tool_context.state.get("temp:x"), tool_context.state.get("stable", 0)))
            tool_context.state["temp:x"] = "only-this-invocation"
            tool_context.state["stable"] = len(calls)
            return {"type": "approval", "value": "ordinary JSON"}

        # Explicit ToolContext annotation is part of ADK's public tool schema.
        from google.adk.tools.tool_context import ToolContext

        lookup.__annotations__["tool_context"] = ToolContext

        class Model(BaseLlm):
            model: str = "offline"

            async def generate_content_async(self, request, stream=False):
                if not any(p.function_response for p in request.contents[-1].parts or []):
                    yield LlmResponse(
                        content=types.Content(
                            role="model",
                            parts=[types.Part(function_call=types.FunctionCall(id="reused", name="lookup", args={}))],
                        )
                    )
                else:
                    users = sum(
                        item.role == "user" and any(part.text for part in item.parts or []) for item in request.contents
                    )
                    yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text=f"users={users}")]))

        for index in (1, 2):
            if index == 2:
                self.service = RunLocalSessionService(**KEY, session=await self.service.snapshot())
            if await self.service.get_session(**KEY) is None:
                await self.service.create_session(**KEY)
            async with Runner(
                app_name="agent",
                agent=LlmAgent(name="agent", model=Model(), tools=[lookup]),
                session_service=self.service,
            ) as runner:
                async with aclosing(
                    runner.run_async(
                        user_id="tenant",
                        session_id="thread",
                        invocation_id=f"run-{index}",
                        new_message=types.Content(role="user", parts=[types.Part(text="hello")]),
                    )
                ):
                    pass
                # A closed iterator without consumption must not execute a tool.
                self.assertEqual(len(calls), index - 1)
                async with aclosing(
                    runner.run_async(
                        user_id="tenant",
                        session_id="thread",
                        invocation_id=f"run-{index}",
                        new_message=types.Content(role="user", parts=[types.Part(text="hello")]),
                    )
                ) as events:
                    result = [event async for event in events]
                self.assertEqual(result[-1].content.parts[0].text, f"users={index}")
        self.assertEqual(calls, [(None, 0), (None, 1)])
        saved = dump_session(await self.service.snapshot())
        self.assertEqual(saved["state"], {"stable": 2})
        self.assertEqual(len(saved["events"]), 8)


if __name__ == "__main__":
    unittest.main()
