"""Send a repeatable task to the isolated standard Worker via real Matrix."""

import argparse
import base64
import json
import time
from urllib.parse import quote

from prepare_matrix_workers import STATE, matrix


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("runtime", choices=["qwenpaw", "hermes"])
    parser.add_argument("turn", choices=["1", "2", "handoff"])
    parser.add_argument("--run", default="acceptance", help="Use the same fresh run name for all turns")
    args = parser.parse_args()
    state = json.loads(STATE.read_text())
    user = state["accounts"][f"trace-{args.runtime}"]["user_id"]
    path = f"/root/agentteams-fs/agents/trace-{args.runtime}/tracing-{args.run}.txt"
    if args.turn == "1":
        text = (f"{user} This is an isolated real-tool tracing test. First read {path} with your file tool; "
                f"it should not exist. After observing the error, write exactly MATRIX_TRACE_OK to {path} using a tool, "
                "then read it back with a tool. Reply briefly with the actual result.")
    elif args.turn == "2":
        text = (f"{user} Continue our previous task in this same conversation. Read {path} using a tool, "
                "append a second line MATRIX_SECOND_TURN_OK with a tool, read it back and confirm both lines briefly.")
    else:
        if args.runtime != "qwenpaw":
            raise SystemExit("This acceptance uses QwenPaw's native proactive-send API as sender")
        other = "hermes" if args.runtime == "qwenpaw" else "qwenpaw"
        other_id = state["accounts"][f"trace-{other}"]["user_id"]
        peer = base64.b64encode(other_id.encode()).decode()
        script = ("import base64, httpx\n"
                  f"peer = base64.b64decode({peer!r}).decode()\n"
                  f"text = peer + ' Use your read_file tool directly to read /root/agentteams-fs/agents/trace-{other}/tracing-{args.run}.txt and report its contents. Do not delegate to a subagent.'\n"
                  f"payload = {{'channel': 'agentteams_matrix', 'target_user': peer, 'target_session': 'matrix:{state['room_id']}', 'text': text}}\n"
                  "with httpx.Client(timeout=45) as client:\n"
                  "    response = client.post('http://127.0.0.1:8088/api/messages/send', headers={'X-Agent-Id': 'default'}, json=payload)\n"
                  "    response.raise_for_status()\n"
                  "    print(response.json())\n")
        text = (f"{user} Execute this isolated cross-worker handoff using your real tools. "
                "Write the following Python program to /tmp/at-handoff.py and execute it with your shell tool using the Python interpreter from your runtime venv. "
                "It calls your existing local proactive-message API. Do not just print the code. "
                "After success, your final reply must be only HANDOFF_SENT. Do not repeat the message in your final reply.\n```python\n" + script + "```")
    result = matrix("PUT", f"/rooms/{quote(state['room_id'], safe='')}/send/m.room.message/trace-{time.time_ns()}",
        {"msgtype": "m.text", "body": text, "m.mentions": {"user_ids": [user]}},
        state["accounts"]["trace-human"]["access_token"])
    print(json.dumps({"runtime": args.runtime, "turn": args.turn, "event_id": result["event_id"], "room_id": state["room_id"]}))


if __name__ == "__main__":
    main()
