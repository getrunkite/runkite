"""Minimal CrewAI example -- proves the crewai_adapter runner works end
to end. Uses a hand-written BaseLLM subclass returning a fixed response
(deterministic, no API key/network needed) so this example runs offline,
same convention as examples/vector_agent's fake embeddings and
examples/langchain_agent's FakeListChatModel.
"""

from crewai import Agent, Crew, Task
from crewai.llms.base_llm import BaseLLM


class FakeLLM(BaseLLM):
    """A BaseLLM implementation that returns a fixed response instead of
    calling a real provider -- CrewAI's own docs describe BaseLLM as
    exactly for this: "Users can extend this class to create custom LLM
    implementations that don't rely on litellm's authentication
    mechanism." """

    response_text: str = "Hello from a CrewAI crew -- one agent, one task, no real LLM API call."

    def __init__(self, **data):
        data.setdefault("model", "fake-model")
        super().__init__(**data)

    def call(self, messages, tools=None, callbacks=None, available_functions=None, from_task=None, from_agent=None, response_model=None):
        return self.response_text


_agent = Agent(
    role="Greeter",
    goal="Greet the user",
    backstory="A friendly assistant that always has a kind word.",
    llm=FakeLLM(),
)
_task = Task(
    description="Respond to: {input}",
    expected_output="A short greeting.",
    agent=_agent,
)

# The runner (crewai_adapter.CrewAIAdapter) loads this exact object via
# "./crew.py:crew" in langgraph.json and calls
# crew.akickoff(inputs={"input": <last human message text>}).
crew = Crew(agents=[_agent], tasks=[_task])
