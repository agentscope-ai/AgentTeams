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
    # Test hook: fail the push (mirror <local> <remote>) for the named
    # task only — pre-action pulls keep working (used to exercise the
    # submit sync-failure withholding path).
    if [ "$1" = "mirror" ] && [ -n "${TEAMHARNESS_TEST_FAIL_SYNC_TASK:-}" ] && [ "$3" = "mock/shared/tasks/${TEAMHARNESS_TEST_FAIL_SYNC_TASK}/" ]; then
      exit 1
    fi
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
        "team:\\n"
        "  teamRoomId: '!team:example.test'\\n"
        "  leaderRuntimeName: 'admin'\\n"
        "  members:\\n"
        "    - name: 'Admin'\\n"
        "      runtimeName: 'admin'\\n"
        "      role: 'team_leader'\\n"
        "      matrixUserId: '@admin:example.test'\\n"
        "    - name: 'Worker A'\\n"
        "      runtimeName: 'worker-a'\\n"
        "      role: 'worker'\\n"
        "      matrixUserId: '@worker-a:example.test'\\n",
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

        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path.endswith("/members"):
                members = [
                    {"state_key": "@worker-a:example.test", "content": {"membership": "join"}},
                    {"state_key": "@worker-remote:example.test", "content": {"membership": "join"}},
                    {"state_key": "@worker-invited:example.test", "content": {"membership": "invite"}},
                    {"state_key": "@admin:example.test", "content": {"membership": "join"}},
                ]
                if os.environ.get("TEAMHARNESS_TEST_EXCLUDE_LEADER_FROM_ROOM") == "1":
                    members = [m for m in members if m["state_key"] != "@admin:example.test"]
                payload = {"chunk": members}
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(payload).encode("utf-8"))
            else:
                self.send_response(404)
                self.end_headers()

        def do_PUT(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if "/send/m.room.message/" not in parsed.path:
                self.send_response(404)
                self.end_headers()
                return
            if "/send/m.room.message/delegate-" in parsed.path and os.environ.get("TEAMHARNESS_TEST_FAIL_NOTIFICATION") == "1":
                self.send_response(500)
                self.end_headers()
                self.wfile.write(json.dumps({"errcode": "M_UNKNOWN", "error": "forced Matrix failure"}).encode("utf-8"))
                return
            if "/send/m.room.message/submit-" in parsed.path and os.environ.get("TEAMHARNESS_TEST_FAIL_SUBMIT_NOTIFICATION") == "1":
                self.send_response(500)
                self.end_headers()
                self.wfile.write(json.dumps({"errcode": "M_UNKNOWN", "error": "forced Matrix failure"}).encode("utf-8"))
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
    reacked = payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": task_id},
    })
    if not reacked.get("ok") or reacked["task"].get("status") != "submitted":
        raise AssertionError(f"ack_task should not regress a submitted task: {reacked!r}")
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
    file_events = [event["content"] for event in matrix["events"] if event["content"].get("msgtype") == "m.file"]
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

    def completion_events():
        return [
            event for event in matrix["events"]
            if "/send/m.room.message/submit-t-001" in event["path"]
        ]

    first_completion = completion_events()
    if len(first_completion) != 1:
        raise AssertionError(f"submit_task should send exactly one completion notification: {matrix['events']!r}")
    completion_body = first_completion[0]["content"].get("body", "")
    if "@admin:example.test" not in (first_completion[0]["content"].get("m.mentions") or {}).get("user_ids", []):
        raise AssertionError(f"completion notification must mention the leader: {first_completion[0]['content']!r}")
    if "TASK_COMPLETED: t-001 - Result: shared/tasks/t-001/result.md" not in completion_body:
        raise AssertionError(f"completion notification must carry the contract line: {completion_body!r}")
    if "- Worker: @worker-a:example.test" not in completion_body:
        raise AssertionError(f"completion notification must carry the executor: {completion_body!r}")
    if "Input collected." not in completion_body:
        raise AssertionError(f"completion notification must carry the summary: {completion_body!r}")
    if first_completion[0]["auth"] != "Bearer test-token":
        raise AssertionError(f"completion notification auth mismatch: {first_completion[0]['auth']!r}")
    submitted_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/tasks/{task_id}/meta.json").read_text(encoding="utf-8"))
    if not submitted_meta.get("completionEventId"):
        raise AssertionError(f"submit_task did not persist completionEventId: {submitted_meta!r}")
    if submitted_meta.get("completionEventId") != submitted.get("notification", {}).get("eventId"):
        raise AssertionError(f"persisted completionEventId mismatch: {submitted_meta!r} vs {submitted.get('notification')!r}")

    resubmitted = payload("taskflow", {
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
    if not resubmitted.get("ok") or resubmitted["task"]["status"] != "submitted":
        raise AssertionError(f"resubmit_task failed: {resubmitted!r}")
    resubmit_notification = resubmitted.get("notification") or {}
    if resubmit_notification.get("reused") is not True:
        raise AssertionError(f"resubmit should reuse the recorded completion notification: {resubmit_notification!r}")
    if resubmit_notification.get("eventId") != submitted.get("notification", {}).get("eventId"):
        raise AssertionError(f"resubmit notification event id mismatch: {resubmit_notification!r}")
    if len(completion_events()) != 1:
        raise AssertionError(f"resubmit must not duplicate the completion notification: {completion_events()!r}")

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
    context_file_event = next(
        (
            event["content"]
            for event in reversed(matrix["events"])
            if event["content"].get("msgtype") == "m.file"
            and event["content"].get("url") == context_published[0].get("mxcUri")
        ),
        None,
    )
    if context_file_event is None:
        raise AssertionError(f"context submit_task file event not found: {context_published!r}")
    if context_file_event.get("m.relates_to") != {"rel_type": "com.agentteams.attachment", "event_id": "$context-task-parent"}:
        raise AssertionError(f"context submit_task file event missing attachment relation: {context_file_event!r}")

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

    # --- Completion notification: BLOCKED status carries the BLOCKED contract line. ---
    blocked_project_id = "blocked-project"
    blocked_task_id = "blocked-task"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": blocked_project_id,
            "title": "Blocked Project",
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
            "projectId": blocked_project_id,
            "tasks": [{
                "taskId": blocked_task_id,
                "title": "Blocked task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": blocked_project_id,
            "taskId": blocked_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Will be blocked.",
        },
    })
    blocked_submitted = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {
            "taskId": blocked_task_id,
            "status": "BLOCKED",
            "summary": "GPU OOM on node 2, needs 24G context.",
        },
    })
    if not blocked_submitted.get("ok") or blocked_submitted["task"]["status"] != "submitted":
        raise AssertionError(f"blocked submit_task failed: {blocked_submitted!r}")
    blocked_events = [
        event for event in matrix["events"]
        if "/send/m.room.message/submit-blocked-task" in event["path"]
    ]
    if len(blocked_events) != 1:
        raise AssertionError(f"blocked submit should send one completion notification: {matrix['events']!r}")
    blocked_body = blocked_events[0]["content"].get("body", "")
    if "BLOCKED: blocked-task - GPU OOM on node 2, needs 24G context." not in blocked_body:
        raise AssertionError(f"blocked notification must carry the BLOCKED contract line: {blocked_body!r}")
    if "TASK_COMPLETED" in blocked_body:
        raise AssertionError(f"blocked notification must not claim completion: {blocked_body!r}")

    # --- Failure injection: a completion send failure must NOT block the
    #     terminal submission (best-effort by contract). ---
    fail_submit_project_id = "fail-submit-project"
    fail_submit_task_id = "fail-submit-task"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": fail_submit_project_id,
            "title": "Fail Submit Project",
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
            "projectId": fail_submit_project_id,
            "tasks": [{
                "taskId": fail_submit_task_id,
                "title": "Fail submit task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": fail_submit_project_id,
            "taskId": fail_submit_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Matrix is down during submit.",
        },
    })
    os.environ["TEAMHARNESS_TEST_FAIL_SUBMIT_NOTIFICATION"] = "1"
    try:
        fail_submit_result = payload("taskflow", {
            "role": "worker",
            "action": "submit_task",
            "payload": {
                "taskId": fail_submit_task_id,
                "status": "SUCCESS",
                "summary": "Result ready but Matrix is down.",
            },
        })
    finally:
        os.environ.pop("TEAMHARNESS_TEST_FAIL_SUBMIT_NOTIFICATION", None)
    if not fail_submit_result.get("ok") or fail_submit_result["task"]["status"] != "submitted":
        raise AssertionError(f"completion notification failure must not block submission: {fail_submit_result!r}")
    fail_submit_notification = fail_submit_result.get("notification") or {}
    if fail_submit_notification.get("sent") is not False:
        raise AssertionError(f"failed completion send must report sent=False: {fail_submit_notification!r}")
    if "HTTP 500" not in str(fail_submit_notification.get("error", "")):
        raise AssertionError(f"failed completion send must report the Matrix error: {fail_submit_notification!r}")
    fail_submit_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/tasks/{fail_submit_task_id}/meta.json").read_text(encoding="utf-8"))
    if fail_submit_meta.get("completionEventId"):
        raise AssertionError(f"failed completion send must not persist completionEventId: {fail_submit_meta!r}")

    # --- Failure injection: a notification send failure must return a
    #     retryable error and must NOT leave the task assigned. ---
    fail_project_id = "fail-inject-project"
    fail_task_id = "fail-inject-task"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": fail_project_id,
            "title": "Failure Injection Project",
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": fail_project_id,
            "tasks": [{
                "taskId": fail_task_id,
                "title": "Will fail to notify",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }],
        },
    })
    os.environ["TEAMHARNESS_TEST_FAIL_NOTIFICATION"] = "1"
    try:
        failed_delegate = payload("taskflow", {
            "role": "leader",
            "action": "delegate_task",
            "payload": {
                "projectId": fail_project_id,
                "taskId": fail_task_id,
                "roomId": "room:!team:example.test",
                "spec": "Notification will be forced to fail.",
            },
        })
    finally:
        os.environ.pop("TEAMHARNESS_TEST_FAIL_NOTIFICATION", None)
    if failed_delegate.get("ok") or not failed_delegate.get("retryable"):
        raise AssertionError(f"notification failure should return a retryable error: {failed_delegate!r}")
    if failed_delegate["task"].get("status") != "prepared":
        raise AssertionError(f"failed delegation must not leave task assigned: {failed_delegate!r}")
    if failed_delegate.get("notification", {}).get("eventId"):
        raise AssertionError(f"failed delegation must not record an event_id: {failed_delegate!r}")
    fail_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/tasks/{fail_task_id}/meta.json").read_text(encoding="utf-8"))
    if fail_meta.get("status") != "prepared":
        raise AssertionError(f"failed delegation must persist prepared status, not assigned: {fail_meta!r}")
    fail_project_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/projects/{fail_project_id}/meta.json").read_text(encoding="utf-8"))
    fail_project_node = next((t for t in fail_project_meta.get("tasks", []) if t.get("task_id") == fail_task_id), None)
    if fail_project_node is None or fail_project_node.get("status") == "assigned":
        raise AssertionError(f"failed delegation must not assign the project node: {fail_project_meta!r}")

    # Retry after the failure is resolved: the stable txn id makes the send
    # idempotent; the task becomes assigned with an event_id.
    retried_delegate = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": fail_project_id,
            "taskId": fail_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Notification will be forced to fail.",
        },
    })
    if not retried_delegate.get("ok") or retried_delegate["task"].get("status") != "assigned":
        raise AssertionError(f"retry after notification failure should assign the task: {retried_delegate!r}")
    if not retried_delegate.get("notification", {}).get("eventId"):
        raise AssertionError(f"retry should record the event_id: {retried_delegate!r}")
    retried_meta = json.loads((pathlib.Path("#{workspace}") / f"shared/tasks/{fail_task_id}/meta.json").read_text(encoding="utf-8"))
    if retried_meta.get("status") != "assigned":
        raise AssertionError(f"retry must persist assigned status: {retried_meta!r}")

    # Re-delegating an already assigned task is idempotent and must NOT send
    # a second notification.
    before_events = len(matrix["events"])
    reused_delegate = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": fail_project_id,
            "taskId": fail_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Idempotent re-delegation.",
        },
    })
    if not reused_delegate.get("ok") or not reused_delegate.get("notification", {}).get("reused"):
        raise AssertionError(f"re-delegation of an assigned task should be idempotent: {reused_delegate!r}")
    after_events = len(matrix["events"])
    if after_events != before_events:
        raise AssertionError("idempotent re-delegation must not send another notification")

    # --- Room membership must be strictly "join": delegating to an
    #     invited-but-not-joined Worker returns a retryable error, sends no
    #     notification, and never assigns the task.
    invite_project_id = "invite-project"
    invite_task_id = "invite-task"
    payload("projectflow", {
        "action": "create_project",
        "payload": {
            "projectId": invite_project_id,
            "title": "Invited Only Project",
        },
    })
    payload("projectflow", {
        "action": "plan_dag",
        "payload": {
            "projectId": invite_project_id,
            "tasks": [{
                "taskId": invite_task_id,
                "title": "Invited but not joined",
                "assignedTo": "@worker-invited:example.test",
                "dependsOn": [],
            }],
        },
    })
    before_invite_events = len(matrix["events"])
    invited_delegate = payload("taskflow", {
        "role": "leader",
        "action": "delegate_task",
        "payload": {
            "projectId": invite_project_id,
            "taskId": invite_task_id,
            "roomId": "room:!team:example.test",
            "spec": "Invited worker cannot be delegated.",
        },
    })
    if invited_delegate.get("ok") or not invited_delegate.get("retryable"):
        raise AssertionError(f"delegation to an invited-but-not-joined Worker should be retryable: {invited_delegate!r}")
    if "not a joined member" not in invited_delegate.get("error", ""):
        raise AssertionError(f"membership error should mention the join requirement: {invited_delegate!r}")
    after_invite_events = len(matrix["events"])
    if after_invite_events != before_invite_events:
        raise AssertionError("delegation to an invited Worker must not send a notification")
    invite_meta_path = pathlib.Path("#{workspace}") / f"shared/tasks/{invite_task_id}/meta.json"
    if invite_meta_path.exists():
        invite_meta = json.loads(invite_meta_path.read_text(encoding="utf-8"))
        if invite_meta.get("status") == "assigned":
            raise AssertionError(f"delegation to an invited Worker must not assign the task: {invite_meta!r}")

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

    # ==================================================================
    # PR review 2026-09-05 (issue #1229): lifecycle attention events
    # ==================================================================

    # A human member (task initiator) joins the roster so @initiator
    # routing can be asserted.
    runtime_cfg = pathlib.Path("#{root}") / "runtime.yaml"
    runtime_cfg.write_text(
        runtime_cfg.read_text(encoding="utf-8").rstrip()
        + "\\n    - name: 'Luo'\\n"
        "      runtimeName: 'luo'\\n"
        "      role: 'human'\\n"
        "      matrixUserId: '@luo:example.test'\\n",
        encoding="utf-8",
    )

    def _lifecycle_setup(tid):
        pid = f"attn-{tid}"
        payload("projectflow", {
            "action": "create_project",
            "payload": {"projectId": pid, "title": "Attention fixture"},
        })
        payload("projectflow", {
            "action": "plan_dag",
            "payload": {"projectId": pid, "tasks": [{
                "taskId": tid,
                "title": "Attention task",
                "assignedTo": "@worker-a:example.test",
                "dependsOn": [],
            }]},
        })
        delegated = payload("taskflow", {
            "role": "leader",
            "action": "delegate_task",
            "payload": {
                "projectId": pid,
                "taskId": tid,
                "roomId": "room:!team:example.test",
                "spec": "Attention spec.",
            },
        })
        if not delegated.get("ok"):
            raise AssertionError(f"delegate_task failed for {tid}: {delegated!r}")
        acked = payload("taskflow", {
            "role": "worker",
            "action": "ack_task",
            "payload": {"taskId": tid},
        })
        if not acked.get("ok"):
            raise AssertionError(f"ack_task failed for {tid}: {acked!r}")
        tdir = pathlib.Path("#{workspace}") / f"shared/tasks/{tid}"
        tdir.mkdir(parents=True, exist_ok=True)
        (tdir / "result.md").write_text("Result body\\n", encoding="utf-8")
        return pid

    def _lifecycle_submit(tid, status, summary="Done."):
        return payload("taskflow", {
            "role": "worker",
            "action": "submit_task",
            "payload": {"taskId": tid, "status": status, "summary": summary},
        })

    # --- P0 ordering: a failed shared-storage sync withholds the
    #     completion notification and returns a retryable failure; the
    #     idempotent retry delivers it once storage recovers. ---
    _lifecycle_setup("order-task")
    os.environ["TEAMHARNESS_TEST_FAIL_SYNC_TASK"] = "order-task"
    try:
        order_result = _lifecycle_submit("order-task", "SUCCESS", "Result ready but storage is down.")
    finally:
        os.environ.pop("TEAMHARNESS_TEST_FAIL_SYNC_TASK", None)
    if order_result.get("ok") is not False:
        raise AssertionError(f"failed sync must not report ok: {order_result!r}")
    if order_result.get("retryable") is not True:
        raise AssertionError(f"failed sync must be retryable: {order_result!r}")
    if "notification" in order_result:
        raise AssertionError(f"failed sync must withhold the completion notification: {order_result!r}")
    if not order_result.get("task") or order_result["task"]["status"] != "submitted":
        raise AssertionError(f"local task state must still be submitted: {order_result!r}")
    order_retry = _lifecycle_submit("order-task", "SUCCESS", "Result ready but storage is down.")
    if not order_retry.get("ok") or order_retry.get("synced") is not True:
        raise AssertionError(f"retry after storage recovery must succeed: {order_retry!r}")
    if (order_retry.get("notification") or {}).get("sent") is not True:
        raise AssertionError(f"retry after storage recovery must send the notification: {order_retry!r}")

    # --- Skip branches: the completion notification is best-effort —
    #     missing Matrix env, a missing leader, or a leader that is not a
    #     joined room member each skip the notification without blocking
    #     the submission itself. ---
    _lifecycle_setup("skip-env-task")
    saved_url = os.environ.pop("AGENTTEAMS_MATRIX_URL", None)
    saved_token = os.environ.pop("AGENTTEAMS_WORKER_MATRIX_TOKEN", None)
    try:
        env_res = _lifecycle_submit("skip-env-task", "SUCCESS", "Matrix env unavailable.")
        if not env_res.get("ok"):
            raise AssertionError(f"missing matrix env must not block submit_task: {env_res!r}")
        env_notif = env_res.get("notification") or {}
        if env_notif.get("sent") is not False or "AGENTTEAMS_MATRIX_URL" not in str(env_notif.get("error", "")):
            raise AssertionError(f"missing matrix env must skip with a clear error: {env_notif!r}")
    finally:
        if saved_url is not None:
            os.environ["AGENTTEAMS_MATRIX_URL"] = saved_url
        if saved_token is not None:
            os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = saved_token

    _lifecycle_setup("skip-leader-task")
    rc_path = pathlib.Path(os.environ["TEAMHARNESS_RUNTIME_CONFIG"])
    saved_rc = rc_path.read_text(encoding="utf-8")
    try:
        rc_path.write_text(saved_rc.replace("matrixUserId: '@admin:example.test'", "matrixUserId: ''"), encoding="utf-8")
        leader_res = _lifecycle_submit("skip-leader-task", "SUCCESS", "No leader configured.")
        if not leader_res.get("ok"):
            raise AssertionError(f"missing leader must not block submit_task: {leader_res!r}")
        leader_notif = leader_res.get("notification") or {}
        if leader_notif.get("sent") is not False or "leader" not in str(leader_notif.get("error", "")).lower():
            raise AssertionError(f"missing leader must skip with a clear error: {leader_notif!r}")
    finally:
        rc_path.write_text(saved_rc, encoding="utf-8")

    _lifecycle_setup("skip-membership-task")
    os.environ["TEAMHARNESS_TEST_EXCLUDE_LEADER_FROM_ROOM"] = "1"
    try:
        member_res = _lifecycle_submit("skip-membership-task", "SUCCESS", "Leader outside the room.")
        if not member_res.get("ok"):
            raise AssertionError(f"leader not in room must not block submit_task: {member_res!r}")
        member_notif = member_res.get("notification") or {}
        if member_notif.get("sent") is not False or "not a joined member" not in str(member_notif.get("error", "")):
            raise AssertionError(f"leader outside the room must skip on membership: {member_notif!r}")
    finally:
        os.environ.pop("TEAMHARNESS_TEST_EXCLUDE_LEADER_FROM_ROOM", None)

    # --- Per-status first-line token + @initiator human mention. ---
    for status, token in (
        ("PARTIAL", "TASK_PARTIAL"),
        ("FAILED", "TASK_FAILED"),
        ("REVISION_NEEDED", "TASK_REVISION_NEEDED"),
    ):
        tid = f"tok-{status.lower()}"
        _lifecycle_setup(tid)
        res = _lifecycle_submit(tid, status, f"Status {status} case.")
        if not res.get("ok"):
            raise AssertionError(f"submit {status} failed: {res!r}")
        evs = [ev for ev in matrix["events"] if f"submit-{tid}-" in ev["path"]]
        if len(evs) != 1:
            raise AssertionError(f"expected exactly one completion event for {tid}: {evs!r}")
        body = evs[0]["content"].get("body", "")
        if f"{token}: {tid} -" not in body:
            raise AssertionError(f"{status} event must carry the {token} first line: {body!r}")
        if f"- Status: {status}" not in body:
            raise AssertionError(f"{status} event must carry the Status line: {body!r}")
        mentions = (evs[0]["content"].get("m.mentions") or {}).get("user_ids", [])
        if "@admin:example.test" not in mentions or "@luo:example.test" not in mentions:
            raise AssertionError(f"{status} event must mention leader and human initiator: {mentions!r}")
    ok_tid = "tok-success"
    _lifecycle_setup(ok_tid)
    ok_res = _lifecycle_submit(ok_tid, "SUCCESS", "All good.")
    ok_ev = [ev for ev in matrix["events"] if f"submit-{ok_tid}-" in ev["path"]]
    ok_body = ok_ev[0]["content"].get("body", "") if ok_ev else ""
    if f"TASK_COMPLETED: {ok_tid} - Result: shared/tasks/{ok_tid}/result.md" not in ok_body:
        raise AssertionError(f"SUCCESS event must keep the Result line: {ok_body!r}")
    if "- Status:" in ok_body:
        raise AssertionError(f"SUCCESS event must not carry a Status line: {ok_body!r}")

    # --- Invalid status is rejected at submit. ---
    bad_tid = "tok-invalid"
    _lifecycle_setup(bad_tid)
    bad = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {"taskId": bad_tid, "status": "MAYBE", "summary": "Not a real status."},
    })
    if bad.get("ok") or "invalid status" not in str(bad.get("error", "")):
        raise AssertionError(f"submit_task must reject unknown statuses: {bad!r}")
    bad_meta = json.loads(
        (pathlib.Path("#{workspace}") / f"shared/tasks/{bad_tid}/meta.json").read_text(encoding="utf-8")
    )
    if bad_meta.get("result_status") == "MAYBE":
        raise AssertionError(f"rejected status must not be persisted: {bad_meta!r}")

    # --- Re-submission with a changed status sends a new event. ---
    ch_tid = "tok-resubmit"
    _lifecycle_setup(ch_tid)
    first = _lifecycle_submit(ch_tid, "FAILED", "First pass failed.")
    n1 = (first.get("notification") or {}).get("eventId")
    if not first.get("ok") or (first.get("notification") or {}).get("sent") is not True or not n1:
        raise AssertionError(f"first FAILED submit must notify: {first!r}")
    second = _lifecycle_submit(ch_tid, "SUCCESS", "Fixed on resubmit.")
    n2 = (second.get("notification") or {}).get("eventId")
    if not second.get("ok") or (second.get("notification") or {}).get("sent") is not True or not n2 or n2 == n1:
        raise AssertionError(f"changed-status resubmit must send a new event: {second!r} (first={n1}, second={n2})")
    if len([ev for ev in matrix["events"] if f"submit-{ch_tid}-" in ev["path"]]) != 2:
        raise AssertionError("changed-status resubmit must not reuse the old event")
    same = _lifecycle_submit(ch_tid, "SUCCESS", "Idempotent same-status resubmit.")
    if (same.get("notification") or {}).get("reused") is not True:
        raise AssertionError(f"same-status resubmit must reuse the event: {same!r}")
    if (same.get("notification") or {}).get("eventId") != n2:
        raise AssertionError(f"same-status resubmit must reuse the same event id: {same!r}")

    # --- request_attention: in-flight ping, idempotent, terminal guard,
    #     resolved by accept_task_result. ---
    att_tid = "att-task"
    att_pid = _lifecycle_setup(att_tid)
    att1 = payload("taskflow", {
        "role": "worker",
        "action": "request_attention",
        "payload": {"taskId": att_tid, "kind": "approval", "question": "Ship to production?"},
    })
    if not att1.get("ok") or (att1.get("attention") or {}).get("notification", {}).get("sent") is not True:
        raise AssertionError(f"request_attention must notify the room: {att1!r}")
    att_ev = [ev for ev in matrix["events"] if f"attention-{att_tid}-approval-" in ev["path"]]
    if len(att_ev) != 1:
        raise AssertionError(f"expected one attention event: {att_ev!r}")
    att_body = att_ev[0]["content"].get("body", "")
    if f"ATTENTION_APPROVAL: {att_tid} - Ship to production?" not in att_body:
        raise AssertionError(f"attention event must carry the contract line: {att_body!r}")
    mentions = (att_ev[0]["content"].get("m.mentions") or {}).get("user_ids", [])
    if "@admin:example.test" not in mentions or "@luo:example.test" not in mentions:
        raise AssertionError(f"attention event must mention leader and human: {mentions!r}")
    att2 = payload("taskflow", {
        "role": "worker",
        "action": "request_attention",
        "payload": {"taskId": att_tid, "kind": "approval", "question": "Ship to production?"},
    })
    if not att2.get("ok") or (att2.get("attention") or {}).get("reused") is not True:
        raise AssertionError(f"unresolved same-kind attention must be idempotent: {att2!r}")
    if len([ev for ev in matrix["events"] if f"attention-{att_tid}-approval-" in ev["path"]]) != 1:
        raise AssertionError("idempotent attention must not send a second event")
    att3 = payload("taskflow", {
        "role": "worker",
        "action": "request_attention",
        "payload": {"taskId": att_tid, "kind": "escalation", "question": "Escalating: storage at capacity."},
    })
    if not att3.get("ok") or (att3.get("attention") or {}).get("reused") is True:
        raise AssertionError(f"different kind must not reuse the pending event: {att3!r}")
    att_close = payload("taskflow", {
        "role": "worker",
        "action": "request_attention",
        "payload": {"taskId": att_tid, "kind": "escalation", "question": "Closing early.", "resolved": True},
    })
    if not att_close.get("ok") or (att_close.get("attention") or {}).get("resolved") is not True:
        raise AssertionError(f"explicit resolved=true must close the open loop: {att_close!r}")
    _lifecycle_submit(att_tid, "BLOCKED", "Blocked on storage.")
    accepted = payload("projectflow", {
        "role": "leader",
        "action": "accept_task_result",
        "payload": {"projectId": att_pid, "taskId": att_tid, "resultStatus": "BLOCKED"},
    })
    if not accepted.get("ok"):
        raise AssertionError(f"accept_task_result failed: {accepted!r}")
    att_meta = json.loads(
        (pathlib.Path("#{workspace}") / f"shared/tasks/{att_tid}/meta.json").read_text(encoding="utf-8")
    )
    unresolved = [item for item in (att_meta.get("attention") or []) if not item.get("resolved")]
    if unresolved:
        raise AssertionError(f"accept_task_result must resolve outstanding attention: {att_meta.get('attention')!r}")
    can_tid = "att-cancel"
    can_pid = _lifecycle_setup(can_tid)
    cancelled = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"projectId": can_pid, "taskId": can_tid, "reason": "Cancelled for attention guard test."},
    })
    if not cancelled.get("ok"):
        raise AssertionError(f"cancel_task failed: {cancelled!r}")
    att5 = payload("taskflow", {
        "role": "worker",
        "action": "request_attention",
        "payload": {"taskId": can_tid, "kind": "approval", "question": "Too late now."},
    })
    if att5.get("ok"):
        raise AssertionError(f"request_attention must reject a terminal task: {att5!r}")

    # --- complete_project: PROJECT_COMPLETED event + idempotent retry. ---
    comp_tid = "comp-task"
    comp_pid = _lifecycle_setup(comp_tid)
    _lifecycle_submit(comp_tid, "SUCCESS", "Comp work done.")
    comp = payload("projectflow", {
        "action": "complete_project",
        "payload": {"projectId": comp_pid},
    })
    if not comp.get("ok"):
        raise AssertionError(f"complete_project failed: {comp!r}")
    note = (comp.get("project") or {}).get("projectNotification") or {}
    if note.get("sent") is not True:
        raise AssertionError(f"complete_project must send PROJECT_COMPLETED: {note!r}")
    comp_ev = [ev for ev in matrix["events"] if f"project-{comp_pid}-" in ev["path"]]
    if len(comp_ev) != 1:
        raise AssertionError(f"expected one project completion event: {comp_ev!r}")
    comp_body = comp_ev[0]["content"].get("body", "")
    if f"PROJECT_COMPLETED: {comp_pid} - Project completed:" not in comp_body:
        raise AssertionError(f"project event must carry the contract line: {comp_body!r}")
    mentions = (comp_ev[0]["content"].get("m.mentions") or {}).get("user_ids", [])
    if "@admin:example.test" not in mentions or "@luo:example.test" not in mentions:
        raise AssertionError(f"project event must mention leader and human: {mentions!r}")
    comp2 = payload("projectflow", {
        "action": "complete_project",
        "payload": {"projectId": comp_pid},
    })
    note2 = (comp2.get("project") or {}).get("projectNotification") or {}
    if note2.get("reused") is not True or note2.get("eventId") != note.get("eventId"):
        raise AssertionError(f"retried complete_project must reuse the event: {note2!r}")
    if len([ev for ev in matrix["events"] if f"project-{comp_pid}-" in ev["path"]]) != 1:
        raise AssertionError("retried complete_project must not send a second event")

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
