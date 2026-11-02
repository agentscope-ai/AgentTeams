#!/usr/bin/env python3
"""Subprocess driver for the loop pause/resume e2e test.

Drives the real TeamHarness taskflow (``mcp.lifecycle.taskflow``) inside
this process, exactly like a worker/leader agent would through the MCP
server.  ONLY the two Matrix notifiers are stubbed:

- ``_send_delegate_notification`` (a refs proxy -> patched on the server
  module, same mechanism test_delegate_redelegation.py uses), and
- ``_task_completion_notification`` (a plain module import in taskflow ->
  patched on the taskflow module object).

Each stub appends one JSON line ``{action, kind, task_id, ...}`` to the
file named by ``E2E_SENT_JSONL`` and returns a "sent" result.  Everything
else runs through the real code paths: the ``mc`` pull/push against the
object store (``_pull_project`` / ``_pull_task`` / ``_sync_task`` /
``_sync_project``), the task/project state files, the pause gate, the
transition table and the artifact publish bookkeeping.

Optional boundary injection (``--pause-loop-before-commit LOOP``, submit
only): before the FIRST conditional project meta commit of the run the
driver fires a REAL loop pause
(``POST {AGENTTEAMS_CONTROLLER_URL}/api/v1/loops/{LOOP}/pause`` with
``{"source": "user"}`` — the same endpoint the e2e test drives), so the
pause lands between the commit's ETag read and its PUT — strictly later
than any pull-window race. The injection outcome (fired / HTTP status /
every commit response) is added to the printed payload as
``pause_injection``.

CLI:
  python loop_driver.py --action delegate --project P --task T --worker W
  python loop_driver.py --action submit --project P --task T --worker W \
      --result '{"summary": "...", "status": "SUCCESS",
                 "deliverables": ["shared/tasks/T/out.txt"]}' \
      [--pause-loop-before-commit LOOP]
  python loop_driver.py --action status --project P --task T --worker W
  python loop_driver.py --action plan_loop --project P \
      --payload '{"goal": "...", "stopCondition": "...",
                  "iterationTemplate": "...", "maxIterations": 2,
                  "tasks": [{"taskId": "A"}, {"taskId": "B", "dependsOn": ["A"]}]}'

Environment:
  E2E_WORKSPACE    workspace directory (fresh per invocation = a fresh
                   process re-reading persisted state from the object store)
  E2E_SENT_JSONL   file the notification stubs append to
  AGENTTEAMS_FS_*  real object-store env (endpoint/keys/bucket/prefix)
  AGENTTEAMS_MATRIX_URL / AGENTTEAMS_WORKER_MATRIX_TOKEN
                   dummy values are fine: the stubs intercept before any
                   HTTP call, the env only arms the send path
  E2E_CONTROLLER_URL / E2E_CONTROLLER_TOKEN
                   optional; mapped onto AGENTTEAMS_CONTROLLER_URL /
                   AGENTTEAMS_AUTH_TOKEN via setdefault (an explicit
                   AGENTTEAMS_* value always wins) so the
                   pause-before-commit injection reaches the real
                   controller without the caller duplicating the env

The resulting JSON payload is printed on stdout (single line).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402  (bootstraps the mcp.* package root)
import mcp.artifact.sync as _sync_mod  # noqa: E402
import mcp.lifecycle.taskflow as _taskflow_mod  # noqa: E402

# A non-Matrix task room: membership validation and artifact publish both
# skip the network for room ids that do not start with "!" — the only
# network calls this driver would otherwise make are the two stubbed
# notifiers.
ROOM_ID = "room:loop-e2e"


def _sent_path() -> Path:
    return Path(os.environ.get("E2E_SENT_JSONL", "").strip() or "/dev/null")


def _record_sent(action: str, kind: str, task_id: str, **extra: Any) -> None:
    record = {"action": action, "kind": kind, "task_id": task_id, **extra}
    path = _sent_path()
    if str(path) == "/dev/null":
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")


def _stub_delegate_notification(
    arguments: dict[str, Any],
    *,
    room_id: str,
    task_id: str,
    title: str,
    assignee: str,
    spec: str,
    txn: str | None = None,
) -> dict[str, Any]:
    """Stub for room/notify._send_delegate_notification (same signature)."""
    _record_sent(
        "delegate_task",
        "delegate",
        task_id,
        room_id=room_id,
        assignee=assignee,
        title=title,
        txn=txn or "",
    )
    return {
        "sent": True,
        "eventId": f"$e2e-delegate-{task_id}-{uuid.uuid4().hex[:12]}",
        "roomId": str(room_id or ""),
        "assignee": assignee,
    }


def _stub_task_completion_notification(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    status: str,
    summary: str,
) -> dict[str, Any]:
    """Stub for room/notify._task_completion_notification (same signature)."""
    room_id = str(task.get("room_id") or "") if isinstance(task, dict) else ""
    _record_sent(
        "submit_task",
        "completion",
        task_id,
        room_id=room_id,
        status=status,
    )
    return {
        "sent": True,
        "eventId": f"$e2e-completion-{task_id}-{uuid.uuid4().hex[:12]}",
        "roomId": room_id,
        "leader": "e2e-stub",
    }


# The ONLY stubs: the two Matrix notifiers.  Everything else stays real.
server._send_delegate_notification = _stub_delegate_notification
_taskflow_mod._task_completion_notification = _stub_task_completion_notification


def _install_pause_injection(loop_name: str) -> dict[str, Any]:
    """Wrap ``sync._commit_project_meta`` with a one-shot REAL loop pause.

    Before the first conditional project meta commit of the run the wrapper
    fires ``POST {AGENTTEAMS_CONTROLLER_URL}/api/v1/loops/{loop}/pause``
    with ``{"source": "user"}`` — the same endpoint the e2e test drives —
    and only then performs the commit. The pause therefore lands between
    the commit's ETag read and its PUT (a strictly later boundary than any
    pull-window race), the commit must hit a real 409, and the retry's
    re-snapshot + re-merge must preserve the durable pause. Every commit
    response is recorded so the test can assert the real 409 -> 200 chain.
    Returns the info dict: {loop, fired, status, commits}.
    """
    info: dict[str, Any] = {
        "loop": loop_name,
        "fired": False,
        "status": None,
        "commits": [],
    }
    real_commit = _sync_mod._commit_project_meta

    def _fire_pause() -> None:
        base = os.environ.get("AGENTTEAMS_CONTROLLER_URL", "").strip().rstrip("/")
        if not base:
            raise AssertionError(
                "pause-before-commit injection needs AGENTTEAMS_CONTROLLER_URL "
                "(the driver maps E2E_CONTROLLER_URL onto it by default)"
            )
        token = os.environ.get("AGENTTEAMS_AUTH_TOKEN", "").strip()
        request = urllib.request.Request(
            f"{base}/api/v1/loops/{loop_name}/pause",
            data=json.dumps({"source": "user"}).encode("utf-8"),
            method="POST",
        )
        request.add_header("Content-Type", "application/json")
        if token:
            request.add_header("Authorization", f"Bearer {token}")
        try:
            with urllib.request.urlopen(request, timeout=15) as response:
                info["status"] = response.status
        except urllib.error.HTTPError as exc:
            info["status"] = exc.code
            raise AssertionError(
                f"pause-before-commit injection failed: loop pause -> HTTP {exc.code}: "
                f"{exc.read().decode('utf-8', errors='replace')[:500]}"
            ) from exc
        except Exception as exc:  # unreachable controller / timeout
            raise AssertionError(
                f"pause-before-commit injection failed: loop pause unreachable: {exc}"
            ) from exc

    def _commit_with_injection(
        project_id: str, meta: dict[str, Any], etag: str
    ) -> dict[str, Any]:
        if not info["fired"]:
            _fire_pause()
            info["fired"] = True
        result = real_commit(project_id, meta, etag)
        info["commits"].append(dict(result))
        return result

    _sync_mod._commit_project_meta = _commit_with_injection
    return info


_ACTION_MAP = {
    "delegate": "delegate_task",
    "submit": "submit_task",
    "status": "check_task",
    # projectflow entry point (not taskflow): the loop plan lands on the
    # project meta's loop record.
    "plan_loop": "plan_loop",
}

# Which MCP tool each action dispatches through.
_TOOL_FOR_ACTION = {
    "plan_loop": "projectflow",
}


def _arguments(action: str, project_id: str, task_id: str, workspace: Path) -> dict[str, Any]:
    role = "worker" if action == "submit" else "leader"
    return {
        "role": role,
        "action": _ACTION_MAP[action],
        "workspaceDir": str(workspace),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="loop pause/resume e2e driver")
    parser.add_argument("--action", required=True, choices=sorted(_ACTION_MAP))
    parser.add_argument("--project", required=True)
    parser.add_argument("--task", default="", help="task id (delegate/submit/status)")
    parser.add_argument("--worker", default="", help="assignee/submitter (delegate/submit)")
    parser.add_argument("--result", default="", help="submit result JSON: {summary,status,deliverables}")
    parser.add_argument(
        "--payload",
        default="",
        help="plan_loop payload JSON: {goal,stopCondition,iterationTemplate,maxIterations,tasks}",
    )
    parser.add_argument(
        "--pause-loop-before-commit",
        default="",
        help="submit only: fire a real loop pause (source user) right before the first "
        "conditional meta commit; the injection result is added to the payload "
        "as pause_injection",
    )
    args = parser.parse_args()

    # Map the e2e job's controller env onto the names the sync module reads
    # (setdefault: an explicit AGENTTEAMS_* value always wins).
    controller_url = os.environ.get("E2E_CONTROLLER_URL", "").strip()
    if controller_url:
        os.environ.setdefault("AGENTTEAMS_CONTROLLER_URL", controller_url)
    controller_token = os.environ.get("E2E_CONTROLLER_TOKEN", "").strip()
    if controller_token:
        os.environ.setdefault("AGENTTEAMS_AUTH_TOKEN", controller_token)

    if args.pause_loop_before_commit:
        if args.action != "submit":
            print(json.dumps({
                "ok": False,
                "error": "--pause-loop-before-commit is only supported with --action submit",
            }))
            return 2
        pause_injection = _install_pause_injection(args.pause_loop_before_commit)
    else:
        pause_injection = None

    workspace = Path(os.environ.get("E2E_WORKSPACE", "").strip() or "/tmp/loop-e2e-driver")
    (workspace / "shared").mkdir(parents=True, exist_ok=True)

    arguments = _arguments(args.action, args.project, args.task, workspace)

    # A fresh process reloads persisted state from the object store before
    # acting (best-effort: a task that was never published has no remote
    # state yet, and the action itself re-reads what it needs locally).
    # plan_loop has no task to pre-pull: the projectflow entry point pulls
    # the project meta itself (read-before-read).
    if args.task:
        server._pull_task(arguments, args.task)

    if args.action == "plan_loop":
        if not args.payload.strip():
            print(json.dumps({"ok": False, "error": "plan_loop requires --payload JSON"}))
            return 2
        try:
            payload = json.loads(args.payload)
        except json.JSONDecodeError as exc:
            print(json.dumps({"ok": False, "error": f"--payload is not valid JSON: {exc}"}))
            return 2
        if not isinstance(payload, dict):
            print(json.dumps({"ok": False, "error": "--payload must be a JSON object"}))
            return 2
        payload.setdefault("projectId", args.project)
        arguments["payload"] = payload
    elif args.action == "delegate":
        arguments["payload"] = {
            "projectId": args.project,
            "taskId": args.task,
            "roomId": ROOM_ID,
            "assignedTo": args.worker,
            "title": f"e2e task {args.task}",
            "spec": f"E2E spec for task {args.task} of project {args.project}.",
        }
    elif args.action == "submit":
        result = json.loads(args.result) if args.result.strip() else {}
        if not isinstance(result, dict):
            print(json.dumps({"ok": False, "error": "--result must be a JSON object"}))
            return 2
        arguments["payload"] = {
            "projectId": args.project,
            "taskId": args.task,
            "summary": str(result.get("summary") or f"e2e result for {args.task}"),
            "status": str(result.get("status") or "SUCCESS"),
            "deliverables": [str(item) for item in (result.get("deliverables") or [])],
        }
    else:
        arguments["payload"] = {
            "projectId": args.project,
            "taskId": args.task,
        }

    response = server.call_tool(_TOOL_FOR_ACTION.get(args.action, "taskflow"), arguments)
    payload = json.loads(response["content"][0]["text"])
    if pause_injection is not None:
        payload["pause_injection"] = pause_injection
    print(json.dumps(payload, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
