"""Minimal LlamaIndex example -- proves the llamaindex_adapter runner
works end to end. Uses a hand-written CustomLLM subclass returning a
fixed response (deterministic, no API key/network needed) so this
example runs offline, same convention as examples/vector_agent's fake
embeddings and examples/langchain_agent's FakeListChatModel.

Uses SimpleChatEngine (LlamaIndex's own lightweight conversational
construct) rather than FunctionAgent -- the latter requires a
FunctionCallingLLM (native tool-calling support), which a fake/offline
LLM would need to emulate for no real benefit in a minimal example.
"""

from llama_index.core.base.llms.types import ChatMessage, ChatResponse, CompletionResponse, LLMMetadata, MessageRole
from llama_index.core.chat_engine import SimpleChatEngine
from llama_index.core.llms import CustomLLM
from llama_index.core.llms.callbacks import llm_chat_callback, llm_completion_callback


class FakeLLM(CustomLLM):
    """A CustomLLM implementation that returns a fixed response instead
    of calling a real provider -- LlamaIndex's own docs describe
    CustomLLM as exactly for this: implement complete()/chat() (and
    their async/streaming variants) against any backend, including none
    at all."""

    response_text: str = "Hello from a LlamaIndex SimpleChatEngine -- no real LLM API call."

    @property
    def metadata(self) -> LLMMetadata:
        return LLMMetadata(context_window=4096, num_output=256, is_chat_model=True)

    @llm_completion_callback()
    def complete(self, prompt: str, formatted: bool = False, **kwargs) -> CompletionResponse:
        return CompletionResponse(text=self.response_text)

    @llm_completion_callback()
    async def acomplete(self, prompt: str, formatted: bool = False, **kwargs) -> CompletionResponse:
        return CompletionResponse(text=self.response_text)

    @llm_completion_callback()
    def stream_complete(self, prompt: str, formatted: bool = False, **kwargs):
        raise NotImplementedError("streaming not needed for this example")

    async def astream_complete(self, prompt: str, formatted: bool = False, **kwargs):
        raise NotImplementedError("streaming not needed for this example")

    @llm_chat_callback()
    def chat(self, messages: list[ChatMessage], **kwargs) -> ChatResponse:
        return ChatResponse(message=ChatMessage(role=MessageRole.ASSISTANT, content=self.response_text))

    @llm_chat_callback()
    async def achat(self, messages: list[ChatMessage], **kwargs) -> ChatResponse:
        return ChatResponse(message=ChatMessage(role=MessageRole.ASSISTANT, content=self.response_text))

    @llm_chat_callback()
    def stream_chat(self, messages: list[ChatMessage], **kwargs):
        raise NotImplementedError("streaming not needed for this example")

    async def astream_chat(self, messages: list[ChatMessage], **kwargs):
        raise NotImplementedError("streaming not needed for this example")


# The runner (llamaindex_adapter.LlamaIndexAdapter) loads this exact
# object via "./chat_engine.py:chat_engine" in langgraph.json and calls
# chat_engine.achat(<last human message text>).
chat_engine = SimpleChatEngine.from_defaults(llm=FakeLLM())
