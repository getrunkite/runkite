"""An app-owned note transaction and approval continuation, independent of the model loop."""

from __future__ import annotations

import asyncio
import json
import sqlite3
from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from pathlib import Path
from typing import Any

from agent_framework import Agent, FunctionTool
from runkite_governance import GovernanceClient
from runkite_governance.agent_framework import (
    GovernanceFunctionMiddleware,
    GovernedFunction,
    PendingInvocation,
)


def _plain(value: Any) -> Any:
    if isinstance(value, Mapping):
        return {key: _plain(item) for key, item in value.items()}
    if isinstance(value, tuple):
        return [_plain(item) for item in value]
    return value


class NoteStore:
    """Local example storage. Operation and note changes share a SQLite transaction."""

    def __init__(self, path: str | Path) -> None:
        self.path = str(path)
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        with self._connect() as db:
            db.execute(
                "CREATE TABLE IF NOT EXISTS operations ("
                "id TEXT PRIMARY KEY, args TEXT NOT NULL, action_id TEXT, "
                "status TEXT NOT NULL CHECK(status IN ('pending','denied','done')))"
            )
            db.execute("CREATE TABLE IF NOT EXISTS notes (id TEXT PRIMARY KEY, args TEXT NOT NULL)")

    @contextmanager
    def _connect(self) -> Iterator[sqlite3.Connection]:
        db = sqlite3.connect(self.path, timeout=10)
        db.row_factory = sqlite3.Row
        try:
            with db:
                yield db
        finally:
            db.close()

    @staticmethod
    def _encode(arguments: Mapping[str, Any]) -> str:
        return json.dumps(_plain(arguments), sort_keys=True, separators=(",", ":"), allow_nan=False)

    @staticmethod
    def _row(row: sqlite3.Row) -> dict[str, Any]:
        result = dict(row)
        result["args"] = json.loads(result["args"])
        if result["status"] == "done":
            result["result"] = {"note_id": result["id"]}
        return result

    def get(self, operation_id: str) -> dict[str, Any]:
        with self._connect() as db:
            row = db.execute("SELECT * FROM operations WHERE id = ?", (operation_id,)).fetchone()
            if row is None:
                raise KeyError("Unknown application operation")
            return self._row(row)

    def save_pending(self, invocation: PendingInvocation) -> dict[str, Any]:
        arguments = self._encode(invocation.arguments)
        with self._connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT * FROM operations WHERE id = ?", (invocation.run_id,)).fetchone()
            if row is not None:
                if row["args"] != arguments:
                    raise ValueError("Operation ID already belongs to different arguments")
                if row["status"] == "pending" and row["action_id"] != invocation.action_id:
                    raise ValueError("Operation already has a different stored approval")
                return self._row(row)
            db.execute(
                "INSERT INTO operations VALUES (?,?,?,'pending')",
                (invocation.run_id, arguments, invocation.action_id),
            )
        return self.get(invocation.run_id)

    def execute_allowed(self, operation_id: str, arguments: Mapping[str, Any]) -> dict[str, Any]:
        encoded = self._encode(arguments)
        with self._connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT * FROM operations WHERE id = ?", (operation_id,)).fetchone()
            if row is not None:
                if row["args"] != encoded:
                    raise ValueError("Operation ID already belongs to different arguments")
                if row["status"] != "done":
                    raise ValueError("Continue the stored approval instead of executing a fresh operation")
                return self._row(row)
            db.execute("INSERT INTO notes VALUES (?,?)", (operation_id, encoded))
            db.execute("INSERT INTO operations VALUES (?,?,NULL,'done')", (operation_id, encoded))
        return self.get(operation_id)

    def settle(self, operation_id: str, action_id: str, status: str) -> dict[str, Any]:
        if status not in {"pending", "approved", "rejected", "consumed"}:
            raise ValueError("Unknown approval status")
        with self._connect() as db:
            db.execute("BEGIN IMMEDIATE")
            row = db.execute("SELECT * FROM operations WHERE id = ?", (operation_id,)).fetchone()
            if row is None:
                raise KeyError("Unknown application operation")
            if row["status"] != "pending":
                return self._row(row)
            if row["action_id"] != action_id:
                raise ValueError("Approval does not belong to the stored operation")
            if status == "approved":
                db.execute("INSERT INTO notes VALUES (?,?)", (operation_id, row["args"]))
                db.execute("UPDATE operations SET status='done' WHERE id = ?", (operation_id,))
            elif status in {"rejected", "consumed"}:
                db.execute("UPDATE operations SET status='denied' WHERE id = ?", (operation_id,))
        return self.get(operation_id)

    def notes(self) -> dict[str, Any]:
        with self._connect() as db:
            return {row["id"]: json.loads(row["args"]) for row in db.execute("SELECT * FROM notes")}


def build_notes_agent(client: Any, governance: GovernanceClient, store: NoteStore, operation_id: str) -> Agent:
    """Build one app-request-scoped agent. The app supplies the model client and stable operation ID."""

    async def create_note(title: str, body: str, importance: str = "normal") -> str:
        result = await asyncio.to_thread(
            store.execute_allowed, operation_id, {"title": title, "body": body, "importance": importance}
        )
        return json.dumps(result)

    function = FunctionTool(name="create_note", func=create_note)

    async def save_pending(invocation: PendingInvocation) -> None:
        await asyncio.to_thread(store.save_pending, invocation)

    middleware = GovernanceFunctionMiddleware(
        governance,
        agent_id="notes-app",
        functions=[GovernedFunction(function=function, connector="workspace", tool="createNote")],
        operation_id=lambda _context: operation_id,
        on_pending=save_pending,
    )
    return Agent(client=client, tools=[function], middleware=[middleware])


async def continue_note(store: NoteStore, governance: GovernanceClient, operation_id: str) -> dict[str, Any]:
    """Continue only a saved operation. There is no fresh-arguments or client-approved flag."""
    operation = await asyncio.to_thread(store.get, operation_id)
    if operation["status"] != "pending":
        return operation
    action = await asyncio.to_thread(governance.action, operation["action_id"])
    if action.get("action_id") != operation["action_id"]:
        raise ValueError("Approval response does not match the stored action")
    return await asyncio.to_thread(store.settle, operation_id, operation["action_id"], action["status"])
