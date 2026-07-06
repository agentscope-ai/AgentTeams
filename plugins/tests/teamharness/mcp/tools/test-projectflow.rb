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

Dir.mktmpdir("teamharness-projectflow-") do |dir|
  root = Pathname.new(dir)
  workspace = root / "workspace"
  bin_dir = root / "bin"
  log_path = root / "mc.log"
  bin_dir.mkpath
  (bin_dir / "mc").write(<<~SH)
    #!/usr/bin/env bash
    printf '%s\\n' "$*" >> "#{log_path}"
  SH
  (bin_dir / "mc").chmod(0o755)

  python_test = <<~PY
    import http.server
    import json
    import os
    import pathlib
    import socketserver
    import sys
    import threading
    import time
    import urllib.parse

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    import server
    from server import call_tool

    server.time.strftime = lambda fmt: "20260605-112233"

    workspace = pathlib.Path("#{workspace}")
    real_shared = pathlib.Path("#{root}") / "real-shared"
    workspace.mkdir(parents=True, exist_ok=True)
    real_shared.mkdir(parents=True, exist_ok=True)
    (workspace / "shared").symlink_to(
        os.path.relpath(real_shared, workspace),
        target_is_directory=True,
    )
    os.environ["AGENTTEAMS_SHARED_DIR"] = str(real_shared)
    os.environ["TEAMHARNESS_SHARED_DIR"] = str(real_shared)

    common = {
        "workspaceDir": str(workspace),
        "storage": {
            "sharedPrefix": "mock/shared",
            "globalSharedPrefix": "mock/global-shared",
        },
    }
    runtime_config = pathlib.Path("#{root}") / "runtime.yaml"
    runtime_config.write_text(
        "team:\\n  teamRoomId: '!team:example.test'\\n",
        encoding="utf-8",
    )
    os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(runtime_config)

    matrix = {"uploads": [], "events": []}

    class MatrixHandler(http.server.BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            return

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path != "/_matrix/media/v3/upload":
                self.send_response(404)
                self.end_headers()
                return
            query = urllib.parse.parse_qs(parsed.query)
            filename = query.get("filename", ["artifact"])[0]
            matrix["uploads"].append({
                "filename": filename,
                "body": body.decode("utf-8", errors="replace"),
                "auth": self.headers.get("Authorization"),
                "content_type": self.headers.get("Content-Type"),
            })
            payload = {"content_uri": f"mxc://example.test/{len(matrix['uploads'])}-{filename}"}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))

        def do_PUT(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if "/send/m.room.message/" not in parsed.path:
                self.send_response(404)
                self.end_headers()
                return
            matrix["events"].append({
                "path": parsed.path,
                "auth": self.headers.get("Authorization"),
                "content": json.loads(body.decode("utf-8") or "{}"),
            })
            payload = {"event_id": f"$project-file-event-{len(matrix['events'])}"}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))

    class MatrixServer(socketserver.ThreadingTCPServer):
        allow_reuse_address = True
        daemon_threads = True

    matrix_server = MatrixServer(("127.0.0.1", 0), MatrixHandler)
    matrix_thread = threading.Thread(target=matrix_server.serve_forever, daemon=True)
    matrix_thread.start()
    matrix_port = matrix_server.server_address[1]
    os.environ["AGENTTEAMS_MATRIX_URL"] = f"http://127.0.0.1:{matrix_port}"
    os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = "test-token"
    context_file = pathlib.Path("#{root}") / "matrix-context.json"
    os.environ["TEAMHARNESS_MATRIX_CONTEXT_FILE"] = str(context_file)

    def tool_payload(name, args):
        merged = dict(common)
        merged.update(args)
        result = call_tool(name, merged)
        return json.loads(result["content"][0]["text"])

    def payload(args):
        return tool_payload("projectflow", args)

    generated = payload({
        "action": "create_project",
        "payload": {"title": "Fix Auth Flow!"},
    })
    if not generated.get("ok"):
        raise AssertionError(f"generated create_project failed: {generated!r}")
    if generated["project"]["project_id"] != "fix-auth-flow-20260605-112233":
        raise AssertionError(f"generated project id mismatch: {generated!r}")

    generated_again = payload({
        "action": "create_project",
        "payload": {"title": "Fix Auth Flow!"},
    })
    if not generated_again.get("ok"):
        raise AssertionError(f"generated collision create_project failed: {generated_again!r}")
    if generated_again["project"]["project_id"] != "fix-auth-flow-20260605-112233-01":
        raise AssertionError(f"generated collision suffix mismatch: {generated_again!r}")

    created = payload({
        "action": "create_project",
        "payload": {
            "projectId": "daily-plan-2026-06-03",
            "title": "Daily Plan",
            "source": "test",
            "requester": "@admin:example.test",
            "replyRoute": {
                "channel": "dingtalk",
                "targetUser": "sender_001",
                "targetSession": "aaaaaaaa",
            },
        },
    })
    if not created.get("ok"):
        raise AssertionError(f"create_project failed: {created!r}")
    if created["project"]["project_id"] != "daily-plan-2026-06-03":
        raise AssertionError(f"project id mismatch: {created!r}")
    expected_reply_route = {
        "channel": "dingtalk",
        "target_user": "sender_001",
        "target_session": "aaaaaaaa",
    }
    if created["project"].get("reply_route") != expected_reply_route:
        raise AssertionError(f"reply route mismatch: {created!r}")
    if created["project"].get("source_room_id") != "aaaaaaaa":
        raise AssertionError(f"source room id should come from reply route target session: {created!r}")

    matrix_dm_created = payload({
        "action": "create_project",
        "payload": {
            "projectId": "matrix-dm-project",
            "title": "Matrix DM Project",
            "source": "matrix",
            "requester": "@admin:example.test",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!dm-room:example.test",
            },
        },
    })
    if not matrix_dm_created.get("ok"):
        raise AssertionError(f"matrix DM create_project failed: {matrix_dm_created!r}")
    expected_matrix_dm_route = {
        "channel": "matrix",
        "target_user": "@admin:example.test",
        "target_session": "!dm-room:example.test",
    }
    if matrix_dm_created["project"].get("reply_route") != expected_matrix_dm_route:
        raise AssertionError(f"matrix DM reply route mismatch: {matrix_dm_created!r}")
    if matrix_dm_created["project"].get("source_room_id") != "!dm-room:example.test":
        raise AssertionError(f"matrix DM source room id should come from reply route target session: {matrix_dm_created!r}")
    resolved_matrix_dm = payload({
        "action": "resolve_project",
        "payload": {"projectId": "matrix-dm-project"},
    })
    if resolved_matrix_dm.get("replyRoute") != expected_matrix_dm_route:
        raise AssertionError(f"resolve_project lost matrix DM reply route: {resolved_matrix_dm!r}")
    if resolved_matrix_dm.get("sourceRoomId") != "!dm-room:example.test":
        raise AssertionError(f"resolve_project lost matrix DM sourceRoomId: {resolved_matrix_dm!r}")

    matrix_plan = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "matrix-dm-project",
            "tasks": [{
                "taskId": "matrix-dm-project-01",
                "title": "Write project report",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    if not matrix_plan.get("ok"):
        raise AssertionError(f"matrix project plan_dag failed: {matrix_plan!r}")
    project_result_path = pathlib.Path("#{workspace}") / "shared/projects/matrix-dm-project/result.md"
    project_result_path.parent.mkdir(parents=True, exist_ok=True)
    project_result_path.write_text("# Matrix DM Project Result\\n\\nReady for requester.\\n", encoding="utf-8")
    matrix_accepted = payload({
        "action": "accept_task_result",
        "payload": {
            "projectId": "matrix-dm-project",
            "taskId": "matrix-dm-project-01",
            "resultStatus": "SUCCESS",
            "summary": "Project report accepted.",
            "parentEventId": "$project-parent",
            "publishArtifacts": True,
        },
    })
    if not matrix_accepted.get("ok"):
        raise AssertionError(f"matrix accept_task_result failed: {matrix_accepted!r}")
    matrix_artifacts = matrix_accepted.get("publishedArtifacts") or []
    if len(matrix_artifacts) != 1 or matrix_artifacts[0].get("status") != "published":
        raise AssertionError(f"project result was not published: {matrix_accepted!r}")
    project_artifact = matrix_artifacts[0]
    if project_artifact.get("filename") != "matrix-dm-project-project-result.md":
        raise AssertionError(f"project artifact filename mismatch: {project_artifact!r}")
    if project_artifact.get("sourcePath") != "shared/projects/matrix-dm-project/result.md":
        raise AssertionError(f"project artifact source mismatch: {project_artifact!r}")
    if not project_artifact.get("mxcUri") or not project_artifact.get("eventId"):
        raise AssertionError(f"project artifact missing Matrix references: {project_artifact!r}")
    if project_artifact.get("parentEventId") != "$project-parent":
        raise AssertionError(f"project artifact missing parent event reference: {project_artifact!r}")
    if not matrix["uploads"] or matrix["uploads"][-1].get("filename") != "matrix-dm-project-project-result.md":
        raise AssertionError(f"project result upload missing: {matrix['uploads']!r}")
    project_file_event = matrix["events"][-1]["content"]
    if project_file_event.get("msgtype") != "m.file" or project_file_event.get("body") != "matrix-dm-project-project-result.md":
        raise AssertionError(f"project result event mismatch: {project_file_event!r}")
    if project_file_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$project-parent"}:
        raise AssertionError(f"project result event missing attachment relation: {project_file_event!r}")
    if not project_file_event.get("url") or not (project_file_event.get("info") or {}).get("size"):
        raise AssertionError(f"project result event missing file metadata: {project_file_event!r}")

    context_project = payload({
        "action": "create_project",
        "payload": {
            "projectId": "matrix-context-project",
            "title": "Matrix Context Project",
            "source": "matrix",
            "requester": "@admin:example.test",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!dm-room:example.test",
            },
        },
    })
    if not context_project.get("ok"):
        raise AssertionError(f"context create_project failed: {context_project!r}")
    context_plan = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "matrix-context-project",
            "tasks": [{
                "taskId": "matrix-context-project-01",
                "title": "Write context project report",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    if not context_plan.get("ok"):
        raise AssertionError(f"context plan_dag failed: {context_plan!r}")
    context_result_path = pathlib.Path("#{workspace}") / "shared/projects/matrix-context-project/result.md"
    context_result_path.parent.mkdir(parents=True, exist_ok=True)
    context_result_path.write_text("# Matrix Context Project Result\\n\\nReady.\\n", encoding="utf-8")
    context_file.write_text(json.dumps({
        "rooms": {
            "!dm-room:example.test": {
                "attachmentParentEventId": "$context-project-parent",
                "updatedAt": time.time(),
            }
        }
    }), encoding="utf-8")
    context_accepted = payload({
        "action": "accept_task_result",
        "payload": {
            "projectId": "matrix-context-project",
            "taskId": "matrix-context-project-01",
            "resultStatus": "SUCCESS",
            "summary": "Project report accepted using Matrix context parent.",
            "publishArtifacts": True,
        },
    })
    if not context_accepted.get("ok"):
        raise AssertionError(f"context accept_task_result failed: {context_accepted!r}")
    context_artifacts = context_accepted.get("publishedArtifacts") or []
    if len(context_artifacts) != 1 or context_artifacts[0].get("status") != "published":
        raise AssertionError(f"context project result was not published: {context_accepted!r}")
    if context_artifacts[0].get("parentEventId") != "$context-project-parent":
        raise AssertionError(f"context project result did not infer parent event: {context_accepted!r}")
    context_project_file_event = matrix["events"][-1]["content"]
    if context_project_file_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$context-project-parent"}:
        raise AssertionError(f"context project result event missing attachment relation: {context_project_file_event!r}")

    deferred_project = payload({
        "action": "create_project",
        "payload": {
            "projectId": "deferred-report-project",
            "title": "Deferred Report Project",
            "source": "matrix",
            "requester": "@admin:example.test",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!dm-room:example.test",
            },
        },
    })
    if not deferred_project.get("ok"):
        raise AssertionError(f"deferred create_project failed: {deferred_project!r}")
    deferred_plan = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "deferred-report-project",
            "tasks": [{
                "taskId": "deferred-report-project-01",
                "title": "Write deferred report",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    if not deferred_plan.get("ok"):
        raise AssertionError(f"deferred plan_dag failed: {deferred_plan!r}")
    deferred_result_path = pathlib.Path("#{workspace}") / "shared/projects/deferred-report-project/result.md"
    deferred_result_path.parent.mkdir(parents=True, exist_ok=True)
    deferred_result_path.write_text("# Deferred Project Result\\n\\nPublish after report.\\n", encoding="utf-8")
    uploads_before_deferred_accept = len(matrix["uploads"])
    deferred_accepted = payload({
        "action": "accept_task_result",
        "payload": {
            "projectId": "deferred-report-project",
            "taskId": "deferred-report-project-01",
            "resultStatus": "SUCCESS",
            "summary": "Project report accepted but artifact publish is deferred.",
        },
    })
    if not deferred_accepted.get("ok"):
        raise AssertionError(f"deferred accept_task_result failed: {deferred_accepted!r}")
    if deferred_accepted.get("publishedArtifacts") != []:
        raise AssertionError(f"deferred accept should not publish artifacts: {deferred_accepted!r}")
    if len(matrix["uploads"]) != uploads_before_deferred_accept:
        raise AssertionError(f"deferred accept uploaded project artifact unexpectedly: {matrix['uploads']!r}")

    uploads_before_complete = len(matrix["uploads"])
    matrix_completed = payload({
        "action": "complete_project",
        "payload": {"projectId": "matrix-dm-project"},
    })
    if not matrix_completed.get("ok"):
        raise AssertionError(f"matrix complete_project failed: {matrix_completed!r}")
    if matrix_completed.get("publishedArtifacts"):
        raise AssertionError(f"complete_project should not publish project artifacts by default: {matrix_completed!r}")
    if len(matrix["uploads"]) != uploads_before_complete:
        raise AssertionError(f"complete_project uploaded project artifact unexpectedly: {matrix['uploads']!r}")

    report = tool_payload("message", {
        "action": "send",
        "replyRoute": expected_matrix_dm_route,
        "text": "Matrix DM Project is complete.",
    })
    if not report.get("ok") or not report.get("messageId"):
        raise AssertionError(f"requester report send failed: {report!r}")
    report_artifact = tool_payload("artifact", {
        "action": "publish_file",
        "path": "shared/projects/matrix-dm-project/result.md",
        "filename": "matrix-dm-project-project-result.md",
        "replyRoute": expected_matrix_dm_route,
        "parentEventId": report["messageId"],
    })
    if not report_artifact.get("ok"):
        raise AssertionError(f"report artifact publish failed: {report_artifact!r}")
    artifact_payload = report_artifact.get("artifact") or {}
    if artifact_payload.get("parentEventId") != report["messageId"]:
        raise AssertionError(f"report artifact parent should be report message id: {report_artifact!r}")
    report_file_event = matrix["events"][-1]["content"]
    if report_file_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": report["messageId"]}:
        raise AssertionError(f"report artifact Matrix relation mismatch: {report_file_event!r}")

    duplicate_explicit = payload({
        "action": "create_project",
        "payload": {"projectId": "daily-plan-2026-06-03", "title": "Duplicate Daily Plan"},
    })
    if duplicate_explicit.get("ok") or "project already exists" not in duplicate_explicit.get("error", ""):
        raise AssertionError(f"explicit project id collision should be rejected: {duplicate_explicit!r}")

    quick = payload({
        "action": "create_quick_project",
        "payload": {
            "title": "Write Readiness Note",
            "source": "matrix",
            "requester": "@admin:example.test",
            "assignedTo": "@worker-a:example.test",
            "roomId": "!team:example.test",
            "spec": "Write one concise readiness note.",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!team:example.test",
            },
        },
    })
    if not quick.get("ok"):
        raise AssertionError(f"create_quick_project failed: {quick!r}")
    quick_project = quick["project"]
    quick_task = quick["task"]
    if quick_project["project_id"] != "write-readiness-note-20260605-112233":
        raise AssertionError(f"quick project id mismatch: {quick!r}")
    if quick_project.get("mode") != "quick" or quick_project.get("plan_type") != "dag":
        raise AssertionError(f"quick project should be quick DAG: {quick!r}")
    if quick_task["task_id"] != "write-readiness-note-20260605-112233-01":
        raise AssertionError(f"quick task id mismatch: {quick!r}")
    if quick_task.get("status") != "assigned" or quick_project["tasks"][0].get("status") != "assigned":
        raise AssertionError(f"quick project task should be assigned: {quick!r}")
    if quick.get("synced") is not True:
        raise AssertionError(f"quick project did not sync task dir: {quick!r}")
    quick_spec_path = pathlib.Path("#{workspace}") / "shared/tasks/write-readiness-note-20260605-112233-01/spec.md"
    if quick_spec_path.read_text(encoding="utf-8").strip() != "Write one concise readiness note.":
        raise AssertionError("quick task spec was not written")
    quick_meta_path = pathlib.Path("#{workspace}") / "shared/tasks/write-readiness-note-20260605-112233-01/meta.json"
    quick_legacy_state_path = pathlib.Path("#{workspace}") / "shared/tasks/write-readiness-note-20260605-112233-01/task.json"
    if not quick_meta_path.exists():
        raise AssertionError(f"quick task meta.json was not written: {quick_meta_path}")
    if quick_legacy_state_path.exists():
        raise AssertionError(f"task.json should not be written: {quick_legacy_state_path}")
    quick_meta = json.loads(quick_meta_path.read_text(encoding="utf-8"))
    if quick_meta.get("room_id") != "!team:example.test" or quick_meta.get("project_id") != quick_project["project_id"]:
        raise AssertionError(f"quick task meta mismatch: {quick_meta!r}")
    if quick_meta.get("task_title") != "Write Readiness Note":
        raise AssertionError(f"quick task meta missing console task_title: {quick_meta!r}")
    if quick_meta.get("assigned_to") != "@worker-a:example.test" or not quick_meta.get("assigned_at"):
        raise AssertionError(f"quick task meta missing console assignment fields: {quick_meta!r}")

    external_quick = payload({
        "action": "create_quick_project",
        "payload": {
            "projectId": "external-quick-project",
            "title": "External Quick Project",
            "source": "dingtalk",
            "requester": "dingtalk:sender_003:cccccccc",
            "assignedTo": "@worker-a:example.test",
            "roomId": "room:!team:example.test",
            "spec": "Should require a dedicated task room.",
        },
    })
    if external_quick.get("ok") or "dedicated task room" not in external_quick.get("error", ""):
        raise AssertionError(f"external quick project with team room should fail: {external_quick!r}")
    external_quick_ok = payload({
        "action": "create_quick_project",
        "payload": {
            "projectId": "external-quick-project-ok",
            "title": "External Quick Project OK",
            "source": "dingtalk",
            "requester": "dingtalk:sender_003:cccccccc",
            "assignedTo": "@worker-a:example.test",
            "roomId": "room:!task-room:example.test",
            "spec": "Use the dedicated assignment room.",
        },
    })
    if not external_quick_ok.get("ok") or external_quick_ok["task"].get("room_id") != "room:!task-room:example.test":
        raise AssertionError(f"external quick project with dedicated room failed: {external_quick_ok!r}")
    resolved_external_quick = payload({
        "action": "resolve_project",
        "payload": {"taskId": external_quick_ok["task"]["task_id"]},
    })
    if resolved_external_quick.get("replyRoute") != external_quick_ok["project"].get("reply_route"):
        raise AssertionError(f"resolve_project lost external quick reply route: {resolved_external_quick!r}")
    if resolved_external_quick.get("sourceRoomId") != "cccccccc":
        raise AssertionError(f"resolve_project lost external quick sourceRoomId: {resolved_external_quick!r}")

    resolved_quick = payload({
        "action": "resolve_project",
        "payload": {"taskId": quick_task["task_id"]},
    })
    if not resolved_quick.get("ok"):
        raise AssertionError(f"resolve_project from taskId failed: {resolved_quick!r}")
    if resolved_quick["project"]["project_id"] != quick_project["project_id"]:
        raise AssertionError(f"resolve_project returned wrong project: {resolved_quick!r}")
    if resolved_quick.get("task", {}).get("task_id") != quick_task["task_id"]:
        raise AssertionError(f"resolve_project returned wrong task: {resolved_quick!r}")
    if resolved_quick.get("replyRoute") != quick_project.get("reply_route"):
        raise AssertionError(f"resolve_project lost reply route: {resolved_quick!r}")
    if resolved_quick.get("planType") != "dag":
        raise AssertionError(f"resolve_project lost plan type: {resolved_quick!r}")

    created_from_string_payload = payload({
        "action": "create_project",
        "payload": json.dumps({
            "projectId": "string-payload-project",
            "title": "String Payload Project",
        }),
    })
    if not created_from_string_payload.get("ok"):
        raise AssertionError(f"create_project with string payload failed: {created_from_string_payload!r}")

    created_from_requester = payload({
        "action": "create_project",
        "payload": {
            "projectId": "legacy-dingtalk-requester",
            "title": "Legacy DingTalk Requester",
            "source": "dingtalk",
            "requester": "dingtalk:sender_002:bbbbbbbb",
        },
    })
    if not created_from_requester.get("ok"):
        raise AssertionError(f"create_project with dingtalk requester failed: {created_from_requester!r}")
    expected_legacy_route = {
        "channel": "dingtalk",
        "target_user": "sender_002",
        "target_session": "bbbbbbbb",
    }
    if created_from_requester["project"].get("reply_route") != expected_legacy_route:
        raise AssertionError(f"legacy requester reply route mismatch: {created_from_requester!r}")

    created_from_matrix_requester = payload({
        "action": "create_project",
        "payload": {
            "projectId": "legacy-matrix-requester",
            "title": "Legacy Matrix Requester",
            "source": "matrix",
            "requester": "matrix:!legacy-dm:example.test",
        },
    })
    if not created_from_matrix_requester.get("ok"):
        raise AssertionError(f"create_project with matrix requester failed: {created_from_matrix_requester!r}")
    expected_legacy_matrix_route = {
        "channel": "matrix",
        "target_session": "!legacy-dm:example.test",
    }
    if created_from_matrix_requester["project"].get("reply_route") != expected_legacy_matrix_route:
        raise AssertionError(f"legacy matrix requester reply route mismatch: {created_from_matrix_requester!r}")
    if created_from_matrix_requester["project"].get("source_room_id") != "!legacy-dm:example.test":
        raise AssertionError(f"legacy matrix requester sourceRoomId mismatch: {created_from_matrix_requester!r}")

    planned = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "daily-plan-2026-06-03",
            "tasks": [
                {
                    "taskId": "t-001",
                    "title": "Collect input",
                    "assignedTo": "@worker-a:example.test",
                    "dependsOn": [],
                },
                {
                    "taskId": "t-002",
                    "title": "Summarize",
                    "assignedTo": "@worker-b:example.test",
                    "dependsOn": ["t-001"],
                },
            ],
        },
    })
    if not planned.get("ok"):
        raise AssertionError(f"plan_dag failed: {planned!r}")
    ready = [task["task_id"] for task in planned.get("readyNodes", [])]
    if ready != ["t-001"]:
        raise AssertionError(f"unexpected ready nodes: {planned!r}")

    no_room_created = payload({
        "action": "create_project",
        "payload": {
            "projectId": "no-room-project",
            "title": "No Room Project",
            "replyRoute": {
                "channel": "dingtalk",
                "targetUser": "sender_009",
                "targetSession": "dddddddd",
            },
        },
    })
    if not no_room_created.get("ok"):
        raise AssertionError(f"no-room create_project failed: {no_room_created!r}")
    no_room_planned = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "no-room-project",
            "tasks": [{
                "taskId": "no-room-project-01",
                "title": "No room task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    if not no_room_planned.get("ok"):
        raise AssertionError(f"no-room plan_dag failed: {no_room_planned!r}")
    no_room_result_path = pathlib.Path("#{workspace}") / "shared/projects/no-room-project/result.md"
    no_room_result_path.write_text("# Daily Plan Result\\n\\nNo Matrix route.\\n", encoding="utf-8")
    no_room_accepted = payload({
        "action": "accept_task_result",
        "payload": {
            "projectId": "no-room-project",
            "taskId": "no-room-project-01",
            "resultStatus": "SUCCESS",
            "summary": "No Matrix room should skip artifact publish.",
        },
    })
    if not no_room_accepted.get("ok"):
        raise AssertionError(f"no-room accept_task_result failed: {no_room_accepted!r}")
    skipped_artifacts = no_room_accepted.get("publishedArtifacts") or []
    if skipped_artifacts:
        raise AssertionError(f"accept_task_result should not publish project artifacts by default: {no_room_accepted!r}")

    checked = payload({
        "action": "ready_nodes",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if [task["task_id"] for task in checked.get("readyNodes", [])] != ["t-001"]:
        raise AssertionError(f"ready_nodes mismatch: {checked!r}")

    duplicate = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "daily-plan-2026-06-03",
            "tasks": [
                {"taskId": "dup", "title": "First"},
                {"taskId": "dup", "title": "Second"},
            ],
        },
    })
    if duplicate.get("ok") or "duplicate task id" not in duplicate.get("error", ""):
        raise AssertionError(f"duplicate task id should be rejected: {duplicate!r}")

    cycle = payload({
        "action": "plan_dag",
        "payload": {
            "projectId": "daily-plan-2026-06-03",
            "tasks": [
                {"taskId": "cycle-a", "dependsOn": ["cycle-b"]},
                {"taskId": "cycle-b", "dependsOn": ["cycle-a"]},
            ],
        },
    })
    if cycle.get("ok") or "cycle" not in cycle.get("error", ""):
        raise AssertionError(f"cycle should be rejected: {cycle!r}")

    paused = payload({
        "action": "pause_project",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if not paused.get("ok") or paused["project"].get("status") != "paused":
        raise AssertionError(f"pause_project failed: {paused!r}")
    paused_ready = payload({
        "action": "ready_nodes",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if paused_ready.get("readyNodes"):
        raise AssertionError(f"paused project should have no ready nodes: {paused_ready!r}")

    resumed = payload({
        "action": "resume_project",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if not resumed.get("ok") or resumed["project"].get("status") != "active":
        raise AssertionError(f"resume_project failed: {resumed!r}")
    resumed_ready = payload({
        "action": "ready_nodes",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if [task["task_id"] for task in resumed_ready.get("readyNodes", [])] != ["t-001"]:
        raise AssertionError(f"resumed ready_nodes mismatch: {resumed_ready!r}")

    completed = payload({
        "action": "complete_project",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if not completed.get("ok") or completed["project"].get("status") != "completed":
        raise AssertionError(f"complete_project failed: {completed!r}")
    completed_ready = payload({
        "action": "ready_nodes",
        "payload": {"projectId": "daily-plan-2026-06-03"},
    })
    if completed_ready.get("readyNodes"):
        raise AssertionError(f"completed project should have no ready nodes: {completed_ready!r}")

    plan_path = pathlib.Path("#{workspace}") / "shared/projects/daily-plan-2026-06-03/plan.md"
    meta_path = pathlib.Path("#{workspace}") / "shared/projects/daily-plan-2026-06-03/meta.json"
    legacy_state_path = pathlib.Path("#{workspace}") / "shared/projects/daily-plan-2026-06-03/project.json"
    if not plan_path.exists() or not meta_path.exists():
        raise AssertionError(f"project files missing: {plan_path}, {meta_path}")
    if legacy_state_path.exists():
        raise AssertionError(f"project.json should not be written: {legacy_state_path}")
    plan_text = plan_path.read_text(encoding="utf-8")
    if "Daily Plan" not in plan_text or "t-001" not in plan_text:
        raise AssertionError(f"plan text missing project details: {plan_text!r}")
    if "Reply Route: `dingtalk/sender_001/aaaaaaaa`" not in plan_text:
        raise AssertionError(f"plan text missing safe reply route: {plan_text!r}")
    state = json.loads(meta_path.read_text(encoding="utf-8"))
    if state.get("reply_route") != expected_reply_route:
        raise AssertionError(f"state reply route mismatch: {state!r}")

    loop_created = payload({
        "action": "create_project",
        "payload": {
            "projectId": "iterative-fix-2026-06-03",
            "title": "Iterative Fix",
            "source": "test",
        },
    })
    if not loop_created.get("ok"):
        raise AssertionError(f"loop project create failed: {loop_created!r}")

    loop_planned = payload({
        "action": "plan_loop",
        "payload": {
            "projectId": "iterative-fix-2026-06-03",
            "goal": "Fix until tests pass",
            "stopCondition": "All target tests pass or max iterations reached",
            "iterationTemplate": "Inspect failure, apply one fix, rerun tests.",
            "maxIterations": 3,
            "currentIteration": 1,
            "tasks": [
                {
                    "taskId": "iterative-fix-2026-06-03-i001-01",
                    "title": "Run first fix pass",
                    "assignedTo": "@worker-a:example.test",
                    "dependsOn": [],
                },
                {
                    "taskId": "iterative-fix-2026-06-03-i001-02",
                    "title": "Verify first fix pass",
                    "assignedTo": "@worker-b:example.test",
                    "dependsOn": ["iterative-fix-2026-06-03-i001-01"],
                },
            ],
        },
    })
    if not loop_planned.get("ok"):
        raise AssertionError(f"plan_loop failed: {loop_planned!r}")
    loop_ready = [task["task_id"] for task in loop_planned.get("readyLoopNodes", [])]
    if loop_ready != ["iterative-fix-2026-06-03-i001-01"]:
        raise AssertionError(f"unexpected ready loop nodes: {loop_planned!r}")

    loop_cycle = payload({
        "action": "plan_loop",
        "payload": {
            "projectId": "iterative-fix-2026-06-03",
            "goal": "Fix until tests pass",
            "stopCondition": "All target tests pass or max iterations reached",
            "iterationTemplate": "Inspect failure, apply one fix, rerun tests.",
            "maxIterations": 3,
            "currentIteration": 1,
            "tasks": [
                {"taskId": "loop-a", "dependsOn": ["loop-b"]},
                {"taskId": "loop-b", "dependsOn": ["loop-a"]},
            ],
        },
    })
    if loop_cycle.get("ok") or "cycle" not in loop_cycle.get("error", ""):
        raise AssertionError(f"loop cycle should be rejected: {loop_cycle!r}")

    loop_checked = payload({
        "action": "ready_loop_nodes",
        "payload": {"projectId": "iterative-fix-2026-06-03"},
    })
    if [task["task_id"] for task in loop_checked.get("readyLoopNodes", [])] != loop_ready:
        raise AssertionError(f"ready_loop_nodes mismatch: {loop_checked!r}")

    loop_recorded = payload({
        "action": "record_loop_iteration",
        "payload": {
            "projectId": "iterative-fix-2026-06-03",
            "iteration": 1,
            "decision": "continue",
            "summary": "First pass found another failure.",
            "nextAction": "Plan the second pass.",
        },
    })
    if not loop_recorded.get("ok"):
        raise AssertionError(f"record_loop_iteration failed: {loop_recorded!r}")
    if loop_recorded["loop"].get("status") != "running":
        raise AssertionError(f"loop status mismatch: {loop_recorded!r}")
    if not loop_recorded["loop"].get("history"):
        raise AssertionError(f"loop history missing: {loop_recorded!r}")

    loop_plan_path = pathlib.Path("#{workspace}") / "shared/projects/iterative-fix-2026-06-03/plan.md"
    loop_plan_text = loop_plan_path.read_text(encoding="utf-8")
    if "Plan Type: `loop`" not in loop_plan_text or "Fix until tests pass" not in loop_plan_text:
        raise AssertionError(f"loop plan text missing details: {loop_plan_text!r}")

    matrix_server.shutdown()
    matrix_server.server_close()

    print(json.dumps({
        "ok": True,
        "project": created["project"]["project_id"],
        "ready": ready,
        "loopReady": loop_ready,
        "publishedProjectArtifact": project_artifact["filename"],
        "planPath": str(plan_path),
    }, ensure_ascii=False))
  PY

  env = {"PATH" => "#{bin_dir}:#{ENV.fetch("PATH", "")}"}
  stdout, stderr, status = Open3.capture3(env, "python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness projectflow MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  result = JSON.parse(stdout)
  commands = log_path.read.lines.map(&:strip)
  fail!("create_quick_project did not push task dir: #{commands.inspect}") unless commands.include?(
    "mirror #{workspace}/shared/tasks/write-readiness-note-20260605-112233-01/ mock/shared/tasks/write-readiness-note-20260605-112233-01/ --overwrite"
  )

  puts JSON.pretty_generate(result.merge("mcCommands" => commands))
end
