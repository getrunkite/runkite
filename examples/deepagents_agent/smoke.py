"""Exercise real upstream graph loading, streaming and checkpoint attachment."""

import asyncio
from pathlib import Path

from langgraph.checkpoint.memory import InMemorySaver
from runkite_runner.worker import LangGraphAdapter


async def main():
    registry = LangGraphAdapter()
    await registry.load_config(str(Path(__file__).with_name("langgraph.json")))
    graph = registry.get_graph("deep_agent")
    graph.checkpointer = InMemorySaver()
    config = {"configurable": {"thread_id": "deepagents-smoke", "run_id": "offline-smoke"}}
    chunks = [
        value
        async for value in graph.astream(
            {"messages": [{"role": "user", "content": "hello"}]}, config, stream_mode="values"
        )
    ]
    assert "existing LangGraph runner" in chunks[-1]["messages"][-1].content
    snapshot = await graph.aget_state(config)
    assert snapshot.values["messages"][-1].content == chunks[-1]["messages"][-1].content
    next_config = {"configurable": {"thread_id": "deepagents-smoke", "run_id": "offline-smoke-next"}}
    result = await graph.ainvoke({"messages": [{"role": "user", "content": "next turn"}]}, next_config)
    assert len(result["messages"]) >= 4
    print("Deep Agents compiled-graph loading, streaming and checkpoint continuity passed")


if __name__ == "__main__":
    asyncio.run(main())
