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
            captured["put_count"] = captured.get("put_count", 0) + 1
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
            if self.path == "/api/messages/send":
                captured["qwenpaw_count"] = captured.get("qwenpaw_count", 0) + 1
                captured["qwenpaw_path"] = self.path
                captured["qwenpaw_agent"] = self.headers.get("X-Agent-Id")
                captured["qwenpaw_body"] = json.loads(body)
                payload = json.dumps({"success": True, "message": "sent"}).encode("utf-8")
            else:
                captured["dingtalk_webhook_count"] = captured.get("dingtalk_webhook_count", 0) + 1
                captured["dingtalk_webhook_path"] = self.path
                captured["dingtalk_webhook_body"] = json.loads(body)
                payload = json.dumps({"errcode": 0, "errmsg": "ok"}).encode("utf-8")
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

    def call(args):
        return call_tool("message", args)

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

    matrix_route_dry = payload({
        "action": "send",
        "replyRoute": {
            "channel": "matrix",
            "targetUser": "@admin:example.test",
            "targetSession": "!dm-room:example.test",
        },
        "text": "Project A is ready.",
        "dryRun": True,
    })
    if not matrix_route_dry.get("ok"):
        raise AssertionError(f"matrix replyRoute dry-run failed: {matrix_route_dry!r}")
    if matrix_route_dry.get("target") != "room:!dm-room:example.test":
        raise AssertionError(f"matrix replyRoute target mismatch: {matrix_route_dry!r}")
    if matrix_route_dry.get("targetKind") != "room":
        raise AssertionError(f"matrix replyRoute target kind mismatch: {matrix_route_dry!r}")

    import os
    os.environ["AGENTTEAMS_MATRIX_URL"] = f"http://127.0.0.1:{server.server_address[1]}"
    os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = "test-token"
    os.environ["AGENTTEAMS_MATRIX_USER_ID"] = "@sender:example.test"
    os.environ["QWENPAW_API_BASE"] = f"http://127.0.0.1:{server.server_address[1]}"
    home_dir = pathlib.Path("#{_dir}") / "home"
    qwenpaw_dir = home_dir / ".qwenpaw"
    (qwenpaw_dir / "workspaces" / "default").mkdir(parents=True, exist_ok=True)
    context_file = pathlib.Path("#{_dir}") / "teamharness-matrix-context.json"
    os.environ["TEAMHARNESS_MATRIX_CONTEXT_FILE"] = str(context_file)
    os.environ.pop("QWENPAW_WORKING_DIR", None)
    os.environ.pop("COPAW_WORKING_DIR", None)
    os.environ["HOME"] = str(home_dir)
    def session_safe(value):
        return re.sub(r'[\\\\/:*?"<>|]', "--", value)

    put_count_before_trigger = captured.get("put_count", 0)
    trigger_intent = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "@leader:example.test",
            "session": {
                "channel": "matrix",
                "id": "!leader-dm:example.test",
            },
        },
        "target": "room:!task-room:example.test",
        "message": {
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: req-123\\nReplyRoute: matrix/@admin:example.test/!leader-dm:example.test\\nContext: shared/roomflow/project-requests/req-123/request.md",
        },
        "replyRoute": {
            "channel": "matrix",
            "targetUser": "@admin:example.test",
            "targetSession": "!leader-dm:example.test",
        },
        "agentId": "@leader:example.test",
    })
    if not trigger_intent.get("ok"):
        raise AssertionError(f"self trigger intent failed: {trigger_intent!r}")
    if captured.get("put_count", 0) != put_count_before_trigger + 1:
        raise AssertionError(f"self trigger should send one Matrix PUT: {captured!r}")
    if trigger_intent.get("delivery", {}).get("sent") != "matrix_self_trigger":
        raise AssertionError(f"self trigger delivery mismatch: {trigger_intent!r}")
    if trigger_intent.get("context", {}).get("via") != "matrix_current_event":
        raise AssertionError(f"self trigger context mismatch: {trigger_intent!r}")
    trigger = trigger_intent.get("trigger") or {}
    if trigger.get("status") != "sent" or trigger.get("type") != "PROJECT_REQUESTED":
        raise AssertionError(f"self trigger status mismatch: {trigger_intent!r}")
    if trigger.get("targetCurrentEvent") != "$event1":
        raise AssertionError(f"self trigger event mismatch: {trigger_intent!r}")
    if trigger.get("sourceSession") != "matrix:!leader-dm:example.test":
        raise AssertionError(f"self trigger source mismatch: {trigger_intent!r}")
    if trigger.get("targetSession") != "matrix:!task-room:example.test":
        raise AssertionError(f"self trigger target mismatch: {trigger_intent!r}")
    marker = captured["body"].get("m.teamharness.trigger") or {}
    if marker.get("kind") != "self_cross_session" or marker.get("type") != "PROJECT_REQUESTED":
        raise AssertionError(f"self trigger marker mismatch: {captured!r}")
    if marker.get("targetRoomId") != "!task-room:example.test":
        raise AssertionError(f"self trigger marker target mismatch: {captured!r}")
    if marker.get("replyRoute") != {
        "channel": "matrix",
        "targetUser": "@admin:example.test",
        "targetSession": "!leader-dm:example.test",
    }:
        raise AssertionError(f"self trigger replyRoute marker mismatch: {captured!r}")

    put_count_before_hidden_route = captured.get("put_count", 0)
    hidden_route_trigger = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "@leader:example.test",
            "session": {
                "channel": "matrix",
                "id": "!leader-dm:example.test",
            },
        },
        "target": "room:!task-room:example.test",
        "message": {
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: hidden route",
        },
        "replyRoute": {
            "channel": "matrix",
            "targetSession": "!leader-dm:example.test",
        },
        "agentId": "@leader:example.test",
    })
    if hidden_route_trigger.get("ok"):
        raise AssertionError(f"PROJECT_REQUESTED with hidden replyRoute should fail: {hidden_route_trigger!r}")
    if "targetSession" not in hidden_route_trigger.get("error", ""):
        raise AssertionError(f"PROJECT_REQUESTED hidden replyRoute error mismatch: {hidden_route_trigger!r}")
    if captured.get("put_count", 0) != put_count_before_hidden_route:
        raise AssertionError(f"PROJECT_REQUESTED hidden replyRoute must not send Matrix PUT: {captured!r}")
    trigger_session_path = (
        qwenpaw_dir
        / "workspaces"
        / "default"
        / "sessions"
        / "matrix"
        / f"{session_safe('!task-room:example.test')}_{session_safe('matrix:!task-room:example.test')}.json"
    )
    if trigger_session_path.exists():
        raise AssertionError(f"self trigger should not write session file: {trigger_session_path}")

    put_count_before_dingtalk_trigger = captured.get("put_count", 0)
    dingtalk_trigger = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "@leader:example.test",
            "session": {
                "channel": "dingtalk",
                "id": "ding-group-session-001",
            },
        },
        "target": "room:!task-room:example.test",
        "message": {
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: dingtalk req\\nRequester route: dingtalk/sender_001/ding-group-session-001",
        },
        "replyRoute": {
            "channel": "dingtalk",
            "targetUser": "sender_001",
            "targetSession": "ding-group-session-001",
            "mentionSender": True,
        },
        "agentId": "@leader:example.test",
    })
    if not dingtalk_trigger.get("ok"):
        raise AssertionError(f"dingtalk source trigger failed: {dingtalk_trigger!r}")
    if captured.get("put_count", 0) != put_count_before_dingtalk_trigger + 1:
        raise AssertionError(f"dingtalk source trigger should send one Matrix PUT: {captured!r}")
    dingtalk_trigger_intent = dingtalk_trigger.get("trigger") or {}
    if dingtalk_trigger_intent.get("status") != "sent":
        raise AssertionError(f"dingtalk source trigger status mismatch: {dingtalk_trigger!r}")
    if dingtalk_trigger_intent.get("sourceChannel") != "dingtalk":
        raise AssertionError(f"dingtalk source channel mismatch: {dingtalk_trigger!r}")
    if dingtalk_trigger_intent.get("sourceSession") != "dingtalk:ding-group-session-001":
        raise AssertionError(f"dingtalk source session mismatch: {dingtalk_trigger!r}")
    if dingtalk_trigger_intent.get("targetSession") != "matrix:!task-room:example.test":
        raise AssertionError(f"dingtalk target session mismatch: {dingtalk_trigger!r}")
    if dingtalk_trigger_intent.get("replyRoute") != {
        "channel": "dingtalk",
        "targetUser": "sender_001",
        "targetSession": "ding-group-session-001",
        "mentionSender": True,
    }:
        raise AssertionError(f"dingtalk reply route mismatch: {dingtalk_trigger!r}")

    put_count_before_hidden_target_user = captured.get("put_count", 0)
    hidden_target_user_trigger = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "@leader:example.test",
            "session": {
                "channel": "dingtalk",
                "id": "ding-group-session-001",
            },
        },
        "target": "room:!task-room:example.test",
        "message": {
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: hidden target user\\nRequester route: dingtalk/ding-group-session-001",
        },
        "replyRoute": {
            "channel": "dingtalk",
            "targetUser": "sender_001",
            "targetSession": "ding-group-session-001",
        },
        "agentId": "@leader:example.test",
    })
    if hidden_target_user_trigger.get("ok"):
        raise AssertionError(f"PROJECT_REQUESTED with hidden targetUser should fail: {hidden_target_user_trigger!r}")
    if "targetUser" not in hidden_target_user_trigger.get("error", ""):
        raise AssertionError(f"PROJECT_REQUESTED hidden targetUser error mismatch: {hidden_target_user_trigger!r}")
    if captured.get("put_count", 0) != put_count_before_hidden_target_user:
        raise AssertionError(f"PROJECT_REQUESTED hidden targetUser must not send Matrix PUT: {captured!r}")

    put_count_before_missing_route = captured.get("put_count", 0)
    missing_route_trigger = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "@leader:example.test",
            "session": {
                "channel": "dingtalk",
                "id": "ding-group-session-001",
            },
        },
        "target": "room:!task-room:example.test",
        "message": {
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: missing route",
        },
        "agentId": "@leader:example.test",
    })
    if missing_route_trigger.get("ok"):
        raise AssertionError(f"PROJECT_REQUESTED without replyRoute should fail: {missing_route_trigger!r}")
    if "replyRoute" not in missing_route_trigger.get("error", ""):
        raise AssertionError(f"PROJECT_REQUESTED missing replyRoute error mismatch: {missing_route_trigger!r}")
    if captured.get("put_count", 0) != put_count_before_missing_route:
        raise AssertionError(f"PROJECT_REQUESTED missing replyRoute must not send Matrix PUT: {captured!r}")

    put_count_before_role_agent = captured.get("put_count", 0)
    role_agent_trigger = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "default",
            "session": {
                "channel": "dingtalk",
                "id": "ding-group-session-001",
            },
        },
        "target": "room:!task-room:example.test",
        "message": {
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: role agent\\nRequester route: dingtalk/ding-group-session-001",
        },
        "replyRoute": {
            "channel": "dingtalk",
            "targetUser": "sender_001",
            "targetSession": "ding-group-session-001",
        },
        "agentId": "default",
    })
    if role_agent_trigger.get("ok"):
        raise AssertionError(f"PROJECT_REQUESTED with role/workspace agent id should fail: {role_agent_trigger!r}")
    if "Matrix user id" not in role_agent_trigger.get("error", ""):
        raise AssertionError(f"PROJECT_REQUESTED role/workspace agent error mismatch: {role_agent_trigger!r}")
    if captured.get("put_count", 0) != put_count_before_role_agent:
        raise AssertionError(f"PROJECT_REQUESTED role/workspace agent must not send Matrix PUT: {captured!r}")

    put_count_before_string_trigger = captured.get("put_count", 0)
    string_trigger = payload({
        "action": "send",
        "channel": "matrix",
        "sender": {
            "agent": "@leader:example.test",
            "session": {
                "channel": "matrix",
                "id": "!leader-dm:example.test",
            },
        },
        "target": "room:!task-room:example.test",
        "message": json.dumps({
            "type": "PROJECT_REQUESTED",
            "text": "PROJECT_REQUESTED: string req\\nRequester route: matrix/!leader-dm:example.test",
        }),
        "replyRoute": {
            "channel": "matrix",
            "targetSession": "!leader-dm:example.test",
        },
        "agentId": "@leader:example.test",
    })
    if not string_trigger.get("ok"):
        raise AssertionError(f"string self trigger failed: {string_trigger!r}")
    if captured.get("put_count", 0) != put_count_before_string_trigger + 1:
        raise AssertionError(f"string self trigger should send one Matrix PUT: {captured!r}")
    if string_trigger.get("delivery", {}).get("sent") != "matrix_self_trigger":
        raise AssertionError(f"string self trigger delivery mismatch: {string_trigger!r}")
    if captured["body"].get("body") != "PROJECT_REQUESTED: string req\\nRequester route: matrix/!leader-dm:example.test":
        raise AssertionError(f"string self trigger body mismatch: {captured!r}")
    string_marker = captured["body"].get("m.teamharness.trigger") or {}
    if string_marker.get("kind") != "self_cross_session" or string_marker.get("type") != "PROJECT_REQUESTED":
        raise AssertionError(f"string self trigger marker mismatch: {captured!r}")

    sent_result = call({
        "action": "send",
        "channel": "matrix",
        "room_id": "!room:example.test",
        "text": "Project is ready.",
    })
    sent = json.loads(sent_result["content"][0]["text"])
    if not sent.get("ok") or sent.get("messageId") != "$event1":
        raise AssertionError(f"send failed: {sent!r}")
    if sent_result.get("isError"):
        raise AssertionError(f"did not expect isError on success: {sent_result!r}")
    if sent.get("sessionRecorded") is not True:
        raise AssertionError(f"session record missing: {sent!r}")
    if captured.get("auth") != "Bearer test-token":
        raise AssertionError(f"auth mismatch: {captured!r}")
    if "/_matrix/client/v3/rooms/%21room%3Aexample.test/send/m.room.message/" not in captured.get("path", ""):
        raise AssertionError(f"send path mismatch: {captured!r}")
    if captured["body"].get("body") != "Project is ready.":
        raise AssertionError(f"body mismatch: {captured!r}")
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
    context = json.loads(context_file.read_text(encoding="utf-8"))
    room_context = context.get("rooms", {}).get("!room:example.test") or {}
    if room_context.get("attachmentParentEventId") != "$event1":
        raise AssertionError(f"message send should update attachment context: {context!r}")

    dingtalk_sent = payload({
        "action": "send",
        "channel": "dingtalk",
        "targetUser": "sender_001",
        "targetSession": "aaaaaaaa",
        "text": "Project A is ready.",
        "agentId": "default",
    })
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

    qwenpaw_body_before_missing = captured.get("qwenpaw_body")
    dingtalk_mention_missing = payload({
        "action": "send",
        "channel": "dingtalk",
        "targetUser": "sender_002",
        "targetSession": "bbbbbbbb",
        "mentionSender": True,
        "text": "Project B is ready.",
        "agentId": "default",
    })
    if dingtalk_mention_missing.get("ok"):
        raise AssertionError(f"dingtalk mention missing webhook should fail: {dingtalk_mention_missing!r}")
    if "session webhook not found" not in dingtalk_mention_missing.get("error", ""):
        raise AssertionError(f"dingtalk mention missing webhook error missing: {dingtalk_mention_missing!r}")
    if dingtalk_mention_missing.get("delivery", {}).get("failed") != "dingtalk_sender_mention_required":
        raise AssertionError(f"dingtalk mention missing webhook delivery mismatch: {dingtalk_mention_missing!r}")
    if captured.get("qwenpaw_body") != qwenpaw_body_before_missing:
        raise AssertionError(f"dingtalk mention missing webhook must not fall back to qwenpaw API: {captured!r}")

    webhook_store = qwenpaw_dir / "workspaces" / "default" / "dingtalk_session_webhooks.json"
    webhook_store.write_text(json.dumps({
        "dingtalk:sw:sender_001_aaaaaaaa": {
            "webhook": f"http://127.0.0.1:{server.server_address[1]}/dingtalk-webhook",
            "conversation_type": "group",
            "sender_staff_id": "staff_001",
        }
    }), encoding="utf-8")
    webhook_count_before_mismatch = captured.get("dingtalk_webhook_count", 0)
    qwenpaw_body_before_mismatch = captured.get("qwenpaw_body")
    dingtalk_mention_mismatch = payload({
        "action": "send",
        "replyRoute": {
            "channel": "dingtalk",
            "targetUser": "sender_001",
            "targetSession": "aaaaaaaa",
            "mentionSender": True,
            "senderStaffId": "staff_wrong",
        },
        "text": "Project C is ready.",
        "agentId": "default",
    })
    if dingtalk_mention_mismatch.get("ok"):
        raise AssertionError(f"dingtalk mention mismatch should fail: {dingtalk_mention_mismatch!r}")
    if "does not match recorded sender" not in dingtalk_mention_mismatch.get("error", ""):
        raise AssertionError(f"dingtalk mention mismatch warning missing: {dingtalk_mention_mismatch!r}")
    if dingtalk_mention_mismatch.get("delivery", {}).get("failed") != "dingtalk_sender_mention_required":
        raise AssertionError(f"dingtalk mention mismatch delivery mismatch: {dingtalk_mention_mismatch!r}")
    if captured.get("dingtalk_webhook_count", 0) != webhook_count_before_mismatch:
        raise AssertionError(f"dingtalk mention mismatch should not call webhook: {captured!r}")
    if captured.get("qwenpaw_body") != qwenpaw_body_before_mismatch:
        raise AssertionError(f"dingtalk mention mismatch must not fall back to qwenpaw API: {captured!r}")

    dingtalk_mention_sent = payload({
        "action": "send",
        "replyRoute": {
            "channel": "dingtalk",
            "targetUser": "sender_001",
            "targetSession": "aaaaaaaa",
        },
        "mentionSender": True,
        "text": "Project A is ready.",
        "agentId": "default",
    })
    server.shutdown()
    if not dingtalk_mention_sent.get("ok"):
        raise AssertionError(f"dingtalk mention send failed: {dingtalk_mention_sent!r}")
    if dingtalk_mention_sent.get("senderMentioned") is not True:
        raise AssertionError(f"dingtalk mention marker missing: {dingtalk_mention_sent!r}")
    if dingtalk_mention_sent.get("mentionedSender") != "staff_001":
        raise AssertionError(f"dingtalk mentioned sender mismatch: {dingtalk_mention_sent!r}")
    if captured.get("dingtalk_webhook_path") != "/dingtalk-webhook":
        raise AssertionError(f"dingtalk webhook path mismatch: {captured!r}")
    webhook_body = captured.get("dingtalk_webhook_body", {})
    if webhook_body.get("at", {}).get("atUserIds") != ["staff_001"]:
        raise AssertionError(f"dingtalk webhook at payload mismatch: {webhook_body!r}")
    webhook_text = webhook_body.get("markdown", {}).get("text") or webhook_body.get("text", {}).get("content") or ""
    if "@staff_001" not in webhook_text or "Project A is ready." not in webhook_text:
        raise AssertionError(f"dingtalk webhook text mismatch: {webhook_body!r}")
    dingtalk_session = json.loads(dingtalk_session_path.read_text(encoding="utf-8"))
    dingtalk_msg, _dingtalk_marks = dingtalk_session["agent"]["memory"]["content"][-1]
    if dingtalk_msg.get("metadata") != {
        "channel": "dingtalk",
        "message_id": "",
        "source": "message_tool_outbound",
        "user_id": "sender_001",
        "session_id": "aaaaaaaa",
        "sender_mentioned": True,
        "mentioned_sender": "staff_001",
    }:
        raise AssertionError(f"dingtalk mention session metadata mismatch: {dingtalk_msg!r}")

    os.environ["AGENTTEAMS_AGENT_ROLE"] = "worker"
    worker_tools = [tool["name"] for tool in list_tools()]
    if "message" in worker_tools:
        raise AssertionError(f"worker should not see message tool: {worker_tools!r}")
    blocked_result = call({
        "action": "send",
        "channel": "matrix",
        "target": "room:!room:example.test",
        "message": "should be blocked",
        "role": "leader",
        "dryRun": True,
    })
    blocked = json.loads(blocked_result["content"][0]["text"])
    if blocked.get("ok") is not False or blocked.get("error") != "forbidden_tool":
        raise AssertionError(f"worker message call should be blocked: {blocked!r}")
    if blocked_result.get("isError") is not True:
        raise AssertionError(f"expected isError True for forbidden tool: {blocked_result!r}")

    print(json.dumps({
        "ok": True,
        "dryRunMentions": dry["mentions"],
        "messageId": sent["messageId"],
        "sendPath": captured["path"],
        "qwenpawSendPath": captured["qwenpaw_path"],
        "triggerStatus": trigger["status"],
    }, ensure_ascii=False))
  PY

  stdout, stderr, status = Open3.capture3("python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness message MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  puts JSON.pretty_generate(JSON.parse(stdout))
end
