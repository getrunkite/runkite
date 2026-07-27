"""ReAct agent with tool calls for VG-001 spike validation.

Uses a fake LLM (no API key needed) that deterministically:
1. Receives user message
2. Decides to call the 'search' tool
3. Gets tool result
4. Generates final response

This tests the full ReAct loop: agent node -> tool node -> agent node,
with real LangGraph StateGraph + ToolNode, producing values + updates events
with tool_calls in message content.
"""

from typing import Annotated, Any, TypedDict

from langchain_core.messages import AIMessage, HumanMessage, ToolMessage, BaseMessage
from langchain_core.tools import tool
from langgraph.graph import StateGraph, START, END
from langgraph.graph.message import add_messages
from langgraph.prebuilt import ToolNode


# --- Define a simple tool ---
@tool
def search(query: str) -> str:
    """Search for information."""
    # Deterministic fake result for testing
    return f"Result for '{query}': The answer is 42."


# --- Define a fake model that makes deterministic decisions ---
class FakeReActModel:
    """A fake chat model that:
    - On first call: emits a tool call to 'search'
    - On second call (after tool result): emits a final text response
    """

    def __init__(self, tools: list):
        self.tools = tools
        self._call_count = 0

    def bind_tools(self, tools):
        return self

    def invoke(self, messages: list, **kwargs) -> AIMessage:
        self._call_count += 1

        # Check if we already have a tool result
        has_tool_result = any(
            isinstance(m, ToolMessage) or (isinstance(m, dict) and m.get("role") == "tool")
            for m in messages
        )

        if not has_tool_result:
            # First call: decide to use the search tool
            last_msg = messages[-1]
            query = last_msg.content if isinstance(last_msg, BaseMessage) else last_msg.get("content", "")
            return AIMessage(
                content="",
                tool_calls=[{
                    "name": "search",
                    "args": {"query": query},
                    "id": "call_001",
                    "type": "tool_call",
                }],
            )
        else:
            # Second call: generate final response from tool results
            tool_result = None
            for m in messages:
                if isinstance(m, ToolMessage):
                    tool_result = m.content
                elif isinstance(m, dict) and m.get("role") == "tool":
                    tool_result = m.get("content", "")

            return AIMessage(content=f"Based on my research: {tool_result}")


# --- State ---
class State(TypedDict):
    messages: Annotated[list, add_messages]


# --- Nodes ---
tools = [search]
model = FakeReActModel(tools)
tool_node = ToolNode(tools)


def agent_node(state: State) -> dict:
    """Call the model to decide next action."""
    result = model.invoke(state["messages"])
    return {"messages": [result]}


def should_continue(state: State) -> str:
    """Route to tools if the last message has tool calls, else end."""
    last_message = state["messages"][-1]
    if isinstance(last_message, AIMessage) and last_message.tool_calls:
        return "tools"
    return END


# --- Build the graph ---
builder = StateGraph(State)
builder.add_node("agent", agent_node)
builder.add_node("tools", tool_node)

builder.add_edge(START, "agent")
builder.add_conditional_edges("agent", should_continue, {"tools": "tools", END: END})
builder.add_edge("tools", "agent")

graph = builder.compile()
