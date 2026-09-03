"""Self-check for the on_tool_call producer: the hook's infrastructure
existed in the Go control plane -- watchRunEventsForToolCallHook
in internal/api/runs.go -- but neither runner emitted that method until
find_new_tool_calls was added.

Proves:
1. find_new_tool_calls extracts tool_calls from AIMessage-shaped objects
   nested anywhere in a stream chunk (dict/list wrapping).
2. Dedup by tool_call id: the same call seen twice (e.g. once in
   "values" mode, once in "updates" mode, if both are requested) is only
   returned once.
3. execute_run, run against a real LangGraph ReAct-style graph (agent
   node emits a tool call, tool node executes it, agent node responds),
   actually emits a "tool_call" method RunEvent with name/args/id -- not
   just that the helper function works in isolation.

Usage:
    python/.venv/bin/python python/tests/test_tool_call_hook.py
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from typing import Annotated, TypedDict  # noqa: E402

from langchain_core.messages import AIMessage, HumanMessage, ToolMessage  # noqa: E402
from langchain_core.tools import tool  # noqa: E402
from langgraph.graph import END, START, StateGraph  # noqa: E402
from langgraph.graph.message import add_messages  # noqa: E402
from langgraph.prebuilt import ToolNode  # noqa: E402
from runkite_runner.worker import execute_run, find_new_tool_calls  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


# --- find_new_tool_calls unit tests ---


def test_find_new_tool_calls_extracts_from_ai_message():
    seen = set()
    msg = AIMessage(
        content="", tool_calls=[{"name": "search", "args": {"q": "x"}, "id": "call_1", "type": "tool_call"}]
    )
    found = find_new_tool_calls({"messages": [msg]}, seen)
    check("finds the one tool call", len(found) == 1)
    check("name extracted", found[0]["name"] == "search")
    check("id tracked in seen set", "call_1" in seen)


def test_find_new_tool_calls_dedups_by_id():
    seen = set()
    msg = AIMessage(content="", tool_calls=[{"name": "search", "args": {}, "id": "call_1", "type": "tool_call"}])
    first = find_new_tool_calls({"agent": {"messages": [msg]}}, seen)
    second = find_new_tool_calls({"agent": {"messages": [msg]}}, seen)
    check("first scan finds it", len(first) == 1)
    check("second scan (same id) finds nothing new", len(second) == 0)


def test_find_new_tool_calls_no_tool_calls_returns_empty():
    seen = set()
    found = find_new_tool_calls({"messages": [AIMessage(content="hello")]}, seen)
    check("plain AIMessage with no tool_calls yields nothing", found == [])


def test_find_new_tool_calls_handles_list_nesting():
    seen = set()
    msg = AIMessage(content="", tool_calls=[{"name": "search", "args": {}, "id": "call_2", "type": "tool_call"}])
    found = find_new_tool_calls([{"messages": [msg]}, "ignored-string"], seen)
    check("finds tool call nested inside a list of dicts", len(found) == 1)


# --- execute_run integration test: real LangGraph ReAct-style graph ---


@tool
def search(query: str) -> str:
    """Search for information."""
    return f"Result for '{query}'"


class _FakeReActModel:
    def __init__(self):
        self._called = False

    def invoke(self, messages, **kwargs):
        has_tool_result = any(isinstance(m, ToolMessage) for m in messages)
        if not has_tool_result:
            return AIMessage(
                content="",
                tool_calls=[{"name": "search", "args": {"query": "hi"}, "id": "call_001", "type": "tool_call"}],
            )
        return AIMessage(content="final answer")


class _State(TypedDict):
    messages: Annotated[list, add_messages]


def _build_react_graph():
    model = _FakeReActModel()
    tool_node = ToolNode([search])

    def agent_node(state):
        return {"messages": [model.invoke(state["messages"])]}

    def should_continue(state):
        last = state["messages"][-1]
        return "tools" if isinstance(last, AIMessage) and last.tool_calls else END

    builder = StateGraph(_State)
    builder.add_node("agent", agent_node)
    builder.add_node("tools", tool_node)
    builder.add_edge(START, "agent")
    builder.add_conditional_edges("agent", should_continue, {"tools": "tools", END: END})
    builder.add_edge("tools", "agent")
    return builder.compile()


class _FakeAdapter:
    def is_factory(self, graph_id):
        return False

    def get_graph(self, graph_id):
        return _build_react_graph()


async def test_execute_run_emits_tool_call_event():
    events = []

    async def event_callback(event):
        events.append(event)

    assignment = {
        "run_id": "r1",
        "thread_id": "t1",
        "graph_id": "react_agent",
        "input": {"messages": [{"role": "human", "content": "search for something"}]},
        "stream_modes": ["updates"],
    }
    status = await execute_run(_FakeAdapter(), assignment, event_callback)

    check("run completed successfully", status == "success")
    tool_call_events = [e for e in events if e["method"] == "tool_call"]
    check("exactly one tool_call event emitted", len(tool_call_events) == 1)
    if tool_call_events:
        data = tool_call_events[0]["data"]
        check("tool_call event has name=search", data["name"] == "search")
        check("tool_call event has id=call_001", data["id"] == "call_001")
        check("tool_call event has args", data["args"] == {"query": "hi"})


async def test_execute_run_does_not_duplicate_tool_call_across_stream_modes():
    """values+updates together both surface the same AIMessage -- the
    dedup-by-id must collapse them to one event, not two."""
    events = []

    async def event_callback(event):
        events.append(event)

    assignment = {
        "run_id": "r2",
        "thread_id": "t2",
        "graph_id": "react_agent",
        "input": {"messages": [{"role": "human", "content": "search again"}]},
        "stream_modes": ["values", "updates"],
    }
    await execute_run(_FakeAdapter(), assignment, event_callback)

    tool_call_events = [e for e in events if e["method"] == "tool_call"]
    check("still exactly one tool_call event with values+updates both requested", len(tool_call_events) == 1)


async def test_execute_run_denies_disallowed_tool():
    """allowed_tools present → non-listed tool_call fails the run before ToolNode."""
    import operator

    class State(TypedDict):
        messages: Annotated[list, operator.add]

    def agent(state):
        return {
            "messages": [
                AIMessage(
                    content="",
                    tool_calls=[
                        {
                            "name": "forbidden",
                            "args": {},
                            "id": "call_bad",
                            "type": "tool_call",
                        }
                    ],
                )
            ]
        }

    def tools_node(state):
        raise AssertionError("ToolNode must not run when allowed_tools denies")

    g = StateGraph(State)
    g.add_node("agent", agent)
    g.add_node("tools", tools_node)
    g.set_entry_point("agent")
    g.add_edge("agent", "tools")
    g.add_edge("tools", END)
    compiled = g.compile()

    class FakeAdapter:
        def is_factory(self, _gid):
            return False

        def get_graph(self, _gid):
            return compiled

    events = []

    async def event_callback(event):
        events.append(event)

    assignment = {
        "run_id": "run-deny",
        "thread_id": "t1",
        "graph_id": "g1",
        "input": {"messages": [HumanMessage(content="hi")]},
        "stream_modes": ["values"],
        "allowed_tools": ["search"],
    }
    status = await execute_run(FakeAdapter(), assignment, event_callback)
    check("status is error", status == "error")
    methods = [e["method"] for e in events]
    check("emitted tool_call before deny", "tool_call" in methods)
    check("emitted tool_auth deny", "tool_auth" in methods)
    check("emitted error", "error" in methods)
    check("emitted end", methods[-1] == "end")
    auth = next(e for e in events if e["method"] == "tool_auth")
    check("reason_code tool_not_allowed", auth["data"].get("reason_code") == "tool_not_allowed")
    check("end status error", events[-1]["data"].get("status") == "error")


def main():
    test_find_new_tool_calls_extracts_from_ai_message()
    test_find_new_tool_calls_dedups_by_id()
    test_find_new_tool_calls_no_tool_calls_returns_empty()
    test_find_new_tool_calls_handles_list_nesting()
    asyncio.run(test_execute_run_emits_tool_call_event())
    asyncio.run(test_execute_run_does_not_duplicate_tool_call_across_stream_modes())
    asyncio.run(test_execute_run_denies_disallowed_tool())
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
