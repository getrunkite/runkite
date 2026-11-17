"""Public, provider-free static graph for the native schema recovery fixture."""

import asyncio
import os
from pathlib import Path
from typing import TypedDict

from langgraph.graph import END, START, StateGraph


class RecoveryState(TypedDict):
    message: str
    busy: bool


async def hold_owned_slot(state: RecoveryState) -> dict:
    directory = Path(os.environ["RUNKITE_SCHEMA_NATIVE_DIR"])
    # A second execution is a failed qualification, not another accepted run.
    with (directory / "started").open("x") as marker:
        marker.write("started")
    deadline = asyncio.get_running_loop().time() + 90
    while not (directory / "release").exists():
        if asyncio.get_running_loop().time() >= deadline:
            raise RuntimeError("native schema fixture release deadline exceeded")
        await asyncio.sleep(0.05)
    return {"message": "native schema recovery completed", "busy": False}


builder = StateGraph(RecoveryState)
builder.add_node("hold_owned_slot", hold_owned_slot)
builder.add_edge(START, "hold_owned_slot")
builder.add_edge("hold_owned_slot", END)
graph = builder.compile()
