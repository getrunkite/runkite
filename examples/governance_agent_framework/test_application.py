"""App continuation tests with the real native framework and no provider calls."""

from __future__ import annotations

import asyncio
import copy
import tempfile
import unittest
from pathlib import Path

from application import NoteStore, build_notes_agent, continue_note
from runkite_governance.agent_framework import GovernanceBlockedError
from test_integration import DecisionClient, ScriptedClient


class SchemaRecordingClient(ScriptedClient):
    """Record tool definitions at the real framework's model-client boundary."""

    def __init__(self, arguments):
        super().__init__(arguments, name="create_note")
        self.tool_schemas = []

    def _inner_get_response(self, *, messages, stream, options, **kwargs):
        self.tool_schemas.append(
            [
                {
                    "name": function.name,
                    "description": function.description,
                    "parameters": copy.deepcopy(function.parameters()),
                }
                for function in options.get("tools", [])
            ]
        )
        return super()._inner_get_response(messages=messages, stream=stream, options=options, **kwargs)


class ApplicationGovernanceClient(DecisionClient):
    def __init__(self, decision):
        super().__init__(decision)
        self.status = "pending"
        self.actions = []

    def action(self, action_id):
        self.actions.append(action_id)
        return {"action_id": action_id, "status": self.status}


class ApplicationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="runkite-framework-app-")
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / "state.db"
        self.store = NoteStore(self.path)
        self.arguments = {"title": "A draft", "body": "The original proposed content"}
        self.expected = {**self.arguments, "importance": "normal"}

    async def test_note_schema_and_governance_match_at_the_model_boundary(self):
        for stream in (False, True):
            for effect in ("allow", "deny", "pending"):
                with self.subTest(stream=stream, effect=effect):
                    operation_id = f"schema-{stream}-{effect}"
                    store = NoteStore(Path(self.directory.name) / f"{operation_id}.db")
                    decision = {"effect": effect}
                    if effect == "pending":
                        decision["action_id"] = f"approval-{operation_id}"
                    governance = ApplicationGovernanceClient(decision)
                    model = SchemaRecordingClient(self.arguments)
                    agent = build_notes_agent(model, governance, store, operation_id)

                    async def execute(*, agent=agent, stream=stream):
                        if stream:
                            async for _ in agent.run("create the note", stream=True):
                                pass
                        else:
                            await agent.run("create the note")

                    if effect == "allow":
                        await asyncio.wait_for(execute(), timeout=3)
                    else:
                        with self.assertRaises(GovernanceBlockedError) as raised:
                            await asyncio.wait_for(execute(), timeout=3)
                        self.assertEqual(raised.exception.decision["effect"], effect)

                    self.assertEqual(len(model.tool_schemas), 2 if effect == "allow" else 1)
                    self.assertEqual(len(model.tool_schemas[0]), 1)
                    definition = model.tool_schemas[0][0]
                    self.assertEqual(definition["name"], "create_note")
                    self.assertIsInstance(definition["description"], str)
                    self.assertIn("note", definition["description"].casefold())
                    parameters = definition["parameters"]
                    self.assertEqual(parameters["type"], "object")
                    self.assertEqual(set(parameters["properties"]), {"title", "body", "importance"})
                    for name in ("title", "body", "importance"):
                        self.assertEqual(parameters["properties"][name]["type"], "string")
                    self.assertEqual(set(parameters["required"]), {"title", "body"})
                    self.assertEqual(parameters["properties"]["importance"]["default"], "normal")
                    for request in model.tool_schemas:
                        self.assertEqual(request, [definition], "Tool definitions changed between model calls")

                    self.assertEqual(len(governance.calls), 1)
                    call = governance.calls[0]
                    self.assertEqual(call["args"], self.expected)
                    self.assertEqual(call["agent_id"], "notes-app")
                    self.assertEqual(call["connector"], "workspace")
                    self.assertEqual(call["tool"], "createNote")
                    self.assertEqual(call["run_id"], operation_id)
                    self.assertEqual(governance.actions, [], "The middleware must not poll")

                    if effect == "allow":
                        self.assertEqual(store.notes(), {operation_id: self.expected})
                        self.assertEqual(store.get(operation_id)["status"], "done")
                    else:
                        self.assertEqual(store.notes(), {})
                    if effect == "pending":
                        saved = store.get(operation_id)
                        self.assertEqual(saved["status"], "pending")
                        self.assertEqual(saved["action_id"], decision["action_id"])
                        self.assertEqual(saved["args"], self.expected)

    async def pending(self):
        governance = ApplicationGovernanceClient({"effect": "pending", "action_id": "approval-one"})
        agent = build_notes_agent(
            ScriptedClient(self.arguments, name="create_note"), governance, self.store, "app-operation-one"
        )
        with self.assertRaises(GovernanceBlockedError):
            await agent.run("create the note")
        self.assertEqual(governance.actions, [], "The adapter must not poll")
        self.assertEqual(self.store.notes(), {})
        saved = self.store.get("app-operation-one")
        self.assertEqual(saved["args"], self.expected)
        self.assertEqual(saved["action_id"], "approval-one")
        return governance

    async def test_allow_reuses_application_transaction_for_idempotency(self):
        governance = ApplicationGovernanceClient({"effect": "allow"})
        for _ in range(2):
            agent = build_notes_agent(
                ScriptedClient(self.arguments, name="create_note"), governance, self.store, "app-operation-one"
            )
            await agent.run("create the note")
        self.assertEqual(len(governance.calls), 2, "The adapter itself does not deduplicate")
        self.assertEqual(self.store.notes(), {"app-operation-one": self.expected})

    async def test_pending_continuation_uses_saved_arguments_across_restart_and_concurrency(self):
        governance = await self.pending()
        self.assertEqual((await continue_note(self.store, governance, "app-operation-one"))["status"], "pending")
        governance.status = "approved"
        restarted = NoteStore(self.path)
        results = await asyncio.gather(
            continue_note(restarted, governance, "app-operation-one"),
            continue_note(restarted, governance, "app-operation-one"),
        )
        self.assertTrue(all(result["status"] == "done" for result in results))
        self.assertEqual(restarted.notes(), {"app-operation-one": self.expected})
        calls = len(governance.actions)
        self.assertEqual((await continue_note(restarted, governance, "app-operation-one"))["status"], "done")
        self.assertEqual(len(governance.actions), calls)

    async def test_rejected_and_consumed_approvals_never_create_a_note(self):
        for status in ("rejected", "consumed"):
            with self.subTest(status=status):
                self.store = NoteStore(Path(self.directory.name) / f"{status}.db")
                governance = await self.pending()
                governance.status = status
                result = await continue_note(self.store, governance, "app-operation-one")
                self.assertEqual(result["status"], "denied")
                self.assertEqual(self.store.notes(), {})

    async def test_stored_approval_cannot_be_replaced_or_reused_for_fresh_arguments(self):
        await self.pending()
        with self.assertRaises(ValueError):
            self.store.execute_allowed("app-operation-one", {**self.expected, "body": "replacement"})
        with self.assertRaises(ValueError):
            self.store.settle("app-operation-one", "another-action", "approved")
        self.assertEqual(self.store.notes(), {})
        self.assertEqual(self.store.get("app-operation-one")["args"], self.expected)

    async def test_poll_response_for_another_action_does_not_authorize_execution(self):
        governance = await self.pending()
        governance.action = lambda _action: {"action_id": "another-action", "status": "approved"}
        with self.assertRaises(ValueError):
            await continue_note(self.store, governance, "app-operation-one")
        self.assertEqual(self.store.notes(), {})
        self.assertEqual(self.store.get("app-operation-one")["status"], "pending")


if __name__ == "__main__":
    unittest.main()
