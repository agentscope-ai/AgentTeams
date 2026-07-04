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

Dir.mktmpdir("teamharness-roomflow-") do |dir|
  python_test = <<~PY
    import http.server
    import json
    import os
    import pathlib
    import socketserver
    import sys
    import threading

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    from server import call_tool

    runtime_config = pathlib.Path("#{dir}") / "runtime.yaml"
    runtime_config.write_text(json.dumps({
        "team": {
            "admin": {
                "name": "admin",
                "matrixUserId": "@runtime-admin:example.test",
            },
        },
    }), encoding="utf-8")
    runtime_config_without_admin = pathlib.Path("#{dir}") / "runtime-without-admin.yaml"
    runtime_config_without_admin.write_text(json.dumps({
        "member": {"matrixUserId": "@leader:example.test"},
        "team": {"leaderDmRoomId": "!leader-dm:example.test"},
    }), encoding="utf-8")

    captured = {"posts": [], "gets": []}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length).decode("utf-8")
            body = json.loads(raw or "{}")
            captured["posts"].append({
                "path": self.path,
                "auth": self.headers.get("Authorization"),
                "body": body,
            })
            if self.path == "/_matrix/client/v3/createRoom":
                payload = json.dumps({"room_id": "!created:example.test"}).encode("utf-8")
            elif self.path == "/_matrix/client/v3/rooms/%21created%3Aexample.test/leave":
                payload = b"{}"
            else:
                self.send_response(404)
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def do_GET(self):
            captured["gets"].append({
                "path": self.path,
                "auth": self.headers.get("Authorization"),
            })
            if self.path == "/_matrix/client/v3/rooms/%21leader-dm%3Aexample.test/members":
                payload = json.dumps({"chunk": [
                    {"state_key": "@leader:example.test", "content": {"membership": "join"}},
                    {"state_key": "@team-admin:example.test", "content": {"membership": "join"}},
                ]}).encode("utf-8")
            elif self.path == "/_matrix/client/v3/rooms/%21created%3Aexample.test/members":
                payload = json.dumps({"chunk": [
                    {"state_key": "@worker:example.test", "content": {"membership": "join"}},
                    {"state_key": "@admin:example.test", "content": {"membership": "join"}},
                ]}).encode("utf-8")
            elif self.path == "/_matrix/client/v3/joined_rooms":
                payload = json.dumps({"joined_rooms": ["!created:example.test"]}).encode("utf-8")
            else:
                self.send_response(404)
                self.end_headers()
                return
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
        result = call_tool("roomflow", args)
        return json.loads(result["content"][0]["text"])

    dry = payload({
        "action": "create_task_room",
        "taskId": "demo-project-001",
        "name": "Task: demo",
        "source": "matrix",
        "invite": ["@worker:example.test"],
        "admin": "@admin:example.test",
        "dryRun": True,
    })
    if not dry.get("ok") or not dry.get("dryRun"):
        raise AssertionError(f"dry create failed: {dry!r}")
    if dry.get("invite") != ["@worker:example.test", "@admin:example.test"]:
        raise AssertionError(f"invite list mismatch: {dry!r}")
    if dry["content"].get("power_level_content_override", {}).get("users", {}).get("@admin:example.test") != 100:
        raise AssertionError(f"admin power level missing: {dry!r}")

    os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime_config)
    dry_runtime_admin = payload({
        "action": "create_task_room",
        "taskId": "demo-project-002",
        "name": "Task: runtime admin",
        "source": "matrix",
        "invite": ["@worker:example.test"],
        "dryRun": True,
    })
    if dry_runtime_admin.get("invite") != ["@worker:example.test", "@runtime-admin:example.test"]:
        raise AssertionError(f"runtime admin was not auto-invited: {dry_runtime_admin!r}")
    if dry_runtime_admin["content"].get("power_level_content_override", {}).get("users", {}).get("@runtime-admin:example.test") != 100:
        raise AssertionError(f"runtime admin power level missing: {dry_runtime_admin!r}")
    os.environ.pop("TEAMHARNESS_RUNTIME_CONFIG", None)

    invalid = payload({
        "action": "create_task_room",
        "taskId": "bad/id",
        "name": "bad",
        "dryRun": True,
    })
    if invalid.get("ok") is not False or "safe id" not in invalid.get("error", ""):
        raise AssertionError(f"unsafe task id was not rejected: {invalid!r}")

    os.environ["HICLAW_MATRIX_URL"] = f"http://127.0.0.1:{server.server_address[1]}"
    os.environ["HICLAW_WORKER_MATRIX_TOKEN"] = "test-token"
    os.environ["HICLAW_MATRIX_USER_ID"] = "@leader:example.test"

    created = payload({
        "action": "create_task_room",
        "taskId": "demo-project-001",
        "name": "Task: demo",
        "source": "matrix",
        "invite": "@worker:example.test",
    })
    if not created.get("ok") or created.get("roomId") != "!created:example.test":
        raise AssertionError(f"create failed: {created!r}")
    if created.get("target") != "room:!created:example.test":
        raise AssertionError(f"target mismatch: {created!r}")
    create_call = captured["posts"][0]
    if create_call.get("auth") != "Bearer test-token":
        raise AssertionError(f"auth mismatch: {captured!r}")
    if create_call["body"].get("name") != "Task: demo":
        raise AssertionError(f"room name mismatch: {captured!r}")
    if create_call["body"].get("invite") != ["@worker:example.test"]:
        raise AssertionError(f"real invite mismatch: {captured!r}")

    listed = payload({"action": "list_rooms"})
    if not listed.get("ok") or listed.get("rooms") != ["!created:example.test"]:
        raise AssertionError(f"list failed: {listed!r}")
    if captured["gets"][0].get("auth") != "Bearer test-token":
        raise AssertionError(f"list auth mismatch: {captured!r}")

    missing_source_room = payload({
        "action": "create_task_room",
        "taskId": "demo-project-004",
        "name": "Task: missing external source room",
        "source": "dingtalk",
        "invite": ["@worker:example.test"],
        "workspaceDir": "#{dir}",
        "dryRun": True,
    })
    if missing_source_room.get("ok") is not False or "sourceRoomId is required" not in missing_source_room.get("error", ""):
        raise AssertionError(f"missing sourceRoomId was not rejected: {missing_source_room!r}")

    external_created = payload({
        "action": "create_task_room",
        "taskId": "demo-project-005",
        "name": "Task: external source room",
        "source": "dingtalk",
        "sourceRoomId": "ding-room-001",
        "invite": ["@worker:example.test"],
        "admin": "@admin:example.test",
        "workspaceDir": "#{dir}",
    })
    if not external_created.get("ok") or external_created.get("roomId") != "!created:example.test":
        raise AssertionError(f"external create failed: {external_created!r}")
    if external_created.get("sourceRoomKey") != "dingtalk:ding-room-001":
        raise AssertionError(f"source room key mismatch: {external_created!r}")
    create_posts = len([post for post in captured["posts"] if post["path"] == "/_matrix/client/v3/createRoom"])
    external_reused = payload({
        "action": "create_task_room",
        "taskId": "demo-project-006",
        "name": "Task: external source room again",
        "source": "dingtalk",
        "sourceRoomId": "ding-room-001",
        "invite": ["@worker:example.test"],
        "admin": "@admin:example.test",
        "workspaceDir": "#{dir}",
    })
    if not external_reused.get("ok") or external_reused.get("roomId") != "!created:example.test" or external_reused.get("reused") is not True:
        raise AssertionError(f"external room was not reused: {external_reused!r}")
    create_posts_after_reuse = len([post for post in captured["posts"] if post["path"] == "/_matrix/client/v3/createRoom"])
    if create_posts_after_reuse != create_posts:
        raise AssertionError(f"external reuse created a new room: {captured!r}")

    os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime_config_without_admin)
    saved_matrix_user_id = os.environ.pop("HICLAW_MATRIX_USER_ID", None)
    saved_worker_name = os.environ.pop("HICLAW_WORKER_NAME", None)
    dry_leader_dm_admin = payload({
        "action": "create_task_room",
        "taskId": "demo-project-003",
        "name": "Task: leader DM inferred admin",
        "source": "matrix",
        "invite": ["@worker:example.test"],
        "dryRun": True,
    })
    if saved_matrix_user_id is not None:
        os.environ["HICLAW_MATRIX_USER_ID"] = saved_matrix_user_id
    if saved_worker_name is not None:
        os.environ["HICLAW_WORKER_NAME"] = saved_worker_name
    if dry_leader_dm_admin.get("invite") != ["@worker:example.test", "@team-admin:example.test"]:
        raise AssertionError(f"leader DM admin was not auto-invited: {dry_leader_dm_admin!r}")
    if dry_leader_dm_admin["content"].get("power_level_content_override", {}).get("users", {}).get("@leader:example.test") != 100:
        raise AssertionError(f"runtime member creator power level missing: {dry_leader_dm_admin!r}")
    if dry_leader_dm_admin["content"].get("power_level_content_override", {}).get("users", {}).get("@team-admin:example.test") != 100:
        raise AssertionError(f"leader DM admin power level missing: {dry_leader_dm_admin!r}")
    os.environ.pop("TEAMHARNESS_RUNTIME_CONFIG", None)

    archived = payload({"action": "archive_room", "roomId": "room:!created:example.test"})
    server.shutdown()
    if not archived.get("ok") or archived.get("archived") is not True:
        raise AssertionError(f"archive failed: {archived!r}")
    if captured["posts"][-1].get("path") != "/_matrix/client/v3/rooms/%21created%3Aexample.test/leave":
        raise AssertionError(f"archive path mismatch: {captured!r}")

    print(json.dumps({
        "ok": True,
        "roomId": created["roomId"],
        "rooms": listed["rooms"],
        "archivePath": captured["posts"][-1]["path"],
    }, ensure_ascii=False))
  PY

  stdout, stderr, status = Open3.capture3("python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness roomflow MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  puts JSON.pretty_generate(JSON.parse(stdout))
end
