"""Execute real native Agent loops with a deterministic local model fixture.

No synthetic spans are substituted for model calls: OpenAI instrumentation
records the native SDK request. The provider response and token counts are
fixtures, explicitly marked on the trace. Set DEMO_LLM_BASE_URL, API_KEY and
MODEL to use a real OpenAI-compatible model instead.
"""

import asyncio
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import threading
import time

from at_telemetry import flush
from at_runtime_adapters import bind

RUNTIME = os.environ["AGENTTEAMS_OTEL_RUNTIME"]
PROMPT = "Reply with a short sentence describing why OpenTelemetry helps multi-runtime agents."


class ModelFixture(BaseHTTPRequestHandler):
    failed_once = False
    def log_message(self, *_):
        pass

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if os.getenv("DEMO_FAIL_FIRST_REQUEST") == "429" and not ModelFixture.failed_once:
            ModelFixture.failed_once = True
            self.send_response(429)
            self.send_header("Content-Type", "application/json")
            self.send_header("Retry-After", "0")
            self.end_headers()
            self.wfile.write(json.dumps({"error": {"message": "isolated retry acceptance", "type": "rate_limit_error"}}).encode())
            return
        time.sleep(0.15)
        content = "OpenTelemetry gives QwenPaw and Hermes a shared trace format for AgentTeams."
        message = {"role": "assistant", "content": content}
        payload = {"id": "chatcmpl-at-demo", "object": "chat.completion", "created": int(time.time()),
                   "model": request.get("model", "at-demo-fixture"),
                   "choices": [{"index": 0, "message": message, "finish_reason": "stop"}],
                   "usage": {"prompt_tokens": 48, "completion_tokens": 18, "total_tokens": 66}}
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if request.get("stream") else "application/json")
        self.end_headers()
        if request.get("stream"):
            chunk = {"id": payload["id"], "object": "chat.completion.chunk",
                     "created": payload["created"], "model": payload["model"]}
            for choices, usage in (([{"index": 0, "delta": {"role": "assistant", "content": content}, "finish_reason": None}], None),
                                   ([{"index": 0, "delta": {}, "finish_reason": "stop"}], None),
                                   ([], payload["usage"])):
                event = {**chunk, "choices": choices}
                if usage:
                    event["usage"] = usage
                self.wfile.write(f"data: {json.dumps(event)}\n\n".encode())
            self.wfile.write(b"data: [DONE]\n\n")
        else:
            self.wfile.write(json.dumps(payload).encode())
        self.wfile.flush()


async def qwenpaw(base_url, api_key, model):
    from agentscope.credential import OpenAICredential
    from agentscope.message import Msg, TextBlock
    from agentscope.model import OpenAIChatModel
    from agentscope.tool import Toolkit
    from qwenpaw.agents.react_agent import QwenPawAgent, ReActConfig
    from qwenpaw.config.config import AgentProfileConfig

    agent = QwenPawAgent(
        name="qwenpaw-tracing-demo",
        model=OpenAIChatModel(credential=OpenAICredential(api_key=api_key, base_url=base_url),
                              model=model, stream=False, max_retries=1),
        system_prompt="You are a concise assistant.", toolkit=Toolkit(),
        react_config=ReActConfig(max_iters=2), middlewares=[],
        agent_config=AgentProfileConfig(id="demo", name="tracing-demo"), workspace_dir=Path("/tmp/at-tracing-workspace"),
    )
    result = await agent.reply(Msg(name="user", content=[TextBlock(text=PROMPT)], role="user"))
    if result.error:
        raise RuntimeError(f"QwenPaw turn failed: {result.error}")
    return "\n".join(block.text for block in result.content if isinstance(block, TextBlock))


def hermes(base_url, api_key, model):
    from run_agent import AIAgent

    agent = AIAgent(base_url=base_url, api_key=api_key, model=model, provider="custom",
                    api_mode="chat_completions", max_iterations=2, enabled_toolsets=[],
                    skip_context_files=True, skip_memory=True, skip_background_review=True,
                    quiet_mode=True, save_trajectories=False)
    result = agent.run_conversation(PROMPT, system_message="You are a concise assistant.")
    if result.get("failed") or not result.get("completed"):
        raise RuntimeError(f"Hermes turn failed: {result.get('error', 'incomplete')}")
    return str(result.get("final_response", ""))


def main():
    base = os.getenv("DEMO_LLM_BASE_URL")
    fixture = not bool(base)
    server = None
    if fixture:
        server = ThreadingHTTPServer(("127.0.0.1", 0), ModelFixture)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        base = f"http://127.0.0.1:{server.server_port}/v1"
    api_key = os.getenv("DEMO_LLM_API_KEY", "demo-fixture")
    model = os.getenv("DEMO_LLM_MODEL", "at-demo-fixture")
    task = os.getenv("DEMO_TASK_ID", f"demo-{RUNTIME}-{int(time.time())}")
    try:
        with bind({"agentteams.task.id": task, "agentteams.room.id": "!tracing-demo:agentteams.local",
                   "agentteams.project.id": "observability-demo", "agentteams.demo.fixture": fixture,
                   "model_source": "fixture" if fixture else "live"}):
            result = asyncio.run(qwenpaw(base, api_key, model)) if RUNTIME == "qwenpaw" else hermes(base, api_key, model)
            if "OpenTelemetry" not in result and fixture:
                raise RuntimeError(f"Native runtime did not return the fixture response: {result[:500]}")
            print(json.dumps({"runtime": RUNTIME, "task": task,
                              "model_source": "fixture" if fixture else "live", "result": result}, ensure_ascii=False))
    finally:
        if not flush():
            raise RuntimeError("OTel flush timed out")
        if server:
            server.shutdown()


if __name__ == "__main__":
    main()
