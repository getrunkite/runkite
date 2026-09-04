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


def test_find_new_tool_calls_empty_name_not_marked_seen():
    """Empty-name stream delta must not mark id seen — otherwise a later
    chunk that fills in a disallowed name is deduped and never denied."""
    seen = set()
    incomplete = type("Msg", (), {"tool_calls": [{"name": "", "args": {}, "id": "call_x", "type": "tool_call"}]})()
    first = find_new_tool_calls({"messages": [incomplete]}, seen)
    check("empty name yields nothing", first == [])
    check("id not marked seen while name empty", "call_x" not in seen)

    complete = type(
        "Msg",
        (),
        {"tool_calls": [{"name": "evil", "args": {}, "id": "call_x", "type": "tool_call"}]},
    )()
    second = find_new_tool_calls({"messages": [complete]}, seen)
    check("named chunk returned after empty", len(second) == 1 and second[0]["name"] == "evil")
    check("id marked seen only after name present", "call_x" in seen)


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


async def test_execute_run_denies_after_empty_name_chunk():
    """Two-chunk stream: id+empty name, then same id with forbidden name.
    Must deny — not false-allow via seen_ids marking on the empty chunk."""
    import operator

    class State(TypedDict):
        messages: Annotated[list, operator.add]
        step: int

    def agent(state):
        step = state.get("step", 0)
        if step == 0:
            return {
                "step": 1,
                "messages": [
                    AIMessage(
                        content="",
                        tool_calls=[
                            {"name": "", "args": {}, "id": "call_stream", "type": "tool_call"},
                        ],
                    )
                ],
            }
        return {
            "step": 2,
            "messages": [
                AIMessage(
                    content="",
                    tool_calls=[
                        {"name": "forbidden", "args": {}, "id": "call_stream", "type": "tool_call"},
                    ],
                )
            ],
        }

    def tools_node(state):
        raise AssertionError("ToolNode must not run when allowed_tools denies after empty-name chunk")

    g = StateGraph(State)
    g.add_node("agent", agent)
    g.add_node("tools", tools_node)
    g.set_entry_point("agent")

    def route(state):
        last = state["messages"][-1] if state.get("messages") else None
        if isinstance(last, AIMessage) and last.tool_calls:
            name = (
                last.tool_calls[0].get("name")
                if isinstance(last.tool_calls[0], dict)
                else getattr(last.tool_calls[0], "name", None)
            )
            if name:
                return "tools"
        if state.get("step", 0) < 2:
            return "agent"
        return END

    g.add_conditional_edges("agent", route, {"agent": "agent", "tools": "tools", END: END})
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
        "run_id": "run-empty-then-deny",
        "thread_id": "t1",
        "graph_id": "g1",
        "input": {"messages": [HumanMessage(content="hi")], "step": 0},
        "stream_modes": ["values"],
        "allowed_tools": ["search"],
    }
    status = await execute_run(FakeAdapter(), assignment, event_callback)
    check("status is error after empty→named deny", status == "error")
    methods = [e["method"] for e in events]
    check("emitted tool_auth deny after named chunk", "tool_auth" in methods)
    check("ToolNode never ran (no crash from tools_node)", "error" in methods)


async def test_execute_run_captures_usage_before_interrupt():
    """A HITL pause must not drop tokens already spent getting there.

    agent node appends an AIMessage carrying real usage_metadata, then a
    separate gate node unconditionally interrupts. The tokens from the
    agent step were already billed by the provider before the pause ever
    happens -- execute_run must surface them on a "values" event before
    "end": {"status": "interrupted"}, not silently drop them until (if
    ever) the run resumes to a terminal status.
    """
    from langgraph.checkpoint.memory import MemorySaver
    from langgraph.types import interrupt

    class _InterruptState(TypedDict):
        messages: Annotated[list, add_messages]

    def agent_with_usage(state):
        msg = AIMessage(
            content="thinking...",
            usage_metadata={"input_tokens": 120, "output_tokens": 40, "total_tokens": 160},
        )
        return {"messages": [msg]}

    def gate(state):
        interrupt({"question": "approve?"})
        return {"messages": [AIMessage(content="resumed")]}

    builder = StateGraph(_InterruptState)
    builder.add_node("agent", agent_with_usage)
    builder.add_node("gate", gate)
    builder.add_edge(START, "agent")
    builder.add_edge("agent", "gate")
    builder.add_edge("gate", END)
    compiled = builder.compile(checkpointer=MemorySaver())

    class _InterruptAdapter:
        def is_factory(self, _gid):
            return False

        def get_graph(self, _gid):
            return compiled

    events = []

    async def event_callback(event):
        events.append(event)

    assignment = {
        "run_id": "r-interrupt-usage",
        "thread_id": "t-interrupt-usage",
        "graph_id": "gate_agent",
        "input": {"messages": [{"role": "human", "content": "do the thing"}]},
        "stream_modes": ["values"],
    }
    status = await execute_run(_InterruptAdapter(), assignment, event_callback)

    check("run reports interrupted", status == "interrupted")
    methods = [e["method"] for e in events]
    check("end status is interrupted", events[-1]["data"].get("status") == "interrupted")

    values_with_usage = [e for e in events if e["method"] == "values" and e["data"].get("usage")]
    check("a values event carried usage before the interrupted end", len(values_with_usage) == 1)
    if values_with_usage:
        usage = values_with_usage[0]["data"]["usage"]
        check("prompt_tokens captured from the pre-interrupt AI turn", usage.get("prompt_tokens") == 120)
        check("completion_tokens captured from the pre-interrupt AI turn", usage.get("completion_tokens") == 40)
    # The usage-carrying values event must land before "end", not after.
    check(
        "usage event precedes end", methods.index("end") == len(methods) - 1 and "values" in methods[: len(methods) - 1]
    )


async def test_execute_run_captures_usage_before_interrupt_updates_mode_only():
    """Same proof as test_execute_run_captures_usage_before_interrupt, but
    with stream_modes=["updates"] instead of ["values"] -- exercises the
    incremental usage_totals fallback (no last_values snapshot ever exists
    in this mode) combined with "updates" mode's {node_name: {...}}
    envelope, which accumulate_usage previously could not see into at all.
    """
    from langgraph.checkpoint.memory import MemorySaver
    from langgraph.types import interrupt

    class _InterruptState(TypedDict):
        messages: Annotated[list, add_messages]

    def agent_with_usage(state):
        msg = AIMessage(
            content="thinking...",
            usage_metadata={"input_tokens": 77, "output_tokens": 33, "total_tokens": 110},
        )
        return {"messages": [msg]}

    def gate(state):
        interrupt({"question": "approve?"})
        return {"messages": [AIMessage(content="resumed")]}

    builder = StateGraph(_InterruptState)
    builder.add_node("agent", agent_with_usage)
    builder.add_node("gate", gate)
    builder.add_edge(START, "agent")
    builder.add_edge("agent", "gate")
    builder.add_edge("gate", END)
    compiled = builder.compile(checkpointer=MemorySaver())

    class _InterruptAdapter:
        def is_factory(self, _gid):
            return False

        def get_graph(self, _gid):
            return compiled

    events = []

    async def event_callback(event):
        events.append(event)

    assignment = {
        "run_id": "r-interrupt-usage-updates",
        "thread_id": "t-interrupt-usage-updates",
        "graph_id": "gate_agent",
        "input": {"messages": [{"role": "human", "content": "do the thing"}]},
        "stream_modes": ["updates"],
    }
    status = await execute_run(_InterruptAdapter(), assignment, event_callback)

    check("run reports interrupted", status == "interrupted")
    values_with_usage = [e for e in events if e["method"] == "values" and e["data"].get("usage")]
    check("usage captured via incremental updates-mode fallback", len(values_with_usage) == 1)
    if values_with_usage:
        usage = values_with_usage[0]["data"]["usage"]
        check("prompt_tokens captured (updates mode)", usage.get("prompt_tokens") == 77)
        check("completion_tokens captured (updates mode)", usage.get("completion_tokens") == 33)


async def test_execute_run_does_not_double_count_usage_across_turns():
    """A live, real-Gemini dogfood run on a multi-turn thread found this:
    turn 2's reported usage included turn 1's tokens again, turn 3
    included turns 1 and 2 again, and so on -- because Runkite injects its
    own checkpointer into every LangGraph.attach_checkpointer'd graph, and
    a stateful graph's "values" snapshot is the *entire* message history,
    not just this run's own turn. Every prior AIMessage.usage_metadata was
    still sitting in state and got summed again on every later run.

    Two separate execute_run calls on the same thread_id (exactly how a
    real multi-turn conversation works over the Agent Protocol -- one run
    per user message, not one long-lived run) must each report only their
    own turn's tokens.
    """
    from langgraph.checkpoint.memory import MemorySaver

    class _ChatState(TypedDict):
        messages: Annotated[list, add_messages]

    call_usage = [
        {"input_tokens": 50, "output_tokens": 20, "total_tokens": 70},
        {"input_tokens": 90, "output_tokens": 35, "total_tokens": 125},
    ]
    call_count = {"n": 0}

    def agent_with_usage(state):
        i = call_count["n"]
        call_count["n"] += 1
        msg = AIMessage(content=f"reply {i}", usage_metadata=call_usage[i])
        return {"messages": [msg]}

    builder = StateGraph(_ChatState)
    builder.add_node("agent", agent_with_usage)
    builder.add_edge(START, "agent")
    builder.add_edge("agent", END)
    compiled = builder.compile(checkpointer=MemorySaver())

    class _ChatAdapter:
        def is_factory(self, _gid):
            return False

        def get_graph(self, _gid):
            return compiled

    async def run_turn(run_id: str, content: str) -> dict | None:
        events = []

        async def event_callback(event):
            events.append(event)

        assignment = {
            "run_id": run_id,
            "thread_id": "t-multiturn-dedup",
            "graph_id": "chat",
            "input": {"messages": [{"role": "human", "content": content}]},
            "stream_modes": ["values"],
        }
        status = await execute_run(_ChatAdapter(), assignment, event_callback)
        check(f"{run_id} succeeds", status == "success")
        values_with_usage = [e for e in events if e["method"] == "values" and e["data"].get("usage")]
        return values_with_usage[-1]["data"]["usage"] if values_with_usage else None

    usage_turn1 = await run_turn("r-turn-1", "hello")
    usage_turn2 = await run_turn("r-turn-2", "again")

    check(
        "turn 1 reports only its own call's tokens",
        usage_turn1
        == {
            "prompt_tokens": 50,
            "completion_tokens": 20,
            "total_tokens": 70,
        },
    )
    check(
        "turn 2 reports only its own call's tokens, not turn 1's on top",
        usage_turn2 == {"prompt_tokens": 90, "completion_tokens": 35, "total_tokens": 125},
    )


async def test_execute_run_resume_does_not_recount_pre_interrupt_usage():
    """The interrupted run and its resume are two separate run_ids in two
    separate usage_events rows -- if the resumed run's final "values"
    snapshot still contains the pre-interrupt AIMessage (it does; nothing
    removes it from state) and accumulate_usage re-sums it, that same
    provider call gets billed twice in Spend for one real API call.
    """
    from langgraph.checkpoint.memory import MemorySaver
    from langgraph.types import interrupt

    class _InterruptState(TypedDict):
        messages: Annotated[list, add_messages]
        approved: bool

    def agent_with_usage(state):
        msg = AIMessage(
            content="draft",
            usage_metadata={"input_tokens": 30, "output_tokens": 15, "total_tokens": 45},
        )
        return {"messages": [msg]}

    def gate(state):
        approval = interrupt({"question": "approve?"})
        return {"approved": bool(approval)}

    builder = StateGraph(_InterruptState)
    builder.add_node("agent", agent_with_usage)
    builder.add_node("gate", gate)
    builder.add_edge(START, "agent")
    builder.add_edge("agent", "gate")
    builder.add_edge("gate", END)
    compiled = builder.compile(checkpointer=MemorySaver())

    class _InterruptAdapter:
        def is_factory(self, _gid):
            return False

        def get_graph(self, _gid):
            return compiled

    def usage_from(events: list) -> dict | None:
        values_with_usage = [e for e in events if e["method"] == "values" and e["data"].get("usage")]
        return values_with_usage[-1]["data"]["usage"] if values_with_usage else None

    events1 = []

    async def cb1(event):
        events1.append(event)

    assignment1 = {
        "run_id": "r-resume-dedup-1",
        "thread_id": "t-resume-dedup",
        "graph_id": "gate_agent",
        "input": {"messages": [{"role": "human", "content": "do it"}]},
        "stream_modes": ["values"],
    }
    status1 = await execute_run(_InterruptAdapter(), assignment1, cb1)
    check("first run interrupts", status1 == "interrupted")
    usage1 = usage_from(events1)
    check(
        "interrupted run reports the draft's tokens",
        usage1
        == {
            "prompt_tokens": 30,
            "completion_tokens": 15,
            "total_tokens": 45,
        },
    )

    events2 = []

    async def cb2(event):
        events2.append(event)

    assignment2 = {
        "run_id": "r-resume-dedup-2",
        "thread_id": "t-resume-dedup",
        "graph_id": "gate_agent",
        "input": None,
        "resume_command": {"response": True},
        "stream_modes": ["values"],
    }
    status2 = await execute_run(_InterruptAdapter(), assignment2, cb2)
    check("resume succeeds", status2 == "success")
    usage2 = usage_from(events2)
    check("resume reports no NEW usage -- the draft's tokens were already billed", usage2 is None)


def main():
    test_find_new_tool_calls_extracts_from_ai_message()
    test_find_new_tool_calls_dedups_by_id()
    test_find_new_tool_calls_no_tool_calls_returns_empty()
    test_find_new_tool_calls_handles_list_nesting()
    test_find_new_tool_calls_empty_name_not_marked_seen()
    asyncio.run(test_execute_run_emits_tool_call_event())
    asyncio.run(test_execute_run_does_not_duplicate_tool_call_across_stream_modes())
    asyncio.run(test_execute_run_denies_disallowed_tool())
    asyncio.run(test_execute_run_denies_after_empty_name_chunk())
    asyncio.run(test_execute_run_captures_usage_before_interrupt())
    asyncio.run(test_execute_run_captures_usage_before_interrupt_updates_mode_only())
    asyncio.run(test_execute_run_does_not_double_count_usage_across_turns())
    asyncio.run(test_execute_run_resume_does_not_recount_pre_interrupt_usage())
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
