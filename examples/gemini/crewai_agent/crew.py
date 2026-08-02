"""CrewAI crew powered by Gemini via LiteLLM (real LLM)."""

from __future__ import annotations

import os
import sys
from pathlib import Path

from crewai import Agent, Crew, LLM, Task

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402

# CrewAI/LiteLLM expect GOOGLE_API_KEY (or GEMINI_API_KEY) in the environment.
os.environ["GOOGLE_API_KEY"] = require_google_api_key()

_llm = LLM(
    model=f"gemini/{gemini_model()}",
    temperature=gemini_temperature(),
)

_agent = Agent(
    role="Greeter",
    goal="Greet the user briefly",
    backstory="A friendly assistant that answers in one short sentence.",
    llm=_llm,
)
_task = Task(
    description="Respond to: {input}",
    expected_output="A short greeting or answer in one sentence.",
    agent=_agent,
)

crew = Crew(agents=[_agent], tasks=[_task])
