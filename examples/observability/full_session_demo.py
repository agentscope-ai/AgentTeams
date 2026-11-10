"""Live-model, two-turn native runtime acceptance. No manually emitted turn spans."""

import asyncio
import json
import os
from pathlib import Path

from at_telemetry import flush

RUNTIME = os.environ["AGENTTEAMS_OTEL_RUNTIME"]
WORK = Path("/tmp/at-full-session")
WORK.mkdir(exist_ok=True)
TARGET = WORK / "result.txt"
PROMPTS = [
    f"This is an isolated tracing acceptance task. First use a tool to read {TARGET}; it does not exist yet. "
    f"After observing the error, use a tool to write the exact text TEAM_TRACE_OK to {TARGET}, "
    "then read it back using a tool and report the result. Execute real tools, do not simulate them.",
    f"Continue the same conversation. Read {TARGET} using a tool, append a second line SECOND_TURN_OK using a tool, "
    "then read it back and confirm both lines. Keep your answer short.",
]


async def qwenpaw():
    from agentscope.credential import OpenAICredential
    from agentscope.message import Msg, TextBlock
    from agentscope.model import OpenAIChatModel
    from agentscope.tool import Toolkit, FunctionTool
    from agentscope.tool import ToolChunk
    from agentscope.message import ToolResultState
    from qwenpaw.agents.react_agent import QwenPawAgent, ReActConfig
    from qwenpaw.config.config import AgentProfileConfig

    def read_file(path: str):
        """Read a UTF-8 file at an absolute path. Args: path: Absolute path."""
        try:
            return ToolChunk(content=[TextBlock(text=Path(path).read_text())], state=ToolResultState.SUCCESS)
        except OSError as exc:
            return ToolChunk(content=[TextBlock(text=str(exc))], state=ToolResultState.ERROR)

    def write_file(path: str, content: str):
        """Write a UTF-8 file. Args: path: Absolute path. content: Complete new file content."""
        Path(path).write_text(content)
        return ToolChunk(content=[TextBlock(text="written")], state=ToolResultState.SUCCESS)

    agent = QwenPawAgent(
        name="qwenpaw-full-session", system_prompt="You are a precise assistant. Execute the requested tools.",
        model=OpenAIChatModel(credential=OpenAICredential(api_key=os.environ["DEMO_LLM_API_KEY"], base_url=os.environ["DEMO_LLM_BASE_URL"]),
                              model=os.environ["DEMO_LLM_MODEL"], stream=True, max_retries=1),
        toolkit=Toolkit(tools=[FunctionTool(read_file), FunctionTool(write_file)]),
        react_config=ReActConfig(max_iters=12), middlewares=[],
        agent_config=AgentProfileConfig(id="default", name="full-session"), workspace_dir=WORK,
        request_context={"session_id": "full-session-qwenpaw", "user_id": "acceptance",
                         "channel_meta": {"room_id": "!full-session:at-demo", "task_id": "full-session-acceptance", "team_id": "tracing-team", "project_id": "tracing-acceptance"}},
    )
    for index, prompt in enumerate(PROMPTS):
        result = await agent.reply(Msg(name="acceptance", role="user", content=[TextBlock(text=prompt)]))
        if result.error:
            raise RuntimeError(str(result.error))
        print(json.dumps({"runtime": RUNTIME, "turn": index + 1, "result": str(result.content)}, ensure_ascii=False), flush=True)


def hermes():
    from run_agent import AIAgent
    agent = AIAgent(base_url=os.environ["DEMO_LLM_BASE_URL"], api_key=os.environ["DEMO_LLM_API_KEY"],
                    model=os.environ["DEMO_LLM_MODEL"], provider="custom", api_mode="chat_completions",
                    max_iterations=12, enabled_toolsets=["terminal", "file"], session_id="full-session-hermes",
                    skip_context_files=True, skip_memory=True, skip_background_review=True,
                    quiet_mode=True, save_trajectories=False)
    history = None
    for index, prompt in enumerate(PROMPTS):
        result = agent.run_conversation(prompt, conversation_history=history, task_id="full-session-acceptance")
        if result.get("failed") or not result.get("completed"):
            raise RuntimeError(str(result.get("error", "incomplete")))
        history = result.get("messages")
        print(json.dumps({"runtime": RUNTIME, "turn": index + 1, "result": result.get("final_response")}, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    try:
        asyncio.run(qwenpaw()) if RUNTIME == "qwenpaw" else hermes()
        assert "TEAM_TRACE_OK" in TARGET.read_text() and "SECOND_TURN_OK" in TARGET.read_text()
        print(json.dumps({"runtime": RUNTIME, "verified_file": TARGET.read_text()}))
    finally:
        flush()
