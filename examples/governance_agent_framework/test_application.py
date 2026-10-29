"""App continuation tests with the real native framework and no provider calls."""

from __future__ import annotations

import asyncio
import tempfile
import unittest
from pathlib import Path

from application import NoteStore, build_notes_agent, continue_note
from runkite_governance.agent_framework import GovernanceBlockedError
from test_integration import DecisionClient, ScriptedClient


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
