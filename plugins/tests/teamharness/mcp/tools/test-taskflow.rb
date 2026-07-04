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
    "task_id" => "remote-001",
    "project_id" => project_id = "remote-project",
    "room_id" => "room:!team:example.test",
    "status" => "assigned",
    "spec_path" => "shared/tasks/remote-001/spec.md"
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
    import json
    import os
    import pathlib
    import sys

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    from server import call_tool

    common = {
        "workspaceDir": "#{workspace}",
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

    acked = payload("taskflow", {
        "role": "worker",
        "action": "ack_task",
        "payload": {"taskId": task_id},
    })
    if not acked.get("ok") or acked["task"]["status"] != "in_progress":
        raise AssertionError(f"ack_task failed: {acked!r}")

    submitted = payload("taskflow", {
        "role": "worker",
        "action": "submit_task",
        "payload": {
            "taskId": task_id,
            "status": "SUCCESS",
            "summary": "Input collected.",
            "deliverables": ["shared/tasks/t-001/result.md"],
        },
    })
    if not submitted.get("ok") or submitted["task"]["status"] != "submitted":
        raise AssertionError(f"submit_task failed: {submitted!r}")
    if submitted.get("synced") is not True:
        raise AssertionError(f"submit_task did not sync result: {submitted!r}")

    checked = payload("taskflow", {
        "role": "leader",
        "action": "check_task",
        "payload": {"taskId": task_id},
    })
    if not checked.get("ok") or not checked.get("effective"):
        raise AssertionError(f"check_task failed: {checked!r}")
    if checked.get("result", {}).get("summary") != "Input collected.":
        raise AssertionError(f"check_task did not return result summary: {checked!r}")

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
    marked_report = payload("projectflow", {
        "action": "mark_requester_report_sent",
        "payload": {"projectId": project_id},
    })
    if not marked_report.get("ok"):
        raise AssertionError(f"mark_requester_report_sent failed: {marked_report!r}")
    if marked_report["project"].get("requester_report", {}).get("pending") is not False:
        raise AssertionError(f"mark_requester_report_sent did not clear pending flag: {marked_report!r}")

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

    result_path_for_validation = pathlib.Path("#{workspace}") / "shared/tasks/t-001/result.md"
    original_result = result_path_for_validation.read_text(encoding="utf-8")
    result_path_for_validation.write_text("# Task Result\\n\\n- Summary: Missing status.\\n", encoding="utf-8")
    invalid_checked = payload("taskflow", {
        "role": "leader",
        "action": "check_task",
        "payload": {"taskId": task_id},
    })
    if not invalid_checked.get("ok") or invalid_checked.get("effective"):
        raise AssertionError(f"invalid result should be ineffective: {invalid_checked!r}")
    if "missing result status" not in invalid_checked.get("validationErrors", []):
        raise AssertionError(f"invalid result should include validation error: {invalid_checked!r}")
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

    os.environ["HICLAW_WORKER_ROLE"] = "worker"
    remote_ack = payload("taskflow", {
        "action": "ack_task",
        "task_id": "remote-001",
    })
    if not remote_ack.get("ok") or remote_ack["task"]["status"] != "in_progress":
        raise AssertionError(f"ack_task did not infer role and pull remote task: {remote_ack!r}")
    if not (pathlib.Path("#{workspace}") / "shared/tasks/remote-001/spec.md").exists():
        raise AssertionError("ack_task did not pull remote task spec")

    workspace = pathlib.Path("#{workspace}")
    spec_path = workspace / "shared/tasks/t-001/spec.md"
    result_path = workspace / "shared/tasks/t-001/result.md"
    meta_path = workspace / "shared/tasks/t-001/meta.json"
    legacy_state_path = workspace / "shared/tasks/t-001/task.json"
    if not spec_path.exists() or not result_path.exists() or not meta_path.exists():
        raise AssertionError("task files missing")
    if legacy_state_path.exists():
        raise AssertionError(f"task.json should not be written: {legacy_state_path}")

    print(json.dumps({
        "ok": True,
      "task": submitted["task"]["task_id"],
      "status": submitted["task"]["status"],
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
