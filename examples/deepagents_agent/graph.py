"""An upstream Deep Agents graph, served by the existing LangGraph runner.

The default model is deterministic and makes no provider or tool calls.
Set DEEPAGENTS_MODEL to a provider:model identifier for a real model.
"""

import os

from deepagents import create_deep_agent
from langchain_core.language_models.fake_chat_models import FakeMessagesListChatModel
from langchain_core.messages import AIMessage


class OfflineModel(FakeMessagesListChatModel):
    def bind_tools(self, tools, **kwargs):
        return self


model = os.environ.get("DEEPAGENTS_MODEL") or OfflineModel(
    responses=[
        AIMessage(content="Deep Agents is running through Runkite's existing LangGraph runner."),
        AIMessage(content="Deep Agents is running through Runkite's existing LangGraph runner."),
    ]
)
graph = create_deep_agent(model=model, tools=[])
