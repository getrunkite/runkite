"""A native, credential-free Google ADK tool loop with managed history."""

from google.adk.agents import LlmAgent
from google.adk.models import BaseLlm, LlmResponse
from google.adk.tools.tool_context import ToolContext
from google.genai import types
from google_adk_adapter import GoogleAdkRunContext


def agent(context: GoogleAdkRunContext) -> LlmAgent:
    """Create a fresh agent and model for this trusted managed assignment."""

    def remember(text: str, tool_context: ToolContext) -> dict[str, str]:
        """Remember the latest text in this thread's native session state."""
        tool_context.state["latest_text"] = text
        return {"remembered": text}

    class OfflineModel(BaseLlm):
        async def generate_content_async(self, llm_request, stream=False):
            latest = llm_request.contents[-1]
            if any(part.function_response for part in latest.parts or []):
                content = types.Content(role="model", parts=[types.Part(text="Remembered in this managed thread.")])
            else:
                text = "".join(part.text or "" for part in latest.parts or [] if not part.thought)
                content = types.Content(
                    role="model",
                    parts=[
                        types.Part(
                            function_call=types.FunctionCall(
                                id=f"remember-{context.run_id}",
                                name="remember",
                                args={"text": text},
                            )
                        )
                    ],
                )
            yield LlmResponse(content=content, partial=False, turn_complete=True)

    return LlmAgent(
        name="managed_example",
        model=OfflineModel(model="offline-google-adk-example"),
        instruction="Remember the user's text once, then reply with a short confirmation.",
        tools=[remember],
    )
