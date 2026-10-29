"""Security checks for the optional Microsoft middleware, without a model service."""

from __future__ import annotations

import asyncio
import copy
import importlib.util
import math
import os
import sys
import threading
import unittest
from datetime import date
from decimal import Decimal

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
HAS_FRAMEWORK = importlib.util.find_spec("agent_framework") is not None
if HAS_FRAMEWORK:
    from agent_framework import FunctionInvocationContext, FunctionTool, MiddlewareFailure
    from pydantic import BaseModel, field_validator
    from runkite_governance.agent_framework import (
        GovernanceBlockedError,
        GovernanceFunctionMiddleware,
        GovernedFunction,
    )


class DecisionClient:
    def __init__(self, decision=None, callback=None):
        self.decision = {"effect": "allow"} if decision is None else decision
        self.callback = callback
        self.calls = []

    def decide(self, **call):
        self.calls.append(call)
        if self.callback:
            return self.callback(call)
        return self.decision


@unittest.skipUnless(HAS_FRAMEWORK, "optional agent-framework-core dependency is not installed")
class GovernanceMiddlewareTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.effects = []

        async def transfer(amount: int = 10):
            self.effects.append(amount)
            return amount

        self.function = FunctionTool(name="transfer", func=transfer)

    def middleware(self, client, **options):
        return GovernanceFunctionMiddleware(
            client,
            agent_id="payments-app",
            functions=[GovernedFunction(self.function, "payments")],
            operation_id=lambda _context: "turn-1-tool-1",
            **options,
        )

    def context(self, arguments, **options):
        return FunctionInvocationContext(function=self.function, arguments=arguments, **options)

    async def invoke(self, middleware, context):
        async def next_call():
            context.result = await context.function.invoke(context=context, skip_parsing=True)

        await middleware.process(context, next_call)
        return context.result

    async def test_python_defaults_are_policy_visible_without_runtime_credentials(self):
        client = DecisionClient()
        context = self.context({}, kwargs={"credential": "never-policy-input"})
        self.assertEqual(await self.invoke(self.middleware(client), context), 10)
        self.assertEqual(client.calls[0]["args"], {"amount": 10})
        self.assertNotIn("credential", client.calls[0])
        self.assertEqual(self.effects, [10])

    async def test_native_execution_preserves_ordinary_nested_json_containers(self):
        effects = []

        async def transfer(details: dict[str, object]):
            self.assertIs(type(details), dict)
            self.assertIs(type(details["nested"]), dict)
            self.assertIs(type(details["nested"]["tags"]), list)
            self.assertIs(type(details["nested"]["tags"][0]), dict)
            effects.append(copy.deepcopy(details))
            return details

        function = FunctionTool(name="transfer", func=transfer)
        client = DecisionClient()
        middleware = GovernanceFunctionMiddleware(
            client,
            agent_id="payments-app",
            functions=[GovernedFunction(function, "payments")],
            operation_id=lambda _context: "turn-1-tool-1",
        )
        arguments = {"details": {"nested": {"tags": [{"Name": "original"}]}}}
        result = await self.invoke(middleware, FunctionInvocationContext(function=function, arguments=arguments))
        self.assertEqual(result, arguments["details"])
        self.assertEqual(effects, [client.calls[0]["args"]["details"]])

    async def test_lossy_or_typed_arguments_fail_before_the_plane(self):
        cycle = {}
        cycle["cycle"] = cycle

        class CustomDict(dict):
            def items(self):
                raise AssertionError("custom serialization must not run")

        bad = [
            None,
            [],
            {"amount": 2**53},
            {"amount": -0.0},
            {"amount": math.inf},
            {"amount": math.nan},
            {"amount": Decimal("1.0")},
            {"amount": date(2026, 1, 1)},
            {"amount": (1, 2)},
            {1: "not a string key"},
            {"amount": "\ud800"},
            {"\udfff": 1},
            {"amount": CustomDict()},
            cycle,
        ]
        client = DecisionClient()
        middleware = self.middleware(client)
        for arguments in bad:
            with self.subTest(arguments_type=type(arguments).__name__):
                with self.assertRaises(MiddlewareFailure):
                    await self.invoke(middleware, self.context(arguments))
        self.assertEqual(client.calls, [])
        self.assertEqual(self.effects, [])

    async def test_non_idempotent_native_normalization_fails_before_the_plane(self):
        class IncreasingInput(BaseModel):
            amount: int

            @field_validator("amount")
            @classmethod
            def increase(cls, value):
                return value + 1

        function = FunctionTool(name="transfer", func=self.function.func, input_model=IncreasingInput)
        client = DecisionClient()
        middleware = GovernanceFunctionMiddleware(
            client,
            agent_id="payments-app",
            functions=[GovernedFunction(function, "payments")],
            operation_id=lambda _context: "turn-1-tool-1",
        )
        with self.assertRaises(MiddlewareFailure):
            await self.invoke(middleware, FunctionInvocationContext(function=function, arguments={"amount": 1}))
        self.assertEqual(client.calls, [])
        self.assertEqual(self.effects, [])

    async def test_same_name_unregistered_function_is_blocked(self):
        other = FunctionTool(name="transfer", func=self.function.func)
        client = DecisionClient()
        context = FunctionInvocationContext(function=other, arguments={"amount": 1})
        with self.assertRaises(MiddlewareFailure):
            await self.invoke(self.middleware(client), context)
        self.assertEqual(client.calls, [])
        self.assertEqual(self.effects, [])

    async def test_noncanonical_operation_identifiers_are_blocked(self):
        for run_id in ("", " turn-1", "turn-1 ", "\ud800", "🚀" * 65):
            client = DecisionClient()
            middleware = GovernanceFunctionMiddleware(
                client,
                agent_id="payments-app",
                functions=[GovernedFunction(self.function, "payments")],
                operation_id=lambda _context, value=run_id: value,
            )
            with self.assertRaises(MiddlewareFailure):
                await self.invoke(middleware, self.context({"amount": 1}))
            self.assertEqual(client.calls, [])

    async def test_pending_persists_an_immutable_snapshot_then_blocks_without_args_in_error(self):
        pending = []
        response = {"effect": "pending", "action_id": "action-1", "reason_code": "mandatory_hitl"}

        async def persist(invocation):
            pending.append(invocation)
            with self.assertRaises(TypeError):
                invocation.arguments["amount"] = 5000
            response["effect"] = "allow"

        client = DecisionClient(response)
        middleware = self.middleware(client, on_pending=persist)
        with self.assertRaises(GovernanceBlockedError) as blocked:
            await self.invoke(middleware, self.context({"amount": 100}))
        self.assertEqual(pending[0].arguments, {"amount": 100})
        self.assertEqual(pending[0].run_id, "turn-1-tool-1")
        self.assertFalse(hasattr(pending[0], "context"))
        self.assertEqual(blocked.exception.decision["effect"], "pending")
        self.assertNotIn("arguments", vars(blocked.exception))
        self.assertNotIn("args", blocked.exception.decision)
        with self.assertRaises(TypeError):
            blocked.exception.decision["effect"] = "allow"
        self.assertEqual(self.effects, [])

    async def test_pending_persistence_failure_never_executes(self):
        async def persist(_invocation):
            raise OSError("application persistence unavailable")

        middleware = self.middleware(DecisionClient({"effect": "pending", "action_id": "action-1"}), on_pending=persist)
        with self.assertRaises(MiddlewareFailure) as failure:
            await self.invoke(middleware, self.context({"amount": 100}))
        self.assertIsInstance(failure.exception.__cause__, OSError)
        self.assertEqual(self.effects, [])

    async def test_malformed_decisions_fail_closed(self):
        for decision in (
            {},
            {"effect": "approved"},
            {"effect": "pending"},
            {"effect": "allow", "reason": 10},
            {"effect": "pending", "action_id": " "},
        ):
            with self.subTest(decision=decision):
                with self.assertRaises(MiddlewareFailure):
                    await self.invoke(self.middleware(DecisionClient(decision)), self.context({"amount": 1}))
        self.assertEqual(self.effects, [])

    async def test_argument_or_tool_mutation_during_the_decision_is_blocked(self):
        for change in ("args", "reference", "name", "func"):
            context = self.context({"amount": 100})
            original_name, original_func = self.function.name, self.function.func

            def mutate(_call, change=change, context=context):
                if change == "args":
                    context.arguments["amount"] = 5000
                elif change == "reference":
                    context.arguments = {"amount": 100}
                elif change == "name":
                    self.function.name = "other"
                else:
                    self.function.func = lambda amount: amount
                return {"effect": "allow"}

            middleware = self.middleware(DecisionClient(callback=mutate))
            try:
                with self.assertRaises(MiddlewareFailure):
                    await self.invoke(middleware, context)
            finally:
                self.function.name, self.function.func = original_name, original_func
        self.assertEqual(self.effects, [])

    async def test_cancellation_sets_transport_event_and_never_executes(self):
        started, finished = threading.Event(), threading.Event()

        def wait(call):
            started.set()
            call["cancelled"].wait(1)
            finished.set()
            return {"effect": "allow"}

        client = DecisionClient(callback=wait)
        task = asyncio.create_task(self.invoke(self.middleware(client), self.context({"amount": 1})))
        self.assertTrue(await asyncio.to_thread(started.wait, 1))
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertTrue(client.calls[0]["cancelled"].is_set())
        self.assertTrue(await asyncio.to_thread(finished.wait, 1))
        self.assertEqual(self.effects, [])

    async def test_native_tool_errors_propagate_without_retry_or_policy_wrapping(self):
        failure = ValueError("native tool failed")
        executions = 0

        async def next_call():
            nonlocal executions
            executions += 1
            raise failure

        client = DecisionClient()
        with self.assertRaises(ValueError) as caught:
            await self.middleware(client).process(self.context({"amount": 1}), next_call)
        self.assertIs(caught.exception, failure)
        self.assertEqual(executions, 1)
        self.assertEqual(len(client.calls), 1)


if __name__ == "__main__":
    unittest.main()
