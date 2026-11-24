"""Wire validation only; native adapters retain authorization and waiter ownership."""
import re
import json
import os
import subprocess
import sys
from pathlib import Path

ANSWER_KEY = "com.agentteams.answer"
REPLY_FORMAT = ANSWER_KEY


def attention_answer(content):
    answer = content.get(ANSWER_KEY)
    return isinstance(answer, dict) and isinstance(answer.get("confirmation_id"), str) and answer["confirmation_id"].startswith("attention:")


def consume_attention(room, event_id, source):
    """Persist a fallback decision before its input is dispatched to a new turn."""
    content = source.get("content") or {}
    if not attention_answer(content):
        return None
    plugin = Path(os.getenv("AGENTTEAMS_PLUGIN_DIR", "/opt/agentteams/plugins/teamharness"))
    environment = dict(os.environ)
    if not environment.get("TEAMHARNESS_RUNTIME_CONFIG") and environment.get("AGENTTEAMS_WORKER_HOME"):
        environment["TEAMHARNESS_RUNTIME_CONFIG"] = str(Path(environment["AGENTTEAMS_WORKER_HOME"]) / "runtime/runtime.yaml")
    result = subprocess.run([sys.executable, str(plugin / "mcp/hitl_answer.py")],
        input=json.dumps({"room_id": room, "event_id": event_id, "source": source}),
        text=True, capture_output=True, check=True, env=environment)
    return json.loads(result.stdout)["accepted"]


def approval_decision(content, agent_id):
    """Return None for ordinary messages, () for invalid structured controls.

    Never fall through to body parsing when a structured decision is present.
    The native handler must still check room, sender, expiry and pending context.
    """
    answer = content.get(ANSWER_KEY)
    if not isinstance(answer, dict) or answer.get("kind") != "tool_approval":
        return None
    request = answer.get("confirmation_id")
    context = answer.get("context_id")
    decision = answer.get("answer")
    if (answer.get("agent_id") != agent_id or not isinstance(decision, str) or decision not in {"approved", "rejected"}
            or not isinstance(request, str) or not re.fullmatch(r"[\w.:-]+", request)
            or not isinstance(context, str) or not re.fullmatch(r"[\w.:-]+", context)):
        return ()
    return request, context, answer["answer"] == "approved"
