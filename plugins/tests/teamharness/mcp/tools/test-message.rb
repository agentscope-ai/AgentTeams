#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "tmpdir"

repo_root = Pathname.new(__dir__).join("../../../../..").expand_path
mcp_dir = repo_root / "plugins/teamharness/mcp"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

Dir.mktmpdir("teamharness-message-") do |_dir|
  python_test = <<~PY
    import http.server
    import json
    import os
    import pathlib
    import re
    import socketserver
    import sys
    import threading

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    from server import call_tool, list_tools

    captured = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_PUT(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length).decode("utf-8")
            captured["path"] = self.path
            captured["auth"] = self.headers.get("Authorization")
            captured["body"] = json.loads(body)
            payload = json.dumps({"event_id": "$event1"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length).decode("utf-8")
            captured["qwenpaw_path"] = self.path
            captured["qwenpaw_agent"] = self.headers.get("X-Agent-Id")
            captured["qwenpaw_body"] = json.loads(body)
            payload = json.dumps({"success": True, "message": "sent"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *_args):
            return

    server = socketserver.TCPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()

    def payload(args):
        result = call_tool("message", args)
        return json.loads(result["content"][0]["text"])

    dry = payload({
        "action": "send",
        "channel": "matrix",
        "target": "room:!room:example.test",
        "message": "@worker:example.test please verify `filesync`",
        "dryRun": True,
    })
    if not dry.get("ok"):
        raise AssertionError(f"dry-run failed: {dry!r}")
    if dry.get("mentions") != ["@worker:example.test"]:
        raise AssertionError(f"mentions mismatch: {dry!r}")
    if dry["content"].get("m.mentions") != {"user_ids": ["@worker:example.test"]}:
        raise AssertionError(f"m.mentions mismatch: {dry!r}")
    if "https://matrix.to/#/%40worker%3Aexample.test" not in dry["content"].get("formatted_body", ""):
        raise AssertionError(f"formatted mention missing: {dry!r}")
    if "<code>filesync</code>" not in dry["content"].get("formatted_body", ""):
        raise AssertionError(f"inline code missing: {dry!r}")

    import builtins
    real_import = builtins.__import__

    def block_markdown_import(name, *args, **kwargs):
        if name == "markdown_it" or name.startswith("markdown_it.") or name == "linkify_it":
            raise ImportError(name)
        return real_import(name, *args, **kwargs)

    builtins.__import__ = block_markdown_import
    try:
        fallback = payload({
            "action": "send",
            "channel": "matrix",
            "target": "room:!room:example.test",
            "message": "@worker:example.test **Task** assigned\\nPlease read `spec.md`.\\n```\\nline 1\\nline 2\\n```",
            "dryRun": True,
        })
        fallback_report = payload({
            "action": "send",
            "channel": "matrix",
            "target": "room:!room:example.test",
            "message": "## Project Status Report\\n\\n**Project**: TeamHarness\\n\\n| Task | Status |\\n|---|---|\\n| test | Done |\\n\\n- `shared/result.md`",
            "dryRun": True,
        })
    finally:
        builtins.__import__ = real_import
    fallback_html = fallback["content"].get("formatted_body", "")
    if "https://matrix.to/#/%40worker%3Aexample.test" not in fallback_html:
        raise AssertionError(f"fallback mention missing: {fallback!r}")
    if "<strong>Task</strong>" not in fallback_html:
        raise AssertionError(f"fallback bold missing: {fallback!r}")
    if "<code>spec.md</code>" not in fallback_html:
        raise AssertionError(f"fallback inline code missing: {fallback!r}")
    if "<br>" not in fallback_html:
        raise AssertionError(f"fallback line break missing: {fallback!r}")
    if "<pre><code>line 1\\nline 2</code></pre>" not in fallback_html:
        raise AssertionError(f"fallback code block missing: {fallback!r}")
    fallback_report_html = fallback_report["content"].get("formatted_body", "")
    if "<h2>Project Status Report</h2>" not in fallback_report_html:
        raise AssertionError(f"fallback heading missing: {fallback_report!r}")
    if "<table>" not in fallback_report_html:
        raise AssertionError(f"fallback table missing: {fallback_report!r}")
    if "<ul><li><code>shared/result.md</code></li></ul>" not in fallback_report_html:
        raise AssertionError(f"fallback list missing: {fallback_report!r}")

    markdown_report = payload({
        "action": "send",
        "channel": "matrix",
        "target": "room:!room:example.test",
        "message": "---\\n\\n## Project Status Report\\n\\n**Project**: TeamHarness\\n\\n| Task | Status |\\n|---|---|\\n| test | Done |\\n\\n- `shared/result.md`\\n1. Start next task",
        "dryRun": True,
    })
    formatted_report = markdown_report["content"].get("formatted_body", "")
    if "<hr" not in formatted_report:
        raise AssertionError(f"markdown horizontal rule was not rendered: {markdown_report!r}")
    if "<h2>Project Status Report</h2>" not in formatted_report:
        raise AssertionError(f"markdown heading was not rendered: {markdown_report!r}")
    if "<strong>Project</strong>" not in formatted_report:
        raise AssertionError(f"markdown bold was not rendered: {markdown_report!r}")
    if "<table>" not in formatted_report:
        raise AssertionError(f"markdown table was not rendered: {markdown_report!r}")
    if "<code>shared/result.md</code>" not in formatted_report:
        raise AssertionError(f"markdown inline code was not rendered: {markdown_report!r}")
    if "<ol>" not in formatted_report:
        raise AssertionError(f"markdown ordered list was not rendered: {markdown_report!r}")
    if "</h2><br>" in formatted_report or "</table><br>" in formatted_report or "</pre><br>" in formatted_report:
        raise AssertionError(f"markdown renderer inserted block-level br tags: {markdown_report!r}")

    text_alias = payload({
        "action": "send",
        "channel": "matrix",
        "target": "room:!room:example.test",
        "text": "@worker:example.test please verify the task",
        "dryRun": True,
    })
    if not text_alias.get("ok"):
        raise AssertionError(f"text alias dry-run failed: {text_alias!r}")
    if text_alias["content"].get("body") != "@worker:example.test please verify the task":
        raise AssertionError(f"text alias body mismatch: {text_alias!r}")

    room_id_alias = payload({
        "action": "send",
        "channel": "matrix",
        "room_id": "room:!room:example.test",
        "text": "@worker:example.test please verify room alias",
        "dryRun": True,
    })
    if not room_id_alias.get("ok"):
        raise AssertionError(f"room_id alias dry-run failed: {room_id_alias!r}")
    if room_id_alias.get("target") != "room:!room:example.test":
        raise AssertionError(f"room_id alias target mismatch: {room_id_alias!r}")

    blocked = payload({
        "action": "send",
        "channel": "matrix",
        "target": "room:!room:example.test",
        "message": "@worker:example.test ok",
        "dryRun": True,
    })
    if blocked.get("ok") is not False or "ping-pong" not in blocked.get("error", ""):
        raise AssertionError(f"low-information mention was not blocked: {blocked!r}")

    user_target = payload({
        "action": "send",
        "channel": "matrix",
        "target": "user:@worker:example.test",
        "message": "@worker:example.test please verify",
        "dryRun": True,
    })
    if user_target.get("ok") is not False or "user targets are not supported yet" not in user_target.get("error", ""):
        raise AssertionError(f"user target was not rejected: {user_target!r}")

    dingtalk_dry = payload({
        "action": "send",
        "replyRoute": {
            "channel": "dingtalk",
            "targetUser": "sender_001",
            "targetSession": "aaaaaaaa",
        },
        "text": "Project A is ready.",
        "dryRun": True,
    })
    if not dingtalk_dry.get("ok"):
        raise AssertionError(f"dingtalk dry-run failed: {dingtalk_dry!r}")
    if dingtalk_dry.get("channel") != "dingtalk":
        raise AssertionError(f"dingtalk dry-run channel mismatch: {dingtalk_dry!r}")
    if dingtalk_dry.get("targetUser") != "sender_001" or dingtalk_dry.get("targetSession") != "aaaaaaaa":
        raise AssertionError(f"dingtalk dry-run target mismatch: {dingtalk_dry!r}")

    import os
    os.environ["HICLAW_MATRIX_URL"] = f"http://127.0.0.1:{server.server_address[1]}"
    os.environ["HICLAW_WORKER_MATRIX_TOKEN"] = "test-token"
    os.environ["HICLAW_MATRIX_USER_ID"] = "@sender:example.test"
    os.environ["QWENPAW_API_BASE"] = f"http://127.0.0.1:{server.server_address[1]}"
    home_dir = pathlib.Path("#{_dir}") / "home"
    qwenpaw_dir = home_dir / ".qwenpaw"
    (qwenpaw_dir / "workspaces" / "default").mkdir(parents=True, exist_ok=True)
    os.environ.pop("QWENPAW_WORKING_DIR", None)
    os.environ.pop("COPAW_WORKING_DIR", None)
    os.environ["HOME"] = str(home_dir)

    sent = payload({
        "action": "send",
        "channel": "matrix",
        "room_id": "!room:example.test",
        "text": "Project is ready.",
    })
    if not sent.get("ok") or sent.get("messageId") != "$event1":
        raise AssertionError(f"send failed: {sent!r}")
    if sent.get("sessionRecorded") is not True:
        raise AssertionError(f"session record missing: {sent!r}")
    if captured.get("auth") != "Bearer test-token":
        raise AssertionError(f"auth mismatch: {captured!r}")
    if "/_matrix/client/v3/rooms/%21room%3Aexample.test/send/m.room.message/" not in captured.get("path", ""):
        raise AssertionError(f"send path mismatch: {captured!r}")
    if captured["body"].get("body") != "Project is ready.":
        raise AssertionError(f"body mismatch: {captured!r}")
    def session_safe(value):
        return re.sub(r'[\\\\/:*?"<>|]', "--", value)

    session_path = (
        qwenpaw_dir
        / "workspaces"
        / "default"
        / "sessions"
        / "matrix"
        / f"{session_safe('!room:example.test')}_{session_safe('matrix:!room:example.test')}.json"
    )
    if not session_path.exists():
        raise AssertionError(f"session file missing: {session_path}")
    session = json.loads(session_path.read_text(encoding="utf-8"))
    recorded_msg, marks = session["agent"]["memory"]["content"][-1]
    if marks != []:
        raise AssertionError(f"session marks mismatch: {session!r}")
    if recorded_msg.get("role") != "assistant":
        raise AssertionError(f"session role mismatch: {recorded_msg!r}")
    if recorded_msg.get("content", [{}])[0].get("text") != "Project is ready.":
        raise AssertionError(f"session text mismatch: {recorded_msg!r}")
    if recorded_msg.get("metadata") != {
        "channel": "matrix",
        "room_id": "!room:example.test",
        "message_id": "$event1",
        "source": "message_tool_outbound",
    }:
        raise AssertionError(f"session metadata mismatch: {recorded_msg!r}")

    dingtalk_sent = payload({
        "action": "send",
        "channel": "dingtalk",
        "targetUser": "sender_001",
        "targetSession": "aaaaaaaa",
        "text": "Project A is ready.",
        "agentId": "default",
    })
    server.shutdown()
    if not dingtalk_sent.get("ok"):
        raise AssertionError(f"dingtalk send failed: {dingtalk_sent!r}")
    if dingtalk_sent.get("sessionRecorded") is not True:
        raise AssertionError(f"dingtalk session record missing: {dingtalk_sent!r}")
    if captured.get("qwenpaw_path") != "/api/messages/send":
        raise AssertionError(f"qwenpaw send path mismatch: {captured!r}")
    if captured.get("qwenpaw_agent") != "default":
        raise AssertionError(f"qwenpaw agent mismatch: {captured!r}")
    expected_qwenpaw_body = {
        "channel": "dingtalk",
        "target_user": "sender_001",
        "target_session": "aaaaaaaa",
        "text": "Project A is ready.",
    }
    if captured.get("qwenpaw_body") != expected_qwenpaw_body:
        raise AssertionError(f"qwenpaw body mismatch: {captured!r}")
    dingtalk_session_path = (
        qwenpaw_dir
        / "workspaces"
        / "default"
        / "sessions"
        / "dingtalk"
        / "sender_001_aaaaaaaa.json"
    )
    if not dingtalk_session_path.exists():
        raise AssertionError(f"dingtalk session file missing: {dingtalk_session_path}")
    dingtalk_session = json.loads(dingtalk_session_path.read_text(encoding="utf-8"))
    dingtalk_msg, dingtalk_marks = dingtalk_session["agent"]["memory"]["content"][-1]
    if dingtalk_marks != []:
        raise AssertionError(f"dingtalk session marks mismatch: {dingtalk_session!r}")
    if dingtalk_msg.get("content", [{}])[0].get("text") != "Project A is ready.":
        raise AssertionError(f"dingtalk session text mismatch: {dingtalk_msg!r}")
    if dingtalk_msg.get("metadata") != {
        "channel": "dingtalk",
        "message_id": "",
        "source": "message_tool_outbound",
        "user_id": "sender_001",
        "session_id": "aaaaaaaa",
    }:
        raise AssertionError(f"dingtalk session metadata mismatch: {dingtalk_msg!r}")

    os.environ["HICLAW_AGENT_ROLE"] = "worker"
    worker_tools = [tool["name"] for tool in list_tools()]
    if "message" in worker_tools:
        raise AssertionError(f"worker should not see message tool: {worker_tools!r}")
    blocked = payload({
        "action": "send",
        "channel": "matrix",
        "target": "room:!room:example.test",
        "message": "should be blocked",
        "role": "leader",
        "dryRun": True,
    })
    if blocked.get("ok") is not False or blocked.get("error") != "forbidden_tool":
        raise AssertionError(f"worker message call should be blocked: {blocked!r}")

    print(json.dumps({
        "ok": True,
        "dryRunMentions": dry["mentions"],
        "messageId": sent["messageId"],
        "sendPath": captured["path"],
        "qwenpawSendPath": captured["qwenpaw_path"],
    }, ensure_ascii=False))
  PY

  stdout, stderr, status = Open3.capture3("python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness message MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  puts JSON.pretty_generate(JSON.parse(stdout))
end
