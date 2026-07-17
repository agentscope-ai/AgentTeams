#!/usr/bin/env python3
"""Run protocol characterization cases against CoPaw domain and TeamHarness MCP.

Usage:
  python plugins/tests/protocol/run_characterization.py
  python plugins/tests/protocol/run_characterization.py --case dag-delegate-flow
  python plugins/tests/protocol/run_characterization.py --engine teamharness-mcp
  python plugins/tests/protocol/run_characterization.py --update
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import asdict
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[3]
COPAW_SRC = REPO_ROOT / "copaw" / "src"
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"
FIXTURES = Path(__file__).resolve().parent / "fixtures" / "cases"
ENGINES = ("copaw-domain", "teamharness-mcp")


def _ensure_copaw_import() -> None:
    src = str(COPAW_SRC)
    protocol_src = str(REPO_ROOT / "shared" / "python" / "agentteams_protocol" / "src")
    for path in (src, protocol_src):
        if path not in sys.path:
            sys.path.insert(0, path)


def _ensure_mcp_import() -> None:
    mcp = str(MCP_DIR)
    plugins = str(MCP_DIR.parents[1])
    protocol_src = str(REPO_ROOT / "shared" / "python" / "agentteams_protocol" / "src")
    for path in (mcp, plugins, protocol_src):
        if path not in sys.path:
            sys.path.insert(0, path)


def _load_task_module():
    _ensure_copaw_import()
    import copaw_worker.task as task  # noqa: WPS433 — intentional dynamic import

    return task


def _load_mcp_tools(*, mock_sync: bool = False):
    _ensure_mcp_import()
    import _bootstrap  # noqa: F401
    import mcp_common  # noqa: WPS433
    from tools.projectflow import projectflow  # noqa: WPS433
    from tools.taskflow import taskflow  # noqa: WPS433

    if mock_sync:
        mcp_common._sync_task = lambda *args, **kwargs: True  # type: ignore[method-assign]
        mcp_common._pull_task = lambda *args, **kwargs: True  # type: ignore[method-assign]
        mcp_common._publish_task_artifacts = lambda *args, **kwargs: []  # type: ignore[method-assign]

    return projectflow, taskflow


def _snapshot_state(task_mod, store, project_id: str, task_id: str | None) -> dict[str, Any]:
    out: dict[str, Any] = {
        "project_meta": asdict(store.read_project_meta(project_id)),
        "project_plan": store.read_project_plan(project_id),
    }
    if task_id and (store.shared_dir / "tasks" / task_id / "meta.json").is_file():
        out["task_meta"] = asdict(store.read_task_meta(task_id))
        spec_path = store.shared_dir / "tasks" / task_id / "spec.md"
        out["task_spec"] = spec_path.read_text(encoding="utf-8") if spec_path.is_file() else None
        result_path = store.shared_dir / "tasks" / task_id / "result.md"
        out["task_result"] = (
            result_path.read_text(encoding="utf-8") if result_path.is_file() else None
        )
    else:
        out["task_meta"] = None
        out["task_spec"] = None
        out["task_result"] = None
    return out


def _normalize_snapshot(data: dict[str, Any]) -> dict[str, Any]:
    """Drop volatile timestamps for stable golden comparison."""
    normalized = json.loads(json.dumps(data))
    for key in ("project_meta", "task_meta"):
        block = normalized.get(key)
        if not isinstance(block, dict):
            continue
        for ts_field in ("createdAt", "created_at", "assignedAt", "assigned_at",
                         "acknowledgedAt", "acknowledged_at", "submittedAt", "submitted_at"):
            if ts_field in block and block[ts_field]:
                block[ts_field] = "<ISO8601>"
    plan = normalized.get("project_plan")
    if isinstance(plan, str):
        normalized["project_plan"] = re.sub(
            r"(\*\*Created\*\*: ).*",
            r"\1<ISO8601>",
            plan,
        )
    return normalized


def _snapshot_mcp_state(workspace: Path, project_id: str, task_id: str | None) -> dict[str, Any]:
    project_dir = workspace / "shared" / "projects" / project_id
    project_meta_path = project_dir / "meta.json"
    project_meta = json.loads(project_meta_path.read_text(encoding="utf-8")) if project_meta_path.is_file() else None
    plan_path = project_dir / "plan.md"
    out: dict[str, Any] = {
        "project_meta": project_meta,
        "project_plan": plan_path.read_text(encoding="utf-8") if plan_path.is_file() else None,
    }
    if task_id:
        task_dir = workspace / "shared" / "tasks" / task_id
        meta_path = task_dir / "meta.json"
        out["task_meta"] = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.is_file() else None
        spec_path = task_dir / "spec.md"
        out["task_spec"] = spec_path.read_text(encoding="utf-8") if spec_path.is_file() else None
        result_path = task_dir / "result.md"
        out["task_result"] = result_path.read_text(encoding="utf-8") if result_path.is_file() else None
    else:
        out["task_meta"] = None
        out["task_spec"] = None
        out["task_result"] = None
    return out


def _normalize_mcp_snapshot(data: dict[str, Any]) -> dict[str, Any]:
    normalized = _normalize_snapshot(data)
    plan = normalized.get("project_plan")
    if isinstance(plan, str):
        normalized["project_plan"] = re.sub(
            r"(# .*\n\n- Project ID:.*\n- Status:.*\n(?:- Plan Type:.*\n)?)",
            r"\1",
            plan,
        )
    return normalized


def _run_mcp_step(projectflow, taskflow, workspace: Path, step: dict[str, Any], ctx: dict[str, Any]) -> None:
    op = step["op"]
    args = step.get("args", {})
    common = {"workspaceDir": str(workspace), "storage": {"sharedPrefix": "mock/shared"}}
    if op == "create_project":
        result = projectflow({"action": "create_project", **common, **args})
    elif op == "plan_dag":
        result = projectflow({"action": "plan_dag", **common, **args})
    elif op == "delegate_task":
        result = taskflow({"action": "delegate_task", "role": "leader", **common, **args})
    elif op == "ack_task":
        result = taskflow({"action": "ack_task", "role": "worker", **common, "taskId": args["taskId"]})
    elif op == "submit_task":
        payload = args.get("result") or {}
        deliverable = (payload.get("deliverables") or [None])[0]
        task_dir = workspace / "shared" / "tasks" / args["taskId"]
        task_dir.mkdir(parents=True, exist_ok=True)
        if deliverable:
            rel = deliverable.split("/", 3)[-1] if deliverable.startswith("shared/tasks/") else "deliverable.md"
            (task_dir / rel).write_text("fixture deliverable\n", encoding="utf-8")
        result = taskflow({
            "action": "submit_task",
            "role": "worker",
            **common,
            "taskId": args["taskId"],
            "status": payload.get("status", "SUCCESS"),
            "summary": payload.get("summary", ""),
            "deliverables": payload.get("deliverables") or [],
        })
    else:
        raise ValueError(f"unsupported op: {op}")
    if not result.get("ok"):
        raise RuntimeError(f"MCP step {op} failed: {result.get('error')}")


def _run_step(task_mod, store, step: dict[str, Any], ctx: dict[str, Any]) -> None:
    op = step["op"]
    args = step.get("args", {})
    TaskResult = task_mod.TaskResult

    if op == "create_project":
        task_mod.create_project(
            store,
            project_id=args["projectId"],
            title=args["title"],
            source=args.get("source"),
            requester=args.get("requester"),
            parent_task_id=args.get("parentTaskId"),
        )
    elif op == "plan_dag":
        task_mod.plan_dag(
            store,
            project_id=args["projectId"],
            tasks=args["tasks"],
        )
    elif op == "delegate_task":
        task_mod.delegate_task(
            store,
            project_id=args["projectId"],
            task_id=args["taskId"],
            spec=args["spec"],
            room_id=args.get("roomId"),
        )
    elif op == "ack_task":
        task_mod.ack_task(store, task_id=args["taskId"], actor=args.get("actor"))
    elif op == "submit_task":
        result_payload = args.get("result")
        result = None
        if result_payload:
            result = TaskResult(
                status=result_payload["status"],
                summary=result_payload["summary"],
                deliverables=result_payload.get("deliverables") or [],
                notes=result_payload.get("notes") or [],
            )
        task_mod.submit_task(
            store,
            task_id=args["taskId"],
            result=result,
            actor=args.get("actor"),
        )
    else:
        raise ValueError(f"unsupported op: {op}")


def _run_case(
    case_dir: Path,
    *,
    engine: str,
    update: bool = False,
) -> list[str]:
    actions_path = case_dir / "actions.json"
    spec = json.loads(actions_path.read_text(encoding="utf-8"))
    project_id = spec["projectId"]
    task_id = spec.get("taskId")
    snapshot_dir = case_dir / "snapshots" / engine

    workspace = case_dir / f"_runtime_workspace_{engine.replace('-', '_')}"
    if workspace.exists():
        import shutil

        shutil.rmtree(workspace)
    workspace.mkdir(parents=True)

    errors: list[str] = []
    for step in spec["steps"]:
        if engine == "copaw-domain":
            if step is spec["steps"][0]:
                task_mod = _load_task_module()
                store = task_mod.FileSystemTaskStore(workspace)
            _run_step(task_mod, store, step, spec)
            actual = _normalize_snapshot(_snapshot_state(task_mod, store, project_id, task_id))
        elif engine == "teamharness-mcp":
            if step is spec["steps"][0]:
                projectflow, taskflow = _load_mcp_tools(mock_sync=True)
            _run_mcp_step(projectflow, taskflow, workspace, step, spec)
            actual = _normalize_mcp_snapshot(_snapshot_mcp_state(workspace, project_id, task_id))
        else:
            raise ValueError(f"unknown engine: {engine}")

        name = step["name"]
        golden_path = snapshot_dir / f"{name}.json"
        if update:
            snapshot_dir.mkdir(parents=True, exist_ok=True)
            golden_path.write_text(
                json.dumps(actual, indent=2, ensure_ascii=False) + "\n",
                encoding="utf-8",
            )
            continue
        if not golden_path.is_file():
            errors.append(f"{case_dir.name}/{engine}/{name}: missing golden {golden_path}")
            continue
        expected = json.loads(golden_path.read_text(encoding="utf-8"))
        if actual != expected:
            errors.append(
                f"{case_dir.name}/{engine}/{name}: snapshot mismatch\n"
                f"  expected: {golden_path}\n"
                f"  re-run with --update --engine {engine} after reviewing change",
            )
    return errors


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", help="Run a single case directory name")
    parser.add_argument(
        "--engine",
        choices=ENGINES,
        help="Run one engine only (default: all engines)",
    )
    parser.add_argument(
        "--update",
        action="store_true",
        help="Write golden snapshots (review diff before commit)",
    )
    args = parser.parse_args()

    if args.case:
        cases = [FIXTURES / args.case]
    else:
        cases = sorted(p for p in FIXTURES.iterdir() if p.is_dir())

    engines = (args.engine,) if args.engine else ENGINES
    all_errors: list[str] = []
    for case_dir in cases:
        if not (case_dir / "actions.json").is_file():
            continue
        for engine in engines:
            all_errors.extend(_run_case(case_dir, engine=engine, update=args.update))

    if args.update:
        print(f"Updated snapshots for {len(cases)} case(s), engine(s): {', '.join(engines)}")
        return 0

    if all_errors:
        for err in all_errors:
            print(err, file=sys.stderr)
        return 1

    print(f"OK: {len(cases)} case(s) matched golden snapshots for {', '.join(engines)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
