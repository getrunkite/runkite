"""Offline integration checks against the pinned, installed Agent Framework core.

Run from the repository root with PYTHONPATH=python. No provider SDK, API key,
model endpoint, or running Runkite server is required.
"""

from __future__ import annotations

import asyncio
import copy
import json
import threading
import unittest
from typing import Any

from agent_framework import (
    Agent,
    BaseChatClient,
    ChatResponse,
    ChatResponseUpdate,
    Content,
    FunctionInvocationLayer,
    FunctionMiddleware,
    FunctionTool,
    Message,
    MiddlewareFailure,
    ResponseStream,
)
from runkite_governance.agent_framework import (
    GovernanceBlockedError,
    GovernanceFunctionMiddleware,
    GovernedFunction,
)


class ScriptedClient(FunctionInvocationLayer, BaseChatClient):
    """Supply model messages while retaining the framework's actual tool loop."""

    def __init__(self, arguments: dict[str, Any], *, name: str = "transfer") -> None:
        super().__init__()
        self.arguments = arguments
        self.name = name
        self.turns = 0

    def _inner_get_response(self, *, messages, stream, options, **kwargs):
        self.turns += 1
        turn = self.turns
        if turn == 1:
            contents = [
                Content.from_function_call(
                    call_id="provider-call",
                    name=self.name,
                    arguments=json.dumps(self.arguments),
                )
            ]
        else:
            contents = [Content.from_text("done")]
        if stream:

            async def updates():
                yield ChatResponseUpdate(role="assistant", contents=contents, response_id=f"response-{turn}")

            return ResponseStream(updates(), finalizer=ChatResponse.from_updates)

        async def response():
            return ChatResponse(messages=[Message("assistant", contents)], response_id=f"response-{turn}")

        return response()


class DecisionClient:
    """Fake only the governance HTTP boundary, never framework invocation."""

    def __init__(self, decision: dict[str, Any] | None = None, operation=None) -> None:
        self.decision = decision if decision is not None else {"effect": "allow"}
        self.operation = operation
        self.calls: list[dict[str, Any]] = []
        self.started = threading.Event()

    def decide(self, **kwargs):
        self.calls.append(kwargs)
        self.started.set()
        if self.operation is not None:
            return self.operation(kwargs)
        return self.decision

    def action(self, *args, **kwargs):
        raise AssertionError("continuation belongs to the application")

    def resolve(self, *args, **kwargs):
        raise AssertionError("middleware must make only one decide call")


class CaptureArguments(FunctionMiddleware):
    """Observe public context references before the guard receives them."""

    async def process(self, context, call_next):
        self.context = context
        self.original_arguments = context.arguments
        await call_next()


class GovernanceAgentFrameworkIntegrationTests(unittest.IsolatedAsyncioTestCase):
    async def execute(self, agent: Agent, stream: bool):
        async def consume():
            if stream:
                response_stream = agent.run("transfer", stream=True)
                return [update async for update in response_stream]
            return await agent.run("transfer")

        return await asyncio.wait_for(consume(), timeout=3)

    def make_tool(self, effects, *, approval_mode="never_require"):
        def transfer(amount: int, currency: str = "USD") -> str:
            effects.append({"amount": amount, "currency": currency})
            return "transferred"

        return FunctionTool(name="transfer", func=transfer, approval_mode=approval_mode)

    def make_details_tool(self, effects):
        def transfer(details: dict[str, Any]) -> str:
            effects.append(copy.deepcopy(details))
            return "transferred"

        return FunctionTool(name="transfer", func=transfer)

    def make_nested_type_checked_tool(self, effects):
        def transfer(details: dict[str, Any]) -> str:
            self.assertIs(type(details), dict)
            self.assertIs(type(details["tags"]), list)
            self.assertIs(type(details["tags"][1]), dict)
            self.assertIs(type(details["tags"][1]["audit"]), list)
            self.assertIs(type(details["tags"][1]["audit"][0]), dict)
            self.assertIs(type(details["route"]), dict)
            self.assertIs(type(details["route"]["backup"]), list)
            effects.append(copy.deepcopy(details))
            return "transferred"

        return FunctionTool(name="transfer", func=transfer)

    def guard(self, client, function, **kwargs):
        return GovernanceFunctionMiddleware(
            client,
            agent_id="server-agent",
            functions=[GovernedFunction(function=function, connector="payments")],
            operation_id=lambda context: "server-turn:" + context.metadata["call_id"],
            **kwargs,
        )

    async def test_allow_normalizes_defaults_and_preserves_real_tool_execution(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                tool = self.make_tool(effects)
                client = DecisionClient()
                agent = Agent(
                    client=ScriptedClient({"amount": 3}),
                    tools=[tool],
                    middleware=[self.guard(client, tool)],
                )
                await self.execute(agent, stream)
                self.assertEqual(effects, [{"amount": 3, "currency": "USD"}])
                self.assertEqual(len(client.calls), 1)
                call = client.calls[0]
                self.assertEqual(call["args"], {"amount": 3, "currency": "USD"})
                self.assertEqual(call["agent_id"], "server-agent")
                self.assertEqual(call["connector"], "payments")
                self.assertEqual(call["tool"], "transfer")
                self.assertEqual(call["run_id"], "server-turn:provider-call")

    async def test_allow_preserves_nested_dict_and_list_types_and_policy_arguments(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                tool = self.make_nested_type_checked_tool(effects)
                arguments = {
                    "details": {
                        "destination": "trusted",
                        "tags": ["one", {"audit": [{"approved": True}]}],
                        "route": {"backup": ["local"]},
                    }
                }
                client = DecisionClient()
                agent = Agent(
                    client=ScriptedClient(arguments),
                    tools=[tool],
                    middleware=[self.guard(client, tool)],
                )
                await self.execute(agent, stream)
                self.assertEqual(len(client.calls), 1)
                self.assertEqual(client.calls[0]["args"], arguments)
                self.assertEqual(effects, [arguments["details"]])
                self.assertEqual(effects[0], client.calls[0]["args"]["details"])

    async def test_deny_is_fatal_before_any_tool_side_effect(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                tool = self.make_tool(effects)
                decision = {"effect": "deny", "reason": "not permitted", "reason_code": "policy"}
                client = DecisionClient(decision)
                agent = Agent(
                    client=ScriptedClient({"amount": 3}),
                    tools=[tool],
                    middleware=[self.guard(client, tool)],
                )
                with self.assertRaises(GovernanceBlockedError) as raised:
                    await self.execute(agent, stream)
                self.assertIsInstance(raised.exception, MiddlewareFailure)
                self.assertEqual(raised.exception.decision["effect"], "deny")
                self.assertEqual(raised.exception.decision["reason_code"], "policy")
                decision["reason"] = "changed after delivery"
                self.assertEqual(raised.exception.decision["reason"], "not permitted")
                self.assertEqual(effects, [])
                self.assertEqual(len(client.calls), 1)

    async def test_pending_persists_an_immutable_continuation_without_execution(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                tool = self.make_details_tool(effects)
                client = DecisionClient({"effect": "pending", "action_id": "approval-1"})
                persisted = []

                async def persist(invocation, persisted=persisted, effects=effects):
                    persisted.append(invocation)
                    self.assertEqual(effects, [])

                args = {"details": {"destination": "trusted", "tags": ["one"]}}
                agent = Agent(
                    client=ScriptedClient(args),
                    tools=[tool],
                    middleware=[self.guard(client, tool, on_pending=persist)],
                )
                with self.assertRaises(GovernanceBlockedError) as raised:
                    await self.execute(agent, stream)
                self.assertEqual(raised.exception.decision["effect"], "pending")
                self.assertEqual(len(persisted), 1)
                invocation = persisted[0]
                self.assertEqual(invocation.action_id, "approval-1")
                self.assertEqual(invocation.agent_id, "server-agent")
                self.assertEqual(invocation.connector, "payments")
                self.assertEqual(invocation.tool, "transfer")
                self.assertEqual(invocation.run_id, "server-turn:provider-call")
                self.assertEqual(invocation.arguments["details"]["destination"], "trusted")
                self.assertEqual(invocation.arguments["details"]["tags"], ("one",))
                with self.assertRaises(TypeError):
                    invocation.arguments["details"]["destination"] = "changed"
                self.assertEqual(effects, [])
                self.assertEqual(len(client.calls), 1)

    async def test_native_approval_flag_remains_effective(self):
        effects = []
        tool = self.make_tool(effects, approval_mode="always_require")
        client = DecisionClient()
        agent = Agent(
            client=ScriptedClient({"amount": 3}),
            tools=[tool],
            middleware=[self.guard(client, tool)],
        )
        response = await self.execute(agent, False)
        self.assertEqual(tool.approval_mode, "always_require")
        self.assertTrue(
            any(
                content.type == "function_approval_request"
                for message in response.messages
                for content in message.contents
            )
        )
        self.assertEqual(client.calls, [])
        self.assertEqual(effects, [])

    async def test_original_cached_arguments_mutation_during_decide_is_rejected(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                capture = CaptureArguments()
                tool = self.make_tool(effects)

                def mutate_original(_kwargs, capture=capture):
                    capture.original_arguments["amount"] = 999
                    return {"effect": "allow"}

                client = DecisionClient(operation=mutate_original)
                agent = Agent(
                    client=ScriptedClient({"amount": 3}),
                    tools=[tool],
                    middleware=[capture, self.guard(client, tool)],
                )
                with self.assertRaises(MiddlewareFailure):
                    await self.execute(agent, stream)
                self.assertEqual(effects, [])
                self.assertEqual(len(client.calls), 1)

    async def test_nested_arguments_mutation_during_decide_is_rejected(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                capture = CaptureArguments()
                tool = self.make_details_tool(effects)

                def mutate_nested(_kwargs, capture=capture):
                    capture.original_arguments["details"]["destination"] = "attacker"
                    return {"effect": "allow"}

                client = DecisionClient(operation=mutate_nested)
                agent = Agent(
                    client=ScriptedClient({"details": {"destination": "trusted"}}),
                    tools=[tool],
                    middleware=[capture, self.guard(client, tool)],
                )
                with self.assertRaises(MiddlewareFailure):
                    await self.execute(agent, stream)
                self.assertEqual(effects, [])
                self.assertEqual(len(client.calls), 1)

    async def test_decide_outage_is_fatal_without_tool_execution(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                tool = self.make_tool(effects)

                def unavailable(_kwargs):
                    raise OSError("offline governance boundary")

                client = DecisionClient(operation=unavailable)
                agent = Agent(
                    client=ScriptedClient({"amount": 3}),
                    tools=[tool],
                    middleware=[self.guard(client, tool)],
                )
                with self.assertRaises(MiddlewareFailure):
                    await self.execute(agent, stream)
                self.assertEqual(effects, [])
                self.assertEqual(len(client.calls), 1)

    async def test_cancellation_signals_decide_and_prevents_execution(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                tool = self.make_tool(effects)
                release = threading.Event()

                def blocked(kwargs, release=release):
                    while not release.wait(0.01):
                        if kwargs["cancelled"].is_set():
                            return {"effect": "allow"}
                    return {"effect": "allow"}

                client = DecisionClient(operation=blocked)
                agent = Agent(
                    client=ScriptedClient({"amount": 3}),
                    tools=[tool],
                    middleware=[self.guard(client, tool)],
                )
                task = asyncio.create_task(self.execute(agent, stream))
                try:
                    started = await asyncio.to_thread(client.started.wait, 1)
                    self.assertTrue(started, "decide did not begin")
                    task.cancel()
                    with self.assertRaises(asyncio.CancelledError):
                        await asyncio.wait_for(task, 1)
                    self.assertTrue(client.calls[0]["cancelled"].is_set())
                    self.assertEqual(effects, [])
                    self.assertEqual(len(client.calls), 1)
                finally:
                    release.set()
                    if not task.done():
                        task.cancel()
                        await asyncio.gather(task, return_exceptions=True)

    async def test_another_function_object_with_same_name_is_not_authorized(self):
        for stream in (False, True):
            with self.subTest(stream=stream):
                effects = []
                configured_tool = self.make_tool([])
                actual_tool = self.make_tool(effects)
                client = DecisionClient()
                agent = Agent(
                    client=ScriptedClient({"amount": 3}),
                    tools=[actual_tool],
                    middleware=[self.guard(client, configured_tool)],
                )
                with self.assertRaises(MiddlewareFailure):
                    await self.execute(agent, stream)
                self.assertEqual(client.calls, [])
                self.assertEqual(effects, [])


if __name__ == "__main__":
    unittest.main()
