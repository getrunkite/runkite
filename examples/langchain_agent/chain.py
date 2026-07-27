"""Minimal plain-LangChain example -- proves the langchain_adapter runner
works end to end without LangGraph, CrewAI, or LlamaIndex.

Uses FakeListChatModel (deterministic, no API key/network needed) so this
example runs offline in CI, same convention as examples/vector_agent's
fake embeddings.
"""

from langchain_core.language_models.fake_chat_models import FakeListChatModel
from langchain_core.output_parsers import StrOutputParser
from langchain_core.prompts import ChatPromptTemplate

_prompt = ChatPromptTemplate.from_messages([
    ("system", "You are a terse, helpful assistant."),
    ("human", "{input}"),
])
_llm = FakeListChatModel(responses=[
    "Hello from a plain LangChain chain -- no LangGraph, no StateGraph, just a Runnable pipe.",
])

# The runner (langchain_adapter.LangChainAdapter) loads this exact
# object via "./chain.py:chain" in langgraph.json and calls
# chain.ainvoke({"input": <last human message text>}).
chain = _prompt | _llm | StrOutputParser()
