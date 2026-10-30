"""Runtime e2e: the #1431 pause/resume closed loop through the real paths.

Drives the real Controller HTTP API (project create / replan / pause /
resume / workflow / task inspection) against the object store, and the
real TeamHarness taskflow in separate ``loop_driver.py`` processes (only
the two Matrix notifiers are stubbed there).  Covers the maintainer's
three points:

1. pause blocks new dispatch — a fresh process delegating a ready task
   fails with "paused", records no notification, and the workflow shows
   the task unassigned with ``next == []`` (no downstream / next-round
   release);
2. in-flight work still saves its result after the pause — an assigned
   task's real submission lands in MinIO and is read back through the
   Controller's task-inspection endpoint;
3. authorized resume + a fresh process continues exactly once — the
   ready task is delegated once (one send record), a second fresh
   process reuses the recorded event instead of re-sending, and the
   already-submitted task is not re-dispatched.

Skipped entirely unless ``E2E_CONTROLLER_URL`` is set AND
``E2E_MINIO=1`` (the CI ``loop-pause-e2e`` job provides both, plus the
``AGENTTEAMS_FS_*`` env the driver's real ``mc`` path needs — see the
README in this directory).
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

import pytest

E2E_DIR = Path(__file__).resolve().parent
DRIVER = E2E_DIR / "loop_driver.py"
BASE_URL = os.environ.get("E2E_CONTROLLER_URL", "").strip().rstrip("/")
TOKEN = os.environ.get("E2E_CONTROLLER_TOKEN", "").strip()

pytestmark = pytest.mark.skipif(
    not BASE_URL or os.environ.get("E2E_MINIO", "").strip() != "1",
    reason="e2e requires a running controller + MinIO (E2E_CONTROLLER_URL, E2E_MINIO=1)",
)


# --- Controller API (stdlib urllib only) ---------------------------------


def _api(method: str, path: str, body: dict[str, Any] | None = None) -> tuple[int, dict[str, Any]]:
    data = json.dumps(body).encode("utf-8") if body is not None else None
    request = urllib.request.Request(BASE_URL + path, data=data, method=method)
    request.add_header("Content-Type", "application/json")
    if TOKEN:
        request.add_header("Authorization", f"Bearer {TOKEN}")
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            status = response.status
            raw = response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code
        raw = exc.read()
    except Exception as exc:  # URLError / timeout -> the controller is unreachable
        raise AssertionError(f"controller {method} {path} unreachable: {exc}") from exc
    try:
        payload = json.loads(raw.decode("utf-8")) if raw else {}
    except json.JSONDecodeError:
        payload = {"raw": raw.decode("utf-8", errors="replace")}
    return status, payload


# --- driver subprocess + sent-record helpers -----------------------------


def _run_driver(
    env: dict[str, str],
    sent_path: Path,
    action: str,
    project_id: str,
    task_id: str,
    worker: str,
    workspace: Path,
    result: dict[str, Any] | None = None,
) -> dict[str, Any]:
    env = dict(env)
    env["E2E_WORKSPACE"] = str(workspace)
    command = [
        sys.executable,
        str(DRIVER),
        "--action",
        action,
        "--project",
        project_id,
        "--task",
        task_id,
        "--worker",
        worker,
    ]
    if result is not None:
        command += ["--result", json.dumps(result)]
    try:
        proc = subprocess.run(command, capture_output=True, text=True, timeout=300, env=env)
    except subprocess.TimeoutExpired as exc:
        raise AssertionError(
            f"driver {action} (task {task_id}) timed out\nsent: {_sent_tail(sent_path)}"
        ) from exc
    if proc.returncode != 0:
        raise AssertionError(
            f"driver {action} (task {task_id}) exited {proc.returncode}\n"
            f"stdout: {proc.stdout}\nstderr: {proc.stderr}\n"
            f"last sent records: {_sent_tail(sent_path)}"
        )
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError:
        raise AssertionError(
            f"driver {action} (task {task_id}) printed non-JSON stdout\n"
            f"stdout: {proc.stdout}\nstderr: {proc.stderr}\n"
            f"last sent records: {_sent_tail(sent_path)}"
        ) from None


def _sent_records(sent_path: Path) -> list[dict[str, Any]]:
    if not sent_path.exists():
        return []
    return [json.loads(line) for line in sent_path.read_text(encoding="utf-8").splitlines() if line.strip()]


def _sent_tail(sent_path: Path, limit: int = 5) -> str:
    return json.dumps(_sent_records(sent_path)[-limit:], ensure_ascii=False)


def _sent_counts(sent_path: Path, kind: str) -> dict[str, int]:
    counts: dict[str, int] = {}
    for record in _sent_records(sent_path):
        if record.get("kind") == kind:
            task_id = str(record.get("task_id") or "")
            counts[task_id] = counts.get(task_id, 0) + 1
    return counts


def _assert_sent_counts(sent_path: Path, kind: str, expected: dict[str, int]) -> None:
    counts = _sent_counts(sent_path, kind)
    for task_id, count in expected.items():
        assert counts.get(task_id, 0) == count, (
            f"{kind} send counts {counts} do not match expected {expected} "
            f"(task {task_id} sent {counts.get(task_id, 0)} times, want {count})\n"
            f"records: {json.dumps(_sent_records(sent_path), ensure_ascii=False)}"
        )


# --- workflow helpers ------------------------------------------------------


def _node_status(workflow: dict[str, Any]) -> dict[str, str]:
    return {
        node.get("id"): str(node.get("status") or "")
        for node in workflow.get("nodes", [])
        if isinstance(node, dict)
    }


def _assert_nodes(workflow: dict[str, Any], expected: dict[str, str]) -> None:
    nodes = _node_status(workflow)
    for task_id, status in expected.items():
        assert nodes.get(task_id) == status, (
            f"node {task_id} status {nodes.get(task_id)!r} != expected {status!r} "
            f"(nodes: {nodes}, workflow: {json.dumps(workflow, ensure_ascii=False)[:2000]})"
        )


def _find_interrupt(workflow: dict[str, Any], interrupt_id: str, value: str) -> dict[str, Any] | None:
    for interrupt in workflow.get("interrupts", []):
        if isinstance(interrupt, dict) and interrupt.get("id") == interrupt_id and interrupt.get("value") == value:
            return interrupt
    return None


# --- the scenario ----------------------------------------------------------


@pytest.fixture
def e2e_env(tmp_path: Path) -> tuple[dict[str, str], Path]:
    """Driver env: inherited object-store env + sent-log + dummy Matrix env
    (the stubs intercept before any HTTP call; the env only arms the send
    path inside the real taskflow code)."""
    sent_path = tmp_path / "sent.jsonl"
    env = dict(os.environ)
    env["E2E_SENT_JSONL"] = str(sent_path)
    # 127.0.0.1:9 (discard port, nothing listens): any code path that is NOT
    # intercepted by the stubs fails deterministically and instantly with
    # ECONNREFUSED instead of depending on DNS NXDOMAIN for
    # "matrix.e2e.invalid" (submit's artifact publish attempts a Matrix
    # upload before the send path, and a slow DNS failure would hang it).
    env.setdefault("AGENTTEAMS_MATRIX_URL", "http://127.0.0.1:9")
    env.setdefault("AGENTTEAMS_WORKER_MATRIX_TOKEN", "e2e-dummy-token")
    return env, sent_path


def test_pause_resume_closed_loop(e2e_env: tuple[dict[str, str], Path], tmp_path: Path) -> None:
    env, sent_path = e2e_env
    project_id = f"e2e-pause-{uuid.uuid4().hex[:8]}"
    worker = "@e2e-worker:matrix.e2e.invalid"

    # --- 1. project + task graph through the real Controller API ----------
    status, project = _api("POST", "/api/v1/projects", {
        "project_id": project_id,
        "title": "loop e2e pause/resume closed loop",
        "source": "e2e",
        "requester": "e2e",
    })
    assert status == 201, f"create project -> {status}: {project}"
    status, replan = _api("POST", f"/api/v1/projects/{project_id}/replan", {
        "tasks": [
            {"taskId": "A", "title": "task A"},
            {"taskId": "B", "title": "task B", "dependsOn": ["A"]},
        ],
    })
    assert status == 200, f"replan -> {status}: {replan}"
    _assert_nodes(replan, {"A": "pending", "B": "pending"})
    assert replan.get("status") == "active", f"replan workflow status: {replan.get('status')}"
    assert replan.get("next") == ["A"], (
        f"expected next=['A'] (A ready, B blocked on A) before the pause, "
        f"got {replan.get('next')}: {json.dumps(replan, ensure_ascii=False)[:2000]}"
    )

    # --- 2. delegate B BEFORE the pause (fresh driver process) ------------
    result = _run_driver(env, sent_path, "delegate", project_id, "B", worker, tmp_path / "ws1")
    assert result.get("ok") is True, f"pre-pause delegate B: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("sent") is True, f"pre-pause delegate B: {result}"
    _assert_sent_counts(sent_path, "delegate", {"B": 1})

    # --- 3. pause through the real Controller API --------------------------
    status, paused_wf = _api("POST", f"/api/v1/projects/{project_id}/pause", {"reason": "e2e pause"})
    assert status == 200, f"pause -> {status}: {paused_wf}"
    assert paused_wf.get("status") == "paused", f"pause response status: {paused_wf.get('status')}"
    assert paused_wf.get("next") == [], (
        f"paused workflow must release nothing, next={paused_wf.get('next')}: "
        f"{json.dumps(paused_wf, ensure_ascii=False)[:2000]}"
    )
    interrupt = _find_interrupt(paused_wf, "project", "paused")
    assert interrupt is not None, f"paused workflow missing project interrupt: {paused_wf.get('interrupts')}"
    assert interrupt.get("action_request", {}).get("action") == "resume", (
        f"pause interrupt action_request: {interrupt.get('action_request')}"
    )

    # --- 4. pause blocks NEW dispatch (fresh driver process) ---------------
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws2")
    assert result.get("ok") is False, f"delegate A while paused must fail: {result}"
    assert "paused" in str(result.get("error")), f"delegate A while paused error: {result.get('error')}"
    _assert_sent_counts(sent_path, "delegate", {"B": 1})  # no new delegate send
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    _assert_nodes(wf, {"A": "pending", "B": "delegated"})
    assert wf.get("next") == [], (
        f"paused workflow must not release downstream / next-round work: "
        f"next={wf.get('next')}: {json.dumps(wf, ensure_ascii=False)[:2000]}"
    )

    # --- 5. in-flight work STILL saves its result under the pause ----------
    task_dir = tmp_path / "ws3" / "shared" / "tasks" / "B"
    task_dir.mkdir(parents=True)
    (task_dir / "result.md").write_text(
        "# Task B result\n\ne2e submission under pause.\n", encoding="utf-8"
    )
    (task_dir / "out.txt").write_text("e2e deliverable for B\n", encoding="utf-8")
    result = _run_driver(
        env,
        sent_path,
        "submit",
        project_id,
        "B",
        worker,
        tmp_path / "ws3",
        result={
            "summary": "task B finished under pause",
            "status": "SUCCESS",
            "deliverables": ["shared/tasks/B/out.txt"],
        },
    )
    assert result.get("ok") is True, f"submit B while paused: {result}\nsent: {_sent_tail(sent_path)}"
    # The result is persisted to MinIO — read it back through the
    # Controller's task-inspection endpoint (MinIO TaskMeta).
    status, inspection = _api("GET", f"/api/v1/projects/{project_id}/tasks/B")
    assert status == 200, f"task B inspection -> {status}: {inspection}"
    assert inspection.get("status") == "submitted", f"task B status: {json.dumps(inspection, ensure_ascii=False)[:2000]}"
    assert inspection.get("result_status") == "SUCCESS", f"task B result_status: {inspection.get('result_status')}"
    assert inspection.get("summary") == "task B finished under pause", f"task B summary: {inspection.get('summary')}"
    assert inspection.get("result_path") == "shared/tasks/B/result.md", f"task B result_path: {inspection.get('result_path')}"
    assert inspection.get("submission_id"), f"task B submission_id missing: {inspection}"
    history_actions = [str(entry.get("action") or "") for entry in inspection.get("history", [])]
    assert "submit_task" in history_actions, f"task B history missing submit_task: {history_actions}"
    _assert_sent_counts(sent_path, "completion", {"B": 1})
    # The project-meta write-back must NOT clobber the paused state.
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    assert wf.get("status") == "paused", f"submit must keep the project paused: {wf.get('status')}"
    _assert_nodes(wf, {"A": "pending", "B": "in-progress"})

    # --- 6. authorized resume through the real Controller API --------------
    status, resumed_wf = _api("POST", f"/api/v1/projects/{project_id}/resume", {})
    assert status == 200, f"resume -> {status}: {resumed_wf}"
    assert resumed_wf.get("status") == "active", f"resume response status: {resumed_wf.get('status')}"

    # --- 7. a FRESH process continues exactly once --------------------------
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws4")
    assert result.get("ok") is True, f"post-resume delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("reused") is not True, (
        f"post-resume delegate A must be a fresh send: {result.get('notification')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    _assert_nodes(wf, {"A": "delegated", "B": "in-progress"})
    assert wf.get("next") == [], f"post-resume next should be empty (B waits on A): {wf.get('next')}"

    # --- 8. a second fresh process REUSES the event (no second send) --------
    result = _run_driver(env, sent_path, "delegate", project_id, "A", worker, tmp_path / "ws5")
    assert result.get("ok") is True, f"retry delegate A: {result}\nsent: {_sent_tail(sent_path)}"
    assert result.get("notification", {}).get("reused") is True, (
        f"retry of an assigned task must reuse the recorded event: {result.get('notification')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})  # A still exactly once

    # --- 9. completed work is NOT re-dispatched (fresh process) -------------
    result = _run_driver(env, sent_path, "delegate", project_id, "B", worker, tmp_path / "ws6")
    assert result.get("ok") is False, f"delegate submitted B must fail: {result}"
    assert "submitted" in str(result.get("error")), (
        f"delegate submitted B error: {result.get('error')}"
    )
    _assert_sent_counts(sent_path, "delegate", {"A": 1, "B": 1})
    status, wf = _api("GET", f"/api/v1/projects/{project_id}/workflow")
    assert status == 200, f"workflow -> {status}: {wf}"
    # Only A advanced (once); B is still the in-flight submission.
    _assert_nodes(wf, {"A": "delegated", "B": "in-progress"})

    # --- 10. check_task on the submitted task (fresh process, real path) ----
    result = _run_driver(env, sent_path, "status", project_id, "B", worker, tmp_path / "ws7")
    assert result.get("ok") is True, f"check B: {result}"
    assert result.get("effective") is True, f"check B effective: {result.get('effective')}"
    assert result.get("result", {}).get("status") == "SUCCESS", f"check B result: {result.get('result')}"
