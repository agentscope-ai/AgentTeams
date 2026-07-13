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

Dir.mktmpdir("teamharness-taskflow-") do |dir|
  root = Pathname.new(dir)
  workspace = root / "workspace"
  remote_task = root / "remote" / "tasks" / "remote-001"
  bin_dir = root / "bin"
  log_path = root / "mc.log"
  remote_task.mkpath
  (remote_task / "meta.json").write(JSON.pretty_generate(
    "taskId" => "remote-001",
    "projectId" => project_id = "remote-project",
    "roomId" => "room:!team:example.test",
    "status" => "assigned",
    "specPath" => "shared/tasks/remote-001/spec.md",
    "taskTitle" => "Remote task",
    "assignedTo" => "@worker-remote:example.test",
    "createdAt" => "2026-06-26T07:00:00Z"
  ))
  (remote_task / "spec.md").write("Remote task spec\n")
  bin_dir.mkpath
  (bin_dir / "mc").write(<<~SH)
    #!/usr/bin/env bash
    printf '%s\\n' "$*" >> "#{log_path}"
    if [ "$1" = "mirror" ] && [ "$2" = "mock/shared/tasks/remote-001/" ]; then
      mkdir -p "$3"
      cp -a "#{remote_task}/." "$3"
    fi
  SH
  (bin_dir / "mc").chmod(0o755)

  python_test = <<~PY
    import builtins
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
    from server import call_tool

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
            payload = {"event_id": f"$file-event-{len(matrix['events'])}"}
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

    def payload(name, args):
        merged = dict(common)
        merged.update(args)
        result = call_tool(name, merged)
        return json.loads(result["content"][0]["text"])

    project_id = "daily-plan-2026-06-03"
    task_id = "t-001"

    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": project_id,
            "title": "Daily Plan",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!team:example.test",
            },
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": project_id,
            "tasks": [{
                "taskId": task_id,
                "title": "Collect input",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })

    delegated = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "roomId": "room:!team:example.test",
            "spec": "Collect input and submit a result.",
        },
    })
    if not delegated.get("ok") or delegated["task"]["status"] != "assigned":
        raise AssertionError(f"delegate_task failed: {delegated!r}")
    if delegated.get("synced") is not True:
        raise AssertionError(f"delegate_task did not sync task dir: {delegated!r}")
    delegated_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/tasks/{task_id}/meta.json").read_text(encoding="utf-8"))
    if delegated_meta.get("task_title") != "Collect input":
        raise AssertionError(f"delegate_task did not write console task_title: {delegated_meta!r}")
    if delegated_meta.get("assigned_to") != "@worker-a:example.test":
        raise AssertionError(f"delegate_task did not write console assigned_to: {delegated_meta!r}")
    assigned_at = delegated_meta.get("assigned_at")
    if not assigned_at:
        raise AssertionError(f"delegate_task did not write console assigned_at: {delegated_meta!r}")

    external_project_id = "external-dingtalk-project"
    external_task_id = "external-dingtalk-project-01"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": external_project_id,
            "title": "External DingTalk Project",
            "source": "dingtalk",
            "requester": "dingtalk:sender_001:aaaaaaaa",
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": external_project_id,
            "tasks": [{
                "taskId": external_task_id,
                "title": "Do external work",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    real_import = builtins.__import__

    def block_yaml_import(name, *args, **kwargs):
        if name == "yaml":
            raise ImportError(name)
        return real_import(name, *args, **kwargs)

    builtins.__import__ = block_yaml_import
    try:
        blocked_team_room = payload("taskflow", {
            "role": "leader",
            "action": "delegate_task",
            "payload": {
                "projectId": external_project_id,
                "taskId": external_task_id,
                "roomId": "room:!team:example.test",
                "spec": "Should require a dedicated assignment room.",
            },
        })
    finally:
        builtins.__import__ = real_import
    if blocked_team_room.get("ok") or "dedicated task room" not in blocked_team_room.get("error", ""):
        raise AssertionError(f"external requester team-room delegation should fail: {blocked_team_room!r}")
    if (pathlib.Path("#{workspace}") / f"shared/tasks/{external_task_id}/spec.md").exists():
        raise AssertionError("failed external delegation should not write task spec")
    delegated_external = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": external_project_id,
            "taskId": external_task_id,
            "roomId": "room:!task-room:example.test",
            "spec": "Use the dedicated task room.",
        },
    })
    if not delegated_external.get("ok") or delegated_external["task"].get("room_id") != "room:!task-room:example.test":
        raise AssertionError(f"external requester dedicated room delegation failed: {delegated_external!r}")
    retried_external = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": external_project_id,
            "taskId": external_task_id,
            "roomId": "room:!task-room:example.test",
            "spec": "Retry in the same dedicated task room.",
        },
    })
    if not retried_external.get("ok"):
        raise AssertionError(f"same-room external redelegation should be idempotent: {retried_external!r}")
    blocked_other_assignment_room = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": external_project_id,
            "taskId": external_task_id,
            "roomId": "room:!other-task-room:example.test",
            "spec": "This should not move the task to another assignment room.",
        },
    })
    if blocked_other_assignment_room.get("ok") or "already delegated to assignment room" not in blocked_other_assignment_room.get("error", ""):
        raise AssertionError(f"external task should not be delegated to another assignment room: {blocked_other_assignment_room!r}")

    acked = payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": task_id},
    })
    if not acked.get("ok") or acked["task"]["status"] != "in_progress":
        raise AssertionError(f"ack_task failed: {acked!r}")
    if acked["task"].get("assigned_at") != assigned_at:
        raise AssertionError(f"ack_task should preserve assigned_at: {acked!r}")

    analysis_path = pathlib.Path("#{workspace}") / "shared/tasks/t-001/workspace/analysis.md"
    analysis_path.parent.mkdir(parents=True, exist_ok=True)
    analysis_path.write_text("analysis artifact\\n", encoding="utf-8")
    detailed_result_path = pathlib.Path("#{workspace}") / "shared/tasks/t-001/result.md"
    detailed_result_path.write_text(
        "# Detailed Result Body\\n\\n"
        "This detailed task result must survive submit_task.\\n\\n"
        "- preserved worker bullet\\n",
        encoding="utf-8",
    )

    invalid_project_deliverable = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {
            "taskId": task_id,
            "status": "SUCCESS",
            "summary": "Project paths are not task deliverables.",
            "deliverables": [
                "shared/projects/project-001/result.md",
            ],
        },
    })
    if invalid_project_deliverable.get("ok"):
        raise AssertionError(f"submit_task should reject project-level deliverables: {invalid_project_deliverable!r}")
    if "shared/tasks/t-001" not in str(invalid_project_deliverable.get("error", "")):
        raise AssertionError(f"project deliverable error should name task boundary: {invalid_project_deliverable!r}")

    submitted = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {
            "taskId": task_id,
            "status": "SUCCESS",
            "summary": "Input collected.",
            "parentEventId": "$task-parent",
            "deliverables": [
                "shared/tasks/t-001/result.md",
                "shared/tasks/t-001/workspace/analysis.md",
            ],
        },
    })
    if not submitted.get("ok") or submitted["task"]["status"] != "submitted":
        raise AssertionError(f"submit_task failed: {submitted!r}")
    if submitted.get("synced") is not True:
        raise AssertionError(f"submit_task did not sync result: {submitted!r}")
    if submitted["task"].get("assigned_at") != assigned_at:
        raise AssertionError(f"submit_task should preserve assigned_at: {submitted!r}")
    submitted_result_text = detailed_result_path.read_text(encoding="utf-8")
    expected_result_text = (
        "# Detailed Result Body\\n\\n"
        "This detailed task result must survive submit_task.\\n\\n"
        "- preserved worker bullet\\n"
    )
    if submitted_result_text != expected_result_text:
        raise AssertionError(f"submit_task should not rewrite agent-owned result.md: {submitted_result_text!r}")
    published = submitted.get("publishedArtifacts") or []
    published_by_source = {item.get("sourcePath"): item for item in published}
    for source, filename in {
        "shared/tasks/t-001/result.md": "t-001-result.md",
        "shared/tasks/t-001/workspace/analysis.md": "t-001-analysis.md",
    }.items():
        artifact = published_by_source.get(source)
        if not artifact or artifact.get("status") != "published":
            raise AssertionError(f"artifact was not published: {source} -> {published!r}")
        if artifact.get("filename") != filename:
            raise AssertionError(f"artifact filename mismatch: {artifact!r}")
        if not artifact.get("mxcUri") or not artifact.get("eventId"):
            raise AssertionError(f"artifact missing Matrix references: {artifact!r}")
        if artifact.get("parentEventId") != "$task-parent":
            raise AssertionError(f"artifact missing parent event reference: {artifact!r}")
    file_events = [event["content"] for event in matrix["events"]]
    if [event.get("body") for event in file_events[:2]] != ["t-001-result.md", "t-001-analysis.md"]:
        raise AssertionError(f"m.file event bodies mismatch: {file_events!r}")
    for event in file_events[:2]:
        if event.get("msgtype") != "m.file" or not event.get("url"):
            raise AssertionError(f"Matrix event is not a file event: {event!r}")
        if event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$task-parent"}:
            raise AssertionError(f"Matrix file event missing attachment relation: {event!r}")
        info = event.get("info") or {}
        if not info.get("size") or not info.get("mimetype"):
            raise AssertionError(f"Matrix file event missing info: {event!r}")
    if not all(upload.get("auth") == "Bearer test-token" for upload in matrix["uploads"][:2]):
        raise AssertionError(f"Matrix upload auth mismatch: {matrix['uploads']!r}")

    context_project_id = "context-parent-project"
    context_task_id = "context-parent-task"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": context_project_id,
            "title": "Context Parent Project",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!team:example.test",
            },
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": context_project_id,
            "tasks": [{
                "taskId": context_task_id,
                "title": "Submit with context parent",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": context_project_id,
            "taskId": context_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Submit without an explicit parentEventId.",
        },
    })
    payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": context_task_id},
    })
    context_result_path = pathlib.Path("#{workspace}") / "shared/tasks/context-parent-task/result.md"
    context_result_path.write_text("context result body\\n", encoding="utf-8")
    context_file.write_text(json.dumps({
        "rooms": {
            "!team:example.test": {
                "attachmentParentEventId": "$context-task-parent",
                "updatedAt": time.time(),
            }
        }
    }), encoding="utf-8")
    context_submitted = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {
            "taskId": context_task_id,
            "status": "SUCCESS",
            "summary": "Submitted using Matrix context parent.",
            "deliverables": [],
        },
    })
    context_published = context_submitted.get("publishedArtifacts") or []
    if len(context_published) != 1 or context_published[0].get("status") != "published":
        raise AssertionError(f"context submit_task should publish result artifact: {context_submitted!r}")
    if context_published[0].get("parentEventId") != "$context-task-parent":
        raise AssertionError(f"context submit_task did not infer parent event: {context_submitted!r}")
    context_event = matrix["events"][-1]["content"]
    if context_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$context-task-parent"}:
        raise AssertionError(f"context submit_task file event missing attachment relation: {context_event!r}")

    secret_task_id = "secret-artifact-01"
    payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": project_id,
            "taskId": secret_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Submit a result with one sensitive deliverable.",
        },
    })
    payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": secret_task_id},
    })
    secret_path = pathlib.Path("#{workspace}") / "shared/tasks/secret-artifact-01/workspace/token-report.md"
    secret_path.parent.mkdir(parents=True, exist_ok=True)
    secret_path.write_text("token=abcdefghijklmnopqrstuvwxyz1234567890\\n", encoding="utf-8")
    secret_submitted = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {
            "taskId": secret_task_id,
            "status": "SUCCESS",
            "summary": "Sensitive deliverable should be rejected for publish.",
            "deliverables": ["shared/tasks/secret-artifact-01/workspace/token-report.md"],
        },
    })
    if not secret_submitted.get("ok") or secret_submitted["task"]["status"] != "submitted":
        raise AssertionError(f"sensitive submit should still succeed: {secret_submitted!r}")
    secret_artifacts = {
        item.get("sourcePath"): item for item in (secret_submitted.get("publishedArtifacts") or [])
    }
    secret_artifact = secret_artifacts.get("shared/tasks/secret-artifact-01/workspace/token-report.md")
    if not secret_artifact or secret_artifact.get("status") != "failed":
        raise AssertionError(f"sensitive deliverable publish should fail explicitly: {secret_submitted!r}")
    if "sensitive" not in str(secret_artifact.get("error", "")).lower():
        raise AssertionError(f"sensitive publish error should be clear and sanitized: {secret_artifact!r}")
    if any(upload.get("filename") == "secret-artifact-01-token-report.md" for upload in matrix["uploads"]):
        raise AssertionError(f"sensitive deliverable should not be uploaded: {matrix['uploads']!r}")
    if any("abcdefghijklmnopqrstuvwxyz1234567890" in upload.get("body", "") for upload in matrix["uploads"]):
        raise AssertionError("sensitive value leaked into Matrix upload")

    checked = payload("taskflow", {
        "role": "leader",
        "action": "check_task",
        "payload": {"taskId": task_id},
    })
    if not checked.get("ok") or not checked.get("effective"):
        raise AssertionError(f"check_task failed: {checked!r}")
    if checked.get("result", {}).get("summary") != "Input collected.":
        raise AssertionError(f"check_task did not return result summary: {checked!r}")
    expected_deliverables = [
        "shared/tasks/t-001/result.md",
        "shared/tasks/t-001/workspace/analysis.md",
    ]
    if checked.get("result", {}).get("deliverables") != expected_deliverables:
        raise AssertionError(f"check_task deliverables should ignore result body bullets: {checked!r}")

    accepted = payload("projectflow", {
        "action": "accept_task_result",
        "payload": {
            "projectId": project_id,
            "taskId": task_id,
            "resultStatus": checked["result"]["status"],
            "summary": checked["result"]["summary"],
        },
    })
    if not accepted.get("ok"):
        raise AssertionError(f"accept_task_result failed: {accepted!r}")
    accepted_tasks = {task["task_id"]: task for task in accepted["project"].get("tasks", [])}
    if accepted_tasks.get(task_id, {}).get("status") != "completed":
        raise AssertionError(f"accept_task_result did not complete project node: {accepted!r}")
    requester_report = accepted["project"].get("requester_report", {})
    if requester_report.get("pending") is not True or requester_report.get("task_id") != task_id:
        raise AssertionError(f"accept_task_result did not mark requester report pending: {accepted!r}")
    accepted_notification = accepted.get("notificationNeeded", {})
    if accepted_notification.get("replyRoute", {}).get("target_session") != "!team:example.test":
        raise AssertionError(f"accepted result should trigger requester report notification: {accepted!r}")
    marked_report = payload("projectflow", {
        "action": "mark_requester_report_sent",
        "payload": {"projectId": project_id},
    })
    if not marked_report.get("ok"):
        raise AssertionError(f"mark_requester_report_sent failed: {marked_report!r}")
    if marked_report["project"].get("requester_report", {}).get("pending") is not False:
        raise AssertionError(f"mark_requester_report_sent did not clear pending flag: {marked_report!r}")

    cancellation_project_id = "superseded-project"
    old_task_id = "superseded-project-old"
    replacement_task_id = "superseded-project-new"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": cancellation_project_id,
            "title": "Superseded Project",
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": cancellation_project_id,
            "tasks": [{
                "taskId": old_task_id,
                "title": "Old active task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": cancellation_project_id,
            "taskId": old_task_id,
            "roomId": "room:!team:example.test",
            "spec": "This task will be superseded.",
        },
    })
    payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": old_task_id},
    })
    missing_reason_cancel = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"taskId": old_task_id},
    })
    if missing_reason_cancel.get("ok") or "reason is required" not in missing_reason_cancel.get("error", ""):
        raise AssertionError(f"missing cancel reason should be rejected: {missing_reason_cancel!r}")
    blank_reason_cancel = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"taskId": old_task_id, "reason": "  "},
    })
    if blank_reason_cancel.get("ok") or "reason is required" not in blank_reason_cancel.get("error", ""):
        raise AssertionError(f"blank cancel reason should be rejected: {blank_reason_cancel!r}")
    cancelled = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {
            "taskId": old_task_id,
            "reason": "superseded",
            "replacementTaskId": replacement_task_id,
        },
    })
    if not cancelled.get("ok") or cancelled["task"].get("status") != "cancelled":
        raise AssertionError(f"cancel_task failed: {cancelled!r}")
    if cancelled.get("synced") is not True:
        raise AssertionError(f"cancel_task did not sync task state: {cancelled!r}")
    if cancelled["task"].get("cancel_reason") != "superseded":
        raise AssertionError(f"cancel_task did not record reason: {cancelled!r}")
    if cancelled["task"].get("replacement_task_id") != replacement_task_id:
        raise AssertionError(f"cancel_task did not record replacement: {cancelled!r}")
    cancelled_nodes = {task["task_id"]: task for task in cancelled["project"].get("tasks", [])}
    if cancelled_nodes.get(old_task_id, {}).get("status") != "cancelled":
        raise AssertionError(f"cancel_task did not update project node: {cancelled!r}")
    old_spec_path = pathlib.Path("#{workspace}") / f"shared/tasks/{old_task_id}/spec.md"
    if not old_spec_path.exists():
        raise AssertionError("cancel_task should not delete historical task files")
    old_spec = old_spec_path.read_text(encoding="utf-8")
    late_delegate = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": cancellation_project_id,
            "taskId": old_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Late delegate should not overwrite a cancelled task.",
        },
    })
    if late_delegate.get("ok") or "terminal task: cancelled" not in late_delegate.get("error", ""):
        raise AssertionError(f"cancelled task should reject late delegate_task: {late_delegate!r}")
    if old_spec_path.read_text(encoding="utf-8") != old_spec:
        raise AssertionError("late delegate_task should not overwrite spec.md for a cancelled task")
    late_ack = payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": old_task_id},
    })
    if late_ack.get("ok") or "terminal task: cancelled" not in late_ack.get("error", ""):
        raise AssertionError(f"cancelled task should reject late ack_task: {late_ack!r}")
    late_submit = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {"taskId": old_task_id, "summary": "late result", "deliverables": []},
    })
    if late_submit.get("ok") or "terminal task: cancelled" not in late_submit.get("error", ""):
        raise AssertionError(f"cancelled task should reject late submit_task: {late_submit!r}")
    old_meta_path = pathlib.Path("#{workspace}") / f"shared/tasks/{old_task_id}/meta.json"
    old_meta = json.loads(old_meta_path.read_text(encoding="utf-8"))
    if old_meta.get("status") != "cancelled":
        raise AssertionError(f"late worker action revived cancelled task meta: {old_meta!r}")
    if old_meta.get("cancel_reason") != "superseded":
        raise AssertionError(f"late task action lost cancel reason: {old_meta!r}")
    old_result_path = pathlib.Path("#{workspace}") / f"shared/tasks/{old_task_id}/result.md"
    if old_result_path.exists():
        raise AssertionError("late submit_task should not write result.md for a cancelled task")
    cancelled_project_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/projects/{cancellation_project_id}/meta.json").read_text(encoding="utf-8"))
    cancelled_project_nodes = {task["task_id"]: task for task in cancelled_project_meta.get("tasks", [])}
    if cancelled_project_nodes.get(old_task_id, {}).get("status") != "cancelled":
        raise AssertionError(f"late worker action revived cancelled project node: {cancelled_project_meta!r}")

    dag_loop_project_id = "dag-loop-reuse-project"
    dag_loop_task_id = "dag-loop-shared-task"
    payload("projectflow", {
        "action": "create_project",
        "payload": {"projectId": dag_loop_project_id, "title": "DAG Loop Reuse Project"},
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": dag_loop_project_id,
            "tasks": [{
                "taskId": dag_loop_task_id,
                "title": "Top-level stale task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    payload("projectflow", {
        "action": "plan_loop",
        "payload": {
            "projectId": dag_loop_project_id,
            "goal": "Repeat until done",
            "stopCondition": "Done",
            "iterationTemplate": "One iteration",
            "maxIterations": 2,
            "tasks": [{
                "taskId": dag_loop_task_id,
                "title": "Active loop task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    dag_loop_meta_path = pathlib.Path("#{workspace}") / f"shared/projects/{dag_loop_project_id}/meta.json"
    dag_loop_project = json.loads(dag_loop_meta_path.read_text(encoding="utf-8"))
    dag_loop_project["loop"]["tasks"][0]["status"] = "cancelled"
    dag_loop_meta_path.write_text(json.dumps(dag_loop_project, ensure_ascii=False, indent=2) + "\\n", encoding="utf-8")
    dag_loop_task_meta_path = pathlib.Path("#{workspace}") / f"shared/tasks/{dag_loop_task_id}/meta.json"
    if dag_loop_task_meta_path.exists():
        raise AssertionError("DAG-to-loop guard setup should not create task meta")
    dag_loop_delegate = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": dag_loop_project_id,
            "taskId": dag_loop_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Late delegate should not revive the loop task.",
        },
    })
    if dag_loop_delegate.get("ok") or "terminal task: cancelled" not in dag_loop_delegate.get("error", ""):
        raise AssertionError(f"DAG-to-loop terminal task should reject delegate_task: {dag_loop_delegate!r}")
    dag_loop_after = json.loads(dag_loop_meta_path.read_text(encoding="utf-8"))
    dag_loop_top_nodes = {task["task_id"]: task for task in dag_loop_after.get("tasks", [])}
    dag_loop_loop_nodes = {task["task_id"]: task for task in dag_loop_after.get("loop", {}).get("tasks", [])}
    if dag_loop_top_nodes.get(dag_loop_task_id, {}).get("status") != "planned":
        raise AssertionError(f"failed delegate_task rewrote stale DAG node: {dag_loop_after!r}")
    if dag_loop_loop_nodes.get(dag_loop_task_id, {}).get("status") != "cancelled":
        raise AssertionError(f"failed delegate_task revived cancelled loop node: {dag_loop_after!r}")
    if (pathlib.Path("#{workspace}") / f"shared/tasks/{dag_loop_task_id}/spec.md").exists():
        raise AssertionError("failed DAG-to-loop delegate_task should not write spec.md")

    worker_cancel = payload("taskflow", {
        "role": "worker",
        "action": "cancel_task",
        "payload": {"taskId": old_task_id, "reason": "manual_replan"},
    })
    if worker_cancel.get("ok") or "leader role" not in worker_cancel.get("error", ""):
        raise AssertionError(f"worker role should not cancel tasks: {worker_cancel!r}")

    missing_cancel = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"taskId": "missing-task", "reason": "manual_replan"},
    })
    if missing_cancel.get("ok") or "task not found" not in missing_cancel.get("error", ""):
        raise AssertionError(f"missing task should return explicit error: {missing_cancel!r}")

    completed_cancel = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"taskId": task_id, "reason": "manual_replan"},
    })
    if completed_cancel.get("ok") or "cannot cancel terminal task" not in completed_cancel.get("error", ""):
        raise AssertionError(f"completed task should not be silently cancelled: {completed_cancel!r}")

    terminal_project_id = "terminal-cancel-project"
    terminal_tasks = [
        ("terminal-revision", "Revision terminal"),
        ("terminal-blocked", "Blocked terminal"),
        ("terminal-cancelled", "Cancelled terminal"),
    ]
    payload("projectflow", {
        "action": "create_project",
        "payload": {"projectId": terminal_project_id, "title": "Terminal Cancel Project"},
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": terminal_project_id,
            "tasks": [
                {
                    "taskId": task_id,
                    "title": title,
                    "assignedTo": "@worker-a:example.test",
                    "dependsOn": [],
                }
                for task_id, title in terminal_tasks
            ],
        },
    })
    for terminal_task_id, _title in terminal_tasks:
        payload("taskflow", {
            "role": "leader",
            "action": "delegate_task",
            "payload": {
                "projectId": terminal_project_id,
                "taskId": terminal_task_id,
                "roomId": "room:!team:example.test",
                "spec": f"Prepare {terminal_task_id}.",
            },
        })
    payload("projectflow", {
        "action": "accept_task_result",
        "payload": {
            "projectId": terminal_project_id,
            "taskId": "terminal-revision",
            "accepted": False,
            "resultStatus": "SUCCESS",
            "summary": "Needs revision.",
        },
    })
    payload("projectflow", {
        "action": "accept_task_result",
        "payload": {
            "projectId": terminal_project_id,
            "taskId": "terminal-blocked",
            "resultStatus": "BLOCKED",
            "summary": "Blocked.",
        },
    })
    payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"taskId": "terminal-cancelled", "reason": "manual_replan"},
    })
    for terminal_task_id in ["terminal-revision", "terminal-blocked", "terminal-cancelled"]:
        terminal_cancel = payload("taskflow", {
            "role": "leader",
            "action": "cancel_task",
            "payload": {"taskId": terminal_task_id, "reason": "manual_replan"},
        })
        if terminal_cancel.get("ok") or "cannot cancel terminal task" not in terminal_cancel.get("error", ""):
            raise AssertionError(f"terminal task should not be silently cancelled: {terminal_task_id} {terminal_cancel!r}")

    replanned = payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": cancellation_project_id,
            "tasks": [{
                "taskId": replacement_task_id,
                "title": "Replacement task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    if not replanned.get("ok"):
        raise AssertionError(f"replan after cancel_task failed: {replanned!r}")
    replanned_task_ids = {task["task_id"] for task in replanned["project"].get("tasks", [])}
    if old_task_id in replanned_task_ids or replacement_task_id not in replanned_task_ids:
        raise AssertionError(f"replan did not replace old task node: {replanned!r}")
    old_resolved = payload("projectflow", {
        "action": "resolve_project",
        "payload": {"taskId": old_task_id},
    })
    if old_resolved.get("task", {}).get("status") != "cancelled":
        raise AssertionError(f"old task meta should remain cancelled after replan: {old_resolved!r}")
    delegated_replacement = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": cancellation_project_id,
            "taskId": replacement_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Replacement task can now proceed.",
        },
    })
    if not delegated_replacement.get("ok") or delegated_replacement["task"].get("status") != "assigned":
        raise AssertionError(f"replacement task delegation failed: {delegated_replacement!r}")

    revision_project_id = "revision-project"
    revision_task_id = "revision-task-01"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": revision_project_id,
            "title": "Revision Project",
            "replyRoute": {
                "channel": "matrix",
                "targetUser": "@admin:example.test",
                "targetSession": "!team:example.test",
            },
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": revision_project_id,
            "tasks": [{
                "taskId": revision_task_id,
                "title": "Draft result",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    rejected = payload("projectflow", {
        "action": "accept_task_result",
        "payload": {
            "projectId": revision_project_id,
            "taskId": revision_task_id,
            "accepted": False,
            "resultStatus": "SUCCESS",
            "summary": "Not good enough.",
        },
    })
    if not rejected.get("ok"):
        raise AssertionError(f"accepted=false conflict failed unexpectedly: {rejected!r}")
    rejected_tasks = {task["task_id"]: task for task in rejected["project"].get("tasks", [])}
    if rejected.get("accepted") is not False or rejected.get("nodeStatus") != "revision":
        raise AssertionError(f"accepted=false did not take precedence: {rejected!r}")
    if rejected_tasks.get(revision_task_id, {}).get("status") != "revision":
        raise AssertionError(f"accepted=false did not mark node revision: {rejected!r}")
    if rejected["project"].get("requester_report", {}).get("pending") is True:
        raise AssertionError(f"accepted=false should not create requester report: {rejected!r}")
    rejected_notification = rejected.get("notificationNeeded", {})
    if rejected_notification.get("replyRoute"):
        raise AssertionError(f"accepted=false should not trigger requester report notification: {rejected!r}")

    result_path_for_validation = pathlib.Path("#{workspace}") / "shared/tasks/t-001/result.md"
    original_result = result_path_for_validation.read_text(encoding="utf-8")
    result_path_for_validation.write_text("# Task Result\\n\\n- Summary: Missing status.\\n", encoding="utf-8")
    invalid_checked = payload("taskflow", {
        "role": "leader",
        "action": "check_task",
        "payload": {"taskId": task_id},
    })
    if not invalid_checked.get("ok") or not invalid_checked.get("effective"):
        raise AssertionError(f"result body should not override task meta validation: {invalid_checked!r}")
    if invalid_checked.get("validationErrors"):
        raise AssertionError(f"result body should not create validation errors: {invalid_checked!r}")
    result_path_for_validation.write_text(original_result, encoding="utf-8")

    forbidden_delegate = payload("taskflow", {
        "role": "worker",
        "action": "delegate_task",
        "payload": {
            "projectId": project_id,
            "taskId": "t-002",
            "roomId": "room:!team:example.test",
            "spec": "This should be rejected for worker role.",
        },
    })
    if forbidden_delegate.get("ok") or "leader role" not in forbidden_delegate.get("error", ""):
        raise AssertionError(f"worker role should not delegate: {forbidden_delegate!r}")

    forbidden_submit = payload("taskflow", {
        "role": "leader",
        "action": "submit_task",
        "payload": {
            "taskId": task_id,
            "status": "SUCCESS",
            "summary": "This should be rejected for leader role.",
            "deliverables": ["shared/tasks/t-001/result.md"],
        },
    })
    if forbidden_submit.get("ok") or "worker or remote-member role" not in forbidden_submit.get("error", ""):
        raise AssertionError(f"leader role should not submit: {forbidden_submit!r}")

    os.environ["AGENTTEAMS_WORKER_ROLE"] = "worker"
    remote_ack = payload("taskflow", {
        "action": "ack_task",
        "task_id": "remote-001",
    })
    if not remote_ack.get("ok") or remote_ack["task"]["status"] != "in_progress":
        raise AssertionError(f"ack_task did not infer role and pull remote task: {remote_ack!r}")
    if not (pathlib.Path("#{workspace}") / "shared/tasks/remote-001/spec.md").exists():
        raise AssertionError("ack_task did not pull remote task spec")
    remote_meta = json.loads((pathlib.Path("#{workspace}") / "shared/tasks/remote-001/meta.json").read_text(encoding="utf-8"))
    if remote_meta.get("task_title") != "Remote task":
        raise AssertionError(f"camelCase taskTitle should be converted to task_title: {remote_meta!r}")
    if remote_meta.get("assigned_to") != "@worker-remote:example.test":
        raise AssertionError(f"camelCase assignedTo should be converted to assigned_to: {remote_meta!r}")
    if remote_meta.get("assigned_at") != "2026-06-26T07:00:00Z":
        raise AssertionError(f"camelCase createdAt should be converted to assigned_at: {remote_meta!r}")
    if "taskId" in remote_meta or "taskTitle" in remote_meta or "assignedTo" in remote_meta:
        raise AssertionError(f"camelCase task keys should not be persisted: {remote_meta!r}")

    workspace = pathlib.Path("#{workspace}")
    spec_path = workspace / "shared/tasks/t-001/spec.md"
    result_path = workspace / "shared/tasks/t-001/result.md"
    meta_path = workspace / "shared/tasks/t-001/meta.json"
    legacy_state_path = workspace / "shared/tasks/t-001/task.json"
    if not spec_path.exists() or not result_path.exists() or not meta_path.exists():
        raise AssertionError("task files missing")
    if legacy_state_path.exists():
        raise AssertionError(f"task.json should not be written: {legacy_state_path}")
    final_meta = json.loads(meta_path.read_text(encoding="utf-8"))
    if final_meta.get("task_title") != "Collect input" or final_meta.get("assigned_to") != "@worker-a:example.test":
        raise AssertionError(f"final task meta missing console fields: {final_meta!r}")
    if final_meta.get("assigned_at") != assigned_at:
        raise AssertionError(f"final task meta should preserve assigned_at: {final_meta!r}")

    matrix_server.shutdown()
    matrix_server.server_close()

    print(json.dumps({
        "ok": True,
      "task": submitted["task"]["task_id"],
      "status": submitted["task"]["status"],
      "publishedArtifacts": [item["filename"] for item in published if item.get("status") == "published"],
      "remoteAck": remote_ack["task"]["task_id"],
      "specPath": str(spec_path),
      "resultPath": str(result_path),
    }, ensure_ascii=False))
  PY

  env = {"PATH" => "#{bin_dir}:#{ENV.fetch("PATH", "")}"}
  stdout, stderr, status = Open3.capture3(env, "python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!(["teamharness taskflow MCP test failed", stderr, stdout].reject(&:empty?).join("\n")) unless status.success?

  result = JSON.parse(stdout)
  commands = log_path.read.lines.map(&:strip)
  fail!("delegate_task did not push task dir: #{commands.inspect}") unless commands.include?(
    "mirror #{workspace}/shared/tasks/t-001/ mock/shared/tasks/t-001/ --overwrite"
  )
  fail!("submit_task did not push only worker-owned files: #{commands.inspect}") unless commands.include?(
    "mirror #{workspace}/shared/tasks/t-001/ mock/shared/tasks/t-001/ --overwrite --exclude spec.md --exclude base/"
  )
  fail!("ack_task did not pull remote task dir: #{commands.inspect}") unless commands.include?(
    "mirror mock/shared/tasks/remote-001/ #{workspace}/shared/tasks/remote-001 --overwrite"
  )

  puts JSON.pretty_generate(result.merge("mcCommands" => commands))
end
