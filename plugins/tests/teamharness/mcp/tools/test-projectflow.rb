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
    import json
    import os
    import pathlib
    import sys

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    import server
    from server import call_tool

    server.time.strftime = lambda fmt: "20260605-112233"

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

    def payload(args):
        merged = dict(common)
        merged.update(args)
        result = call_tool("projectflow", merged)
        return json.loads(result["content"][0]["text"])

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

    print(json.dumps({
        "ok": True,
        "project": created["project"]["project_id"],
        "ready": ready,
        "loopReady": loop_ready,
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
