#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"
require "tmpdir"

repo_root = Pathname.new(__dir__).join("../../../../..").expand_path
mcp_dir = repo_root / "plugins/teamharness/mcp"
contracts_dir = repo_root / "plugins/teamharness/contracts"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

Dir.mktmpdir("teamharness-transition-") do |dir|
  root = Pathname.new(dir)
  workspace = root / "workspace"
  bin_dir = root / "bin"
  log_path = root / "mc.log"
  bin_dir.mkpath
  (bin_dir / "mc").write(<<~SH)
    #!/usr/bin/env bash
    printf '%s\\n' "$*" >> "#{log_path}"
    if [ "$1" = "mirror" ]; then
      mkdir -p "$3"
    fi
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
    import urllib.parse

    sys.path.insert(0, str(pathlib.Path("#{mcp_dir}")))
    from server import (
        TASK_HISTORY_LIMIT,
        TERMINAL_TASK_STATUSES,
        TRANSITIONS,
        _append_transition_history,
        call_tool,
    )

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
            })
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(
                json.dumps({"content_uri": f"mxc://example.test/{len(matrix['uploads'])}-{filename}"}).encode("utf-8")
            )

        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path.endswith("/members"):
                members = [
                    {"state_key": "@worker-a:example.test", "content": {"membership": "join"}},
                    {"state_key": "@admin:example.test", "content": {"membership": "join"}},
                ]
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
            matrix["events"].append({
                "path": parsed.path,
                "content": json.loads(body.decode("utf-8") or "{}"),
            })
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(
                json.dumps({"event_id": f"$evt-{len(matrix['events'])}"}).encode("utf-8")
            )

    class MatrixServer(socketserver.ThreadingTCPServer):
        allow_reuse_address = True
        daemon_threads = True

    matrix_server = MatrixServer(("127.0.0.1", 0), MatrixHandler)
    threading.Thread(target=matrix_server.serve_forever, daemon=True).start()
    os.environ["AGENTTEAMS_MATRIX_URL"] = f"http://127.0.0.1:{matrix_server.server_address[1]}"
    os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = "test-token"

    def payload(name, args):
        merged = dict(common)
        merged.update(args)
        return json.loads(call_tool(name, merged)["content"][0]["text"])

    def read_meta(tid):
        return json.loads(
            (workspace / f"shared/tasks/{tid}/meta.json").read_text(encoding="utf-8")
        )

    def read_project(pid):
        return json.loads(
            (workspace / f"shared/projects/{pid}/meta.json").read_text(encoding="utf-8")
        )

    def new_project(pid, tid, with_assignee=True):
        payload("projectflow", {
            "action": "create_project",
            "payload": {"projectId": pid, "title": f"Transition fixture {pid}"},
        })
        task = {"taskId": tid, "title": "T", "dependsOn": []}
        if with_assignee:
            task["assignedTo"] = "@worker-a:example.test"
        payload("projectflow", {
            "action": "plan_dag",
            "payload": {"projectId": pid, "tasks": [task]},
        })

    def delegate(pid, tid, spec="Spec.", **extra):
        pl = {
            "projectId": pid,
            "taskId": tid,
            "roomId": "room:!team:example.test",
            "spec": spec,
        }
        pl.update(extra)
        return payload("taskflow", {"role": "leader", "action": "delegate_task", "payload": pl})

    def ack(tid):
        return payload("taskflow", {"role": "worker", "action": "ack_task", "payload": {"taskId": tid}})

    def submit(tid, status="SUCCESS", summary="Done."):
        return payload("taskflow", {
            "role": "worker",
            "action": "submit_task",
            "payload": {"taskId": tid, "status": status, "summary": summary},
        })

    def progress(tid, note):
        return payload("taskflow", {
            "role": "worker",
            "action": "report_progress",
            "payload": {"taskId": tid, "note": note},
        })

    def must_fail(res, fragment, label):
        if res.get("ok"):
            raise AssertionError(f"{label}: expected failure but succeeded: {res!r}")
        err = str(res.get("error", ""))
        if fragment not in err:
            raise AssertionError(f"{label}: error {err!r} missing {fragment!r}")
        return res

    def entry(h, i):
        return (h[i]["from"], h[i]["to"], h[i]["action"], h[i].get("actor"))

    # ==================================================================
    # 1. Golden fixture consistency (cross-language single source of truth)
    # ==================================================================
    fixture = json.loads(
        pathlib.Path("#{contracts_dir}")
        .joinpath("task-transitions.json")
        .read_text(encoding="utf-8")
    )
    if TRANSITIONS != fixture["transitions"]:
        raise AssertionError(
            f"TRANSITIONS drifted from golden fixture: {TRANSITIONS!r} vs {fixture['transitions']!r}"
        )
    if set(TERMINAL_TASK_STATUSES) != set(fixture["terminal"]):
        raise AssertionError(
            f"TERMINAL_TASK_STATUSES drifted from golden fixture: "
            f"{TERMINAL_TASK_STATUSES!r} vs {fixture['terminal']!r}"
        )
    if TASK_HISTORY_LIMIT != 50:
        raise AssertionError(f"history cap must stay 50 (projectHistoryLimit precedent), got {TASK_HISTORY_LIMIT}")
    all_states = set(fixture["states"])
    for src, targets in fixture["transitions"].items():
        if src not in all_states:
            raise AssertionError(f"unknown source state in fixture: {src}")
        for t in targets:
            if t not in all_states:
                raise AssertionError(f"unknown target state in fixture: {t}")
    for t in fixture["terminal"]:
        if fixture["transitions"].get(t):
            raise AssertionError(f"terminal state {t} must have no outbound edges")

    # ==================================================================
    # 2. Happy path: every table edge exercised through the public API,
    #    with the full history chain asserted.
    # ==================================================================
    new_project("proj-happy", "t-happy")
    d = delegate("proj-happy", "t-happy")
    if not d.get("ok") or d["task"]["status"] != "assigned":
        raise AssertionError(f"happy delegate failed: {d!r}")
    h = read_meta("t-happy")["history"]
    if len(h) != 2:
        raise AssertionError(f"delegate must record planned->prepared->assigned: {h!r}")
    if entry(h, 0) != ("planned", "prepared", "delegate_task", "leader:default"):
        raise AssertionError(f"creation history entry wrong: {h[0]!r}")
    if entry(h, 1) != ("prepared", "assigned", "delegate_task", "leader:default"):
        raise AssertionError(f"assignment history entry wrong: {h[1]!r}")
    def is_rfc3339_utc(ts):
        if not ts.endswith("Z") or "T" not in ts:
            return False
        from datetime import datetime
        body = ts[:-1]
        if "." in body:
            head, frac = body.split(".", 1)
            if len(frac) > 6 or not frac.isdigit():
                return False
        else:
            head = body
        try:
            datetime.strptime(head, "%Y-%m-%dT%H:%M:%S")
            return True
        except ValueError:
            return False

    if not is_rfc3339_utc(h[0]["ts"]):
        raise AssertionError(f"history ts must be RFC3339 UTC: {h[0]['ts']!r}")

    a = ack("t-happy")
    if not a.get("ok") or a["task"]["status"] != "in_progress":
        raise AssertionError(f"happy ack failed: {a!r}")
    h = read_meta("t-happy")["history"]
    if entry(h, 2) != ("assigned", "in_progress", "ack_task", "worker:default"):
        raise AssertionError(f"ack history entry wrong: {h[2]!r}")

    events_before = len(matrix["events"])
    p = progress("t-happy", "Halfway there.")
    if not p.get("ok") or p["task"]["status"] != "in_progress":
        raise AssertionError(f"happy progress failed: {p!r}")
    if len(matrix["events"]) != events_before:
        raise AssertionError("report_progress must not send a room notification")
    h = read_meta("t-happy")["history"]
    if entry(h, 3) != ("in_progress", "in_progress", "progress", "worker:default"):
        raise AssertionError(f"progress history entry wrong: {h[3]!r}")
    if h[3].get("note") != "Halfway there.":
        raise AssertionError(f"progress note not recorded: {h[3]!r}")

    s = submit("t-happy")
    if not s.get("ok") or s["task"]["status"] != "submitted":
        raise AssertionError(f"happy submit failed: {s!r}")
    h = read_meta("t-happy")["history"]
    if entry(h, 4) != ("in_progress", "submitted", "submit_task", "worker:default"):
        raise AssertionError(f"submit history entry wrong: {h[4]!r}")

    sub_id = s["task"]["submission_id"]
    acc = payload("projectflow", {
        "role": "leader",
        "action": "accept_task_result",
        "payload": {
            "projectId": "proj-happy",
            "taskId": "t-happy",
            "submissionId": sub_id,
            "accepted": True,
            "resultStatus": "SUCCESS",
            "summary": "Looks good.",
        },
    })
    if not acc.get("ok"):
        raise AssertionError(f"happy accept failed: {acc!r}")
    m = read_meta("t-happy")
    if m["status"] != "completed":
        raise AssertionError(f"accept must sync the task meta (gap fix): {m['status']!r}")
    h = m["history"]
    if entry(h, 5) != ("submitted", "completed", "accept_task_result", "leader:default"):
        raise AssertionError(f"accept history entry wrong: {h[5]!r}")
    node = next(n for n in read_project("proj-happy")["tasks"] if n.get("task_id") == "t-happy")
    if node.get("status") != "completed":
        raise AssertionError(f"project node must be completed: {node!r}")

    # History chain integrity: each entry starts where the previous one ended,
    # timestamps non-decreasing.
    for i in range(1, len(h)):
        if h[i]["from"] != h[i - 1]["to"]:
            raise AssertionError(f"history chain broken at {i}: {h!r}")
    ts_list = [e["ts"] for e in h]
    if ts_list != sorted(ts_list):
        raise AssertionError(f"history timestamps must be non-decreasing: {ts_list!r}")

    # ==================================================================
    # 3. Out-of-order transitions are rejected with actionable errors.
    # ==================================================================
    # 3a. prepared (delegate without an assignee target): ack/submit/progress
    #     all reject until the assignment lands.
    payload("projectflow", {
        "action": "create_project",
        "payload": {"projectId": "proj-prep", "title": "Prepared fixture"},
    })
    d = delegate("proj-prep", "t-prep")
    if d.get("ok"):
        raise AssertionError(f"delegate without an assignee target must stay retryable: {d!r}")
    if read_meta("t-prep")["status"] != "prepared":
        raise AssertionError("unnotified delegation must leave the task prepared")
    must_fail(ack("t-prep"), "ack_task: task is 'prepared'", "ack@prepared")
    must_fail(submit("t-prep"), "submit_task: task is 'prepared'", "submit@prepared")
    must_fail(progress("t-prep", "x"), "report_progress: task is 'prepared'", "progress@prepared")

    # 3b. in_progress must not be re-delegated (would reset recorded progress).
    new_project("proj-redel", "t-redel")
    if not delegate("proj-redel", "t-redel").get("ok"):
        raise AssertionError("redel delegate failed")
    if not ack("t-redel").get("ok"):
        raise AssertionError("redel ack failed")
    must_fail(delegate("proj-redel", "t-redel"), "delegate_task: task is 'in_progress'", "redelegate@in_progress")
    if read_meta("t-redel")["status"] != "in_progress":
        raise AssertionError("failed redelegate must not reset the task state")

    # 3c. submitted must not be re-delegated either.
    new_project("proj-subd", "t-subd")
    if not delegate("proj-subd", "t-subd").get("ok"):
        raise AssertionError("subd delegate failed")
    if not ack("t-subd").get("ok"):
        raise AssertionError("subd ack failed")
    if not submit("t-subd").get("ok"):
        raise AssertionError("subd submit failed")
    must_fail(delegate("proj-subd", "t-subd"), "delegate_task: task is 'submitted'", "redelegate@submitted")

    # 3d. accept only from submitted.
    acc_bad = payload("projectflow", {
        "role": "leader",
        "action": "accept_task_result",
        "payload": {
            "projectId": "proj-redel",
            "taskId": "t-redel",
            "accepted": True,
            "resultStatus": "SUCCESS",
            "summary": "Too early.",
        },
    })
    must_fail(acc_bad, "requires submitted task state, got in_progress", "accept@in_progress")
    if read_meta("t-redel")["status"] != "in_progress":
        raise AssertionError("rejected accept must not change the task state")

    # 3e. accept is leader-only.
    must_fail(
        payload("projectflow", {
            "role": "worker",
            "action": "accept_task_result",
            "payload": {"projectId": "proj-redel", "taskId": "t-redel", "accepted": True, "resultStatus": "SUCCESS"},
        }),
        "accept_task_result requires leader role",
        "accept@worker",
    )

    # 3f. terminal tasks are frozen.
    must_fail(
        payload("taskflow", {
            "role": "leader",
            "action": "cancel_task",
            # submissionId required because t-happy was submitted (#1183 fence);
            # the terminal guard must reject it regardless.
            "payload": {"taskId": "t-happy", "submissionId": sub_id, "reason": "Too late."},
        }),
        "cannot cancel terminal task",
        "cancel@terminal",
    )
    # Terminal tasks are frozen by the mutability guard before the
    # progress status gate (the terminal check is upstream of it).
    must_fail(progress("t-happy", "done now"), "report_progress cannot update terminal task", "progress@completed")
    must_fail(
        payload("taskflow", {
            "role": "leader",
            "action": "cancel_task",
            "payload": {"taskId": "t-nope", "reason": "x"},
        }),
        "task not found",
        "cancel@no-meta",
    )

    # ==================================================================
    # 4. Same-state re-entry is a legal no-op (idempotent retry).
    # ==================================================================
    history_len = len(read_meta("t-redel")["history"])
    a2 = ack("t-redel")
    if not a2.get("ok") or a2["task"]["status"] != "in_progress":
        raise AssertionError(f"idempotent ack failed: {a2!r}")
    if len(read_meta("t-redel")["history"]) != history_len:
        raise AssertionError("idempotent ack must not append history")

    # ==================================================================
    # 5. report_progress specifics.
    # ==================================================================
    must_fail(
        payload("taskflow", {
            "role": "leader",
            "action": "report_progress",
            "payload": {"taskId": "t-redel", "note": "x"},
        }),
        "report_progress requires worker",
        "progress@leader",
    )
    must_fail(progress("t-redel", "   "), "note is required", "progress@empty")
    long_note = "x" * 250
    p = progress("t-redel", long_note)
    if not p.get("ok") or p.get("truncated") is not True:
        raise AssertionError(f"long progress note must be truncated and flagged: {p!r}")
    last = read_meta("t-redel")["history"][-1]
    if len(last.get("note", "")) != 201 or not last["note"].endswith("\u2026"):
        raise AssertionError(f"truncated note must be 200 chars + ellipsis: {last['note']!r}")
    must_fail(progress("t-subd", "late"), "report_progress: task is 'submitted'", "progress@submitted")

    # ==================================================================
    # 6. History cap and no-op semantics (unit level).
    # ==================================================================
    capped = {}
    for i in range(55):
        _append_transition_history(capped, "planned", "in_progress", "ack_task", f"worker:{i}")
    if len(capped["history"]) != TASK_HISTORY_LIMIT:
        raise AssertionError(f"history must cap at {TASK_HISTORY_LIMIT}: {len(capped['history'])}")
    if capped["history"][0]["actor"] != "worker:5":
        raise AssertionError(f"cap must drop the oldest entry: {capped['history'][0]!r}")
    noop = {}
    _append_transition_history(noop, "in_progress", "in_progress", "ack_task", "worker:a")
    if noop.get("history"):
        raise AssertionError("unmarked no-op re-entries must not be recorded")
    _append_transition_history(noop, "in_progress", "in_progress", "progress", "worker:a", "explicit", record_noop=True)
    if len(noop["history"]) != 1:
        raise AssertionError("record_noop must record the entry")

    # ==================================================================
    # 7. Cancel leaves an auditable trace (reason + terminal fields).
    # ==================================================================
    new_project("proj-cancel", "t-cancel")
    if not delegate("proj-cancel", "t-cancel").get("ok"):
        raise AssertionError("cancel delegate failed")
    if not ack("t-cancel").get("ok"):
        raise AssertionError("cancel ack failed")
    c = payload("taskflow", {
        "role": "leader",
        "action": "cancel_task",
        "payload": {"taskId": "t-cancel", "reason": "Scope changed."},
    })
    if not c.get("ok") or c["task"]["status"] != "cancelled":
        raise AssertionError(f"cancel failed: {c!r}")
    m = read_meta("t-cancel")
    last = m["history"][-1]
    if (last["from"], last["to"], last["action"], last.get("note")) != (
        "in_progress", "cancelled", "cancel_task", "Scope changed."
    ):
        raise AssertionError(f"cancel history entry wrong: {last!r}")
    if m.get("cancel_reason") != "Scope changed." or not m.get("cancelled_at"):
        raise AssertionError(f"cancel fields missing: {m!r}")
    node = next(n for n in read_project("proj-cancel")["tasks"] if n.get("task_id") == "t-cancel")
    if node.get("status") != "cancelled":
        raise AssertionError(f"project node must be cancelled: {node!r}")

    matrix_server.shutdown()
    matrix_server.server_close()

    print(json.dumps({"ok": True}, ensure_ascii=False))
  PY

  env = {"PATH" => "#{bin_dir}:#{ENV.fetch("PATH", "")}"}
  stdout, stderr, status = Open3.capture3(env, "python3", "-", stdin_data: python_test, chdir: repo_root.to_s)
  fail!([
    "teamharness transition table MCP test failed",
    stderr,
    stdout
  ].reject(&:empty?).join("\n")) unless status.success?

  puts JSON.parse(stdout)
end
