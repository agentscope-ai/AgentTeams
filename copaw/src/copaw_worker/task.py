"""Local taskflow state machine for AgentTeams CoPaw agents."""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
import json
from pathlib import Path
import re
from typing import Any, Protocol


class TaskflowError(ValueError):
    """Expected user-facing taskflow error."""


MARKER_TO_STATUS = {
    " ": "pending",
    "~": "delegated",
    "x": "completed",
    "!": "blocked",
    "\u2192": "revision",
}
STATUS_TO_MARKER = {value: key for key, value in MARKER_TO_STATUS.items()}
RESULT_STATUSES = {
    "SUCCESS",
    "SUCCESS_WITH_NOTES",
    "REVISION_NEEDED",
    "BLOCKED",
    "INTERRUPTED",
}
EFFECTIVE_RESULT_STATUSES = {"SUCCESS", "SUCCESS_WITH_NOTES"}


@dataclass(frozen=True)
class ProjectMeta:
    project_id: str
    title: str
    status: str = "active"
    source: str | None = None
    requester: str | None = None
    parent_task_id: str | None = None
    created_at: str | None = None


@dataclass(frozen=True)
class DagTask:
    task_id: str
    title: str
    assigned_to: str
    depends_on: list[str] = field(default_factory=list)
    status: str = "pending"


@dataclass(frozen=True)
class LoopPlan:
    goal: str
    stop_condition: str
    iteration_template: str
    max_iterations: int
    current_iteration: int = 0
    status: str = "running"
    tasks: list[DagTask] = field(default_factory=list)
    history: list[str] = field(default_factory=list)


@dataclass
class TaskMeta:
    task_id: str
    project_id: str
    task_title: str
    assigned_to: str
    room_id: str | None = None
    status: str = "assigned"
    depends_on: list[str] = field(default_factory=list)
    assigned_at: str | None = None
    acknowledged_at: str | None = None
    submitted_at: str | None = None


@dataclass(frozen=True)
class TaskResult:
    status: str
    summary: str
    deliverables: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)


class TaskStore(Protocol):
    """Storage interface for taskflow; implementations decide persistence."""

    def read_project_meta(self, project_id: str) -> ProjectMeta: ...
    def write_project_meta(self, meta: ProjectMeta) -> None: ...
    def read_project_plan(self, project_id: str) -> str: ...
    def write_project_plan(self, project_id: str, plan: str) -> None: ...
    def read_task_meta(self, task_id: str) -> TaskMeta: ...
    def write_task_meta(self, meta: TaskMeta) -> None: ...
    def read_task_spec(self, task_id: str) -> str: ...
    def write_task_spec(self, task_id: str, spec: str) -> None: ...
    def read_task_result(self, task_id: str) -> TaskResult: ...
    def write_task_result(self, task_id: str, result: TaskResult) -> None: ...


class FileSystemTaskStore:
    """TaskStore implementation backed by local shared/ files."""

    def __init__(self, workspace_dir: Path | str | None = None) -> None:
        self.workspace_dir = Path(workspace_dir) if workspace_dir else Path.cwd()
        self.shared_dir = self.workspace_dir / "shared"

    def _project_dir(self, project_id: str) -> Path:
        return self.shared_dir / "projects" / _safe_id(project_id)

    def _task_dir(self, task_id: str) -> Path:
        return self.shared_dir / "tasks" / _safe_id(task_id)

    def read_project_meta(self, project_id: str) -> ProjectMeta:
        path = self._project_dir(project_id) / "meta.json"
        data = _read_json(path)
        return ProjectMeta(
            project_id=str(data["project_id"]),
            title=str(data["title"]),
            status=str(data.get("status") or "active"),
            source=data.get("source"),
            requester=data.get("requester"),
            parent_task_id=data.get("parent_task_id"),
            created_at=data.get("created_at"),
        )

    def write_project_meta(self, meta: ProjectMeta) -> None:
        path = self._project_dir(meta.project_id) / "meta.json"
        _write_json(path, _drop_none(asdict(meta)))

    def read_project_plan(self, project_id: str) -> str:
        path = self._project_dir(project_id) / "plan.md"
        if not path.exists():
            raise TaskflowError(f"project plan not found: {path}")
        return path.read_text()

    def write_project_plan(self, project_id: str, plan: str) -> None:
        path = self._project_dir(project_id) / "plan.md"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(plan)

    def read_task_meta(self, task_id: str) -> TaskMeta:
        path = self._task_dir(task_id) / "meta.json"
        data = _read_json(path)
        return TaskMeta(
            task_id=str(data["task_id"]),
            project_id=str(data["project_id"]),
            task_title=str(data["task_title"]),
            assigned_to=str(data["assigned_to"]),
            room_id=data.get("room_id"),
            status=str(data.get("status") or "assigned"),
            depends_on=list(data.get("depends_on") or []),
            assigned_at=data.get("assigned_at"),
            acknowledged_at=data.get("acknowledged_at"),
            submitted_at=data.get("submitted_at"),
        )

    def write_task_meta(self, meta: TaskMeta) -> None:
        path = self._task_dir(meta.task_id) / "meta.json"
        _write_json(path, _drop_none(asdict(meta)))

    def read_task_spec(self, task_id: str) -> str:
        path = self._task_dir(task_id) / "spec.md"
        if not path.exists():
            raise TaskflowError(f"task spec not found: {path}")
        return path.read_text()

    def write_task_spec(self, task_id: str, spec: str) -> None:
        path = self._task_dir(task_id) / "spec.md"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(spec)

    def read_task_result(self, task_id: str) -> TaskResult:
        path = self._task_dir(task_id) / "result.md"
        if not path.exists():
            raise TaskflowError(f"task result not found: {path}")
        result = parse_task_result(path.read_text())
        validate_task_result(task_id, result)
        return result

    def write_task_result(self, task_id: str, result: TaskResult) -> None:
        validate_task_result(task_id, result)
        path = self._task_dir(task_id) / "result.md"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(render_task_result(result))


def create_project(
    store: TaskStore,
    *,
    project_id: str,
    title: str,
    source: str | None = None,
    requester: str | None = None,
    parent_task_id: str | None = None,
) -> ProjectMeta:
    """Create project meta and an empty DAG plan."""
    safe = _safe_id(project_id)
    if _project_exists(store, safe):
        raise TaskflowError(f"project already exists: {safe}")
    meta = ProjectMeta(
        project_id=safe,
        title=title,
        source=source,
        requester=requester,
        parent_task_id=parent_task_id,
        created_at=_now(),
    )
    store.write_project_meta(meta)
    store.write_project_plan(meta.project_id, _initial_plan(meta))
    return meta


def add_tasks(
    store: TaskStore,
    *,
    project_id: str,
    tasks: list[dict[str, Any]],
) -> list[DagTask]:
    """Add or update pending DAG tasks and validate the graph."""
    plan = store.read_project_plan(project_id)
    existing = {task.task_id: task for task in parse_dag_tasks(plan)}
    incoming_ids: set[str] = set()

    for raw in tasks:
        task = _dag_task_from_payload(raw)
        if task.task_id in incoming_ids:
            raise TaskflowError(f"duplicate task id in payload: {task.task_id}")
        incoming_ids.add(task.task_id)

        current = existing.get(task.task_id)
        if current and current.status != "pending":
            raise TaskflowError(
                f"cannot modify non-pending task {task.task_id} ({current.status})",
            )
        existing[task.task_id] = task

    final_tasks = list(existing.values())
    validate_dag(final_tasks)
    store.write_project_plan(project_id, replace_dag_tasks(plan, final_tasks))
    return final_tasks


def plan_dag(
    store: TaskStore,
    *,
    project_id: str,
    tasks: list[dict[str, Any]],
) -> list[DagTask]:
    """Replace the project DAG with the Leader's latest graph shape."""
    plan = store.read_project_plan(project_id)
    existing = {task.task_id: task for task in parse_dag_tasks(plan)}
    incoming_ids: set[str] = set()
    final_tasks: list[DagTask] = []

    for raw in tasks:
        task = _dag_task_from_payload(raw)
        if task.task_id in incoming_ids:
            raise TaskflowError(f"duplicate task id in payload: {task.task_id}")
        incoming_ids.add(task.task_id)

        current = existing.get(task.task_id)
        if current:
            task = DagTask(
                task_id=task.task_id,
                title=task.title,
                assigned_to=task.assigned_to,
                depends_on=task.depends_on,
                status=current.status,
            )
        final_tasks.append(task)

    validate_dag(final_tasks)
    store.write_project_plan(project_id, replace_dag_tasks(plan, final_tasks))
    return final_tasks


def plan_loop(
    store: TaskStore,
    *,
    project_id: str,
    goal: str,
    stop_condition: str,
    iteration_template: str,
    max_iterations: int,
    current_iteration: int = 0,
    status: str = "running",
    tasks: list[dict[str, Any]] | None = None,
) -> LoopPlan:
    """Replace the project execution plan with a Loop plan."""
    if max_iterations < 1:
        raise TaskflowError("maxIterations must be greater than zero")
    if current_iteration < 0:
        raise TaskflowError("currentIteration must be zero or greater")
    if current_iteration > max_iterations:
        raise TaskflowError("currentIteration cannot exceed maxIterations")

    plan = store.read_project_plan(project_id)
    existing_loop = parse_loop_plan(plan)
    existing_tasks = {
        task.task_id: task
        for task in (existing_loop.tasks if existing_loop else parse_loop_tasks(plan))
    }
    final_tasks: list[DagTask] = []
    incoming_ids: set[str] = set()

    for raw in tasks or []:
        task = _dag_task_from_payload(raw)
        if task.task_id in incoming_ids:
            raise TaskflowError(f"duplicate task id in payload: {task.task_id}")
        incoming_ids.add(task.task_id)
        current = existing_tasks.get(task.task_id)
        if current:
            task = DagTask(
                task_id=task.task_id,
                title=task.title,
                assigned_to=task.assigned_to,
                depends_on=task.depends_on,
                status=current.status,
            )
        final_tasks.append(task)

    validate_dag(final_tasks)
    loop = LoopPlan(
        goal=_single_line(goal),
        stop_condition=_single_line(stop_condition),
        iteration_template=iteration_template.strip(),
        max_iterations=max_iterations,
        current_iteration=current_iteration,
        status=_safe_loop_status(status),
        tasks=final_tasks,
        history=existing_loop.history if existing_loop else [],
    )
    store.write_project_plan(project_id, replace_loop_plan(plan, loop))
    return loop


def ready_loop_nodes(store: TaskStore, *, project_id: str) -> list[DagTask]:
    """Return pending current-iteration Loop nodes whose dependencies are accepted."""
    meta = store.read_project_meta(project_id)
    if meta.status == "paused":
        return []

    loop = parse_loop_plan(store.read_project_plan(project_id))
    if loop is None:
        raise TaskflowError(f"project has no loop plan: {project_id}")
    if loop.status in {"completed", "blocked", "waiting_user"}:
        return []

    validate_dag(loop.tasks)
    effective = {
        task.task_id
        for task in loop.tasks
        if task.status == "completed"
    }
    return [
        task
        for task in loop.tasks
        if task.status == "pending" and all(dep in effective for dep in task.depends_on)
    ]


def record_loop_iteration(
    store: TaskStore,
    *,
    project_id: str,
    iteration: int,
    decision: str,
    summary: str,
    next_action: str | None = None,
) -> LoopPlan:
    """Record a Leader decision for one Loop iteration."""
    if iteration < 1:
        raise TaskflowError("iteration must be greater than zero")
    plan = store.read_project_plan(project_id)
    loop = parse_loop_plan(plan)
    if loop is None:
        raise TaskflowError(f"project has no loop plan: {project_id}")
    if iteration > loop.max_iterations:
        raise TaskflowError("iteration cannot exceed maxIterations")

    safe_decision = _safe_loop_decision(decision)
    status = {
        "continue": "running",
        "replan": "running",
        "ask_user": "waiting_user",
        "stop_success": "completed",
        "stop_blocked": "blocked",
    }[safe_decision]
    current_iteration = max(loop.current_iteration, iteration)
    detail = f"Iteration {iteration}: {safe_decision} — {_single_line(summary)}"
    if next_action and next_action.strip():
        detail += f" Next: {_single_line(next_action)}"
    updated = LoopPlan(
        goal=loop.goal,
        stop_condition=loop.stop_condition,
        iteration_template=loop.iteration_template,
        max_iterations=loop.max_iterations,
        current_iteration=current_iteration,
        status=status,
        tasks=loop.tasks,
        history=[*loop.history, detail],
    )
    store.write_project_plan(project_id, replace_loop_plan(plan, updated))
    return updated


def ready_nodes(store: TaskStore, *, project_id: str) -> list[DagTask]:
    """Return pending DAG nodes whose dependencies have been accepted."""
    meta = store.read_project_meta(project_id)
    if meta.status == "paused":
        return []

    plan = store.read_project_plan(project_id)
    if parse_plan_type(plan) != "dag":
        raise TaskflowError(f"project plan is not a DAG: {project_id}")
    tasks = parse_dag_tasks(plan)
    validate_dag(tasks)
    effective = {
        task.task_id
        for task in tasks
        if task.status == "completed"
    }
    return [
        task
        for task in tasks
        if task.status == "pending" and all(dep in effective for dep in task.depends_on)
    ]


def delegate_task(
    store: TaskStore,
    *,
    project_id: str,
    task_id: str,
    spec: str,
    room_id: str | None = None,
) -> TaskMeta:
    """Create task meta/spec for a ready DAG node and mark it delegated."""
    if not spec or not spec.strip():
        raise TaskflowError("spec is required")
    meta = store.read_project_meta(project_id)
    if meta.status == "paused":
        raise TaskflowError(f"project is paused: {project_id}")

    plan = store.read_project_plan(project_id)
    tasks = parse_loop_tasks(plan) if parse_plan_type(plan) == "loop" else parse_dag_tasks(plan)
    task = _find_task(tasks, task_id)
    if task.status != "pending":
        raise TaskflowError(f"task {task_id} is not pending")
    effective = {
        item.task_id
        for item in tasks
        if item.status == "completed"
    }
    missing = [dep for dep in task.depends_on if dep not in effective]
    if missing:
        raise TaskflowError(f"task {task_id} is blocked by: {', '.join(missing)}")

    meta = TaskMeta(
        task_id=task.task_id,
        project_id=project_id,
        task_title=task.title,
        assigned_to=canonical_worker_id(task.assigned_to),
        room_id=room_id,
        status="assigned",
        depends_on=task.depends_on,
        assigned_at=_now(),
    )
    store.write_task_meta(meta)
    store.write_task_spec(task.task_id, spec)
    updated = _replace_task_status(tasks, task.task_id, "delegated")
    if parse_plan_type(plan) == "loop":
        loop = parse_loop_plan(plan)
        if loop is None:
            raise TaskflowError(f"project has no loop plan: {project_id}")
        updated_loop = LoopPlan(
            goal=loop.goal,
            stop_condition=loop.stop_condition,
            iteration_template=loop.iteration_template,
            max_iterations=loop.max_iterations,
            current_iteration=loop.current_iteration,
            status=loop.status,
            tasks=updated,
            history=loop.history,
        )
        store.write_project_plan(project_id, replace_loop_plan(plan, updated_loop))
    else:
        store.write_project_plan(project_id, replace_dag_tasks(plan, updated))
    return meta


def pause_project(store: TaskStore, *, project_id: str) -> ProjectMeta:
    """Pause DAG scheduling by preventing ready nodes from being issued."""
    meta = store.read_project_meta(project_id)
    updated = ProjectMeta(
        project_id=meta.project_id,
        title=meta.title,
        status="paused",
        source=meta.source,
        requester=meta.requester,
        parent_task_id=meta.parent_task_id,
        created_at=meta.created_at,
    )
    store.write_project_meta(updated)
    return updated


def resume_project(store: TaskStore, *, project_id: str) -> ProjectMeta:
    """Resume DAG scheduling without changing the graph."""
    meta = store.read_project_meta(project_id)
    updated = ProjectMeta(
        project_id=meta.project_id,
        title=meta.title,
        status="active",
        source=meta.source,
        requester=meta.requester,
        parent_task_id=meta.parent_task_id,
        created_at=meta.created_at,
    )
    store.write_project_meta(updated)
    return updated


def complete_project(store: TaskStore, *, project_id: str) -> ProjectMeta:
    """Mark a project complete after the Leader has finalized project result files."""
    meta = store.read_project_meta(project_id)
    updated = ProjectMeta(
        project_id=meta.project_id,
        title=meta.title,
        status="completed",
        source=meta.source,
        requester=meta.requester,
        parent_task_id=meta.parent_task_id,
        created_at=meta.created_at,
    )
    store.write_project_meta(updated)
    return updated


def check_task(store: TaskStore, *, task_id: str) -> TaskResult:
    """Read and validate a submitted task result without changing the project DAG."""
    return store.read_task_result(task_id)


def is_effective_result(result: TaskResult) -> bool:
    """Return whether a task result is a candidate for Leader acceptance."""
    return result.status in EFFECTIVE_RESULT_STATUSES


def ack_task(store: TaskStore, *, task_id: str, actor: str | None = None) -> TaskMeta:
    """Mark a local task as acknowledged/in progress without touching graph."""
    meta = store.read_task_meta(task_id)
    _require_assigned_worker(meta, actor)
    _require_task_room(meta)
    meta.status = "in_progress"
    meta.acknowledged_at = meta.acknowledged_at or _now()
    store.write_task_meta(meta)
    return meta


def submit_task(
    store: TaskStore,
    *,
    task_id: str,
    result: TaskResult | None = None,
    actor: str | None = None,
) -> TaskMeta:
    """Mark a local task submitted after result.md exists and is valid."""
    meta = store.read_task_meta(task_id)
    _require_assigned_worker(meta, actor)
    _require_task_room(meta)
    if result is not None:
        store.write_task_result(task_id, result)
    else:
        store.read_task_result(task_id)
    meta.status = "submitted"
    meta.submitted_at = _now()
    store.write_task_meta(meta)
    return meta


def _require_assigned_worker(meta: TaskMeta, actor: str | None) -> None:
    current = canonical_worker_id(actor)
    if not current:
        raise TaskflowError("current worker identity is required")
    assigned = canonical_worker_id(meta.assigned_to)
    if current != assigned:
        raise TaskflowError(
            f"task {meta.task_id} is assigned to {meta.assigned_to}, not {current}",
        )


def _require_task_room(meta: TaskMeta) -> None:
    if not (meta.room_id or "").strip():
        raise TaskflowError(f"task {meta.task_id} is missing room_id")


def canonical_worker_id(value: str | None) -> str:
    """Normalize Matrix/display worker identities to the logical worker name."""
    text = (value or "").strip()
    if not text:
        return ""

    token = text.split()[0].strip()
    token = token.strip("`'\"")
    token = token.removeprefix("@")
    if ":" in token:
        token = token.split(":", 1)[0]
    return token.strip(":,;")


def parse_dag_tasks(plan: str) -> list[DagTask]:
    """Parse DAG task lines from a project plan."""
    tasks: list[DagTask] = []
    for line in (plan or "").splitlines():
        task = _parse_dag_line(line)
        if task:
            tasks.append(task)
    return tasks


def parse_loop_tasks(plan: str) -> list[DagTask]:
    """Parse current-iteration task lines from a Loop plan."""
    lines = (plan or "").splitlines()
    start = _heading_index(lines, "## Current Iteration")
    if start is None:
        return []
    end = len(lines)
    for idx in range(start + 1, len(lines)):
        if lines[idx].startswith("## "):
            end = idx
            break
    tasks: list[DagTask] = []
    for line in lines[start + 1 : end]:
        task = _parse_dag_line(line)
        if task:
            tasks.append(task)
    return tasks


def parse_plan_type(plan: str) -> str:
    for line in (plan or "").splitlines():
        if line.startswith("**Plan Type**:"):
            value = line[len("**Plan Type**:") :].strip().lower()
            if value in {"dag", "loop"}:
                return value
    return "dag"


def parse_loop_plan(plan: str) -> LoopPlan | None:
    lines = (plan or "").splitlines()
    if parse_plan_type(plan) != "loop":
        return None

    status = _plan_field(lines, "Loop Status") or "running"
    iteration_text = _plan_field(lines, "Iteration") or "0 / 1"
    current_iteration, max_iterations = _parse_iteration(iteration_text)
    goal = _plan_field(lines, "Goal") or ""
    stop_condition = _plan_field(lines, "Stop Condition") or ""
    iteration_template = "\n".join(_section_lines(lines, "## Iteration Template")).strip()
    history = [
        line[2:].strip()
        for line in _section_lines(lines, "## Iteration History")
        if line.strip().startswith("- ")
    ]
    return LoopPlan(
        goal=goal,
        stop_condition=stop_condition,
        iteration_template=iteration_template,
        max_iterations=max_iterations,
        current_iteration=current_iteration,
        status=_safe_loop_status(status),
        tasks=parse_loop_tasks(plan),
        history=history,
    )


def replace_dag_tasks(plan: str, tasks: list[DagTask]) -> str:
    """Replace the DAG task section while preserving project header/suffix."""
    lines = (plan or "").splitlines()
    heading_index = _execution_heading_index(lines)
    if heading_index is None:
        lines.extend(["", "## DAG Task Plan"])
        heading_index = len(lines) - 1

    suffix_index = len(lines)
    for idx in range(heading_index + 1, len(lines)):
        if lines[idx].startswith("## ") and lines[idx].strip() not in {
            "## DAG Task Plan",
            "## Loop Plan",
            "## Iteration Template",
            "## Current Iteration",
            "## Iteration History",
        }:
            suffix_index = idx
            break

    prefix = lines[:heading_index]
    suffix = lines[suffix_index:]
    rendered = [render_dag_task(task) for task in tasks]
    new_lines = prefix + ["## DAG Task Plan", "", "**Plan Type**: dag", "", *rendered]
    if suffix:
        new_lines += [""] + suffix
    return "\n".join(new_lines).rstrip() + "\n"


def replace_loop_plan(plan: str, loop: LoopPlan) -> str:
    """Replace the Loop plan section while preserving project header/suffix."""
    lines = (plan or "").splitlines()
    heading_index = _execution_heading_index(lines)
    if heading_index is None:
        heading_index = len(lines)
    suffix_index = len(lines)
    for idx in range(heading_index + 1, len(lines)):
        if lines[idx].startswith("## ") and lines[idx].strip() not in {
            "## DAG Task Plan",
            "## Loop Plan",
            "## Iteration Template",
            "## Current Iteration",
            "## Iteration History",
        }:
            suffix_index = idx
            break

    prefix = lines[:heading_index]
    suffix = lines[suffix_index:]
    rendered = [
        "## Loop Plan",
        "",
        "**Plan Type**: loop",
        f"**Loop Status**: {loop.status}",
        f"**Iteration**: {loop.current_iteration} / {loop.max_iterations}",
        f"**Goal**: {_single_line(loop.goal)}",
        f"**Stop Condition**: {_single_line(loop.stop_condition)}",
        "",
        "## Iteration Template",
        "",
        loop.iteration_template.strip(),
        "",
        "## Current Iteration",
        "",
        *[render_dag_task(task) for task in loop.tasks],
        "",
        "## Iteration History",
        "",
        *[f"- {item}" for item in loop.history],
    ]
    new_lines = prefix + rendered
    if suffix:
        new_lines += [""] + suffix
    return "\n".join(new_lines).rstrip() + "\n"


def render_dag_task(task: DagTask) -> str:
    marker = STATUS_TO_MARKER.get(task.status)
    if marker is None:
        raise TaskflowError(f"unknown task status: {task.status}")
    details = [f"assigned: {task.assigned_to}"]
    if task.depends_on:
        details.append(f"depends: {', '.join(task.depends_on)}")
    return f"- [{marker}] {task.task_id} \u2014 {task.title} ({', '.join(details)})"


def validate_dag(tasks: list[DagTask]) -> None:
    ids = [task.task_id for task in tasks]
    if len(ids) != len(set(ids)):
        raise TaskflowError("duplicate task ids in graph")

    all_ids = set(ids)
    for task in tasks:
        missing = [dep for dep in task.depends_on if dep not in all_ids]
        if missing:
            raise TaskflowError(
                f"task {task.task_id} depends on unknown task(s): {', '.join(missing)}",
            )

    incoming = {task.task_id: len(task.depends_on) for task in tasks}
    outgoing: dict[str, list[str]] = {task.task_id: [] for task in tasks}
    for task in tasks:
        for dep in task.depends_on:
            outgoing[dep].append(task.task_id)

    queue = [task_id for task_id, count in incoming.items() if count == 0]
    visited = 0
    while queue:
        current = queue.pop(0)
        visited += 1
        for child in outgoing[current]:
            incoming[child] -= 1
            if incoming[child] == 0:
                queue.append(child)

    if visited != len(tasks):
        raise TaskflowError("cycle detected in DAG")


def parse_task_result(text: str) -> TaskResult:
    status = ""
    summary = ""
    deliverables: list[str] = []
    notes: list[str] = []
    section = ""

    for raw_line in (text or "").splitlines():
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith("STATUS:"):
            status = line[len("STATUS:") :].strip()
            section = ""
            continue
        if line.startswith("SUMMARY:"):
            summary = line[len("SUMMARY:") :].strip()
            section = ""
            continue
        if line == "DELIVERABLES:":
            section = "deliverables"
            continue
        if line == "NOTES:":
            section = "notes"
            continue
        if line.startswith("- "):
            item = line[2:].strip()
            if section == "deliverables":
                deliverables.append(item)
            elif section == "notes":
                notes.append(item)

    if status not in RESULT_STATUSES:
        raise TaskflowError(f"invalid result status: {status or '<missing>'}")
    if not summary:
        raise TaskflowError("result summary is required")
    return TaskResult(status=status, summary=summary, deliverables=deliverables, notes=notes)


def render_task_result(result: TaskResult) -> str:
    lines = [
        f"STATUS: {result.status}",
        f"SUMMARY: {_single_line(result.summary)}",
        "",
        "DELIVERABLES:",
    ]
    lines.extend(f"- {item}" for item in result.deliverables)
    if result.notes:
        lines.extend(["", "NOTES:"])
        lines.extend(f"- {item}" for item in result.notes)
    return "\n".join(lines).rstrip() + "\n"


def validate_task_result(task_id: str, result: TaskResult) -> None:
    if result.status not in RESULT_STATUSES:
        raise TaskflowError(f"invalid result status: {result.status or '<missing>'}")
    if not result.summary.strip():
        raise TaskflowError("result summary is required")
    prefix = f"shared/tasks/{_safe_id(task_id)}/"
    for path in result.deliverables:
        if not isinstance(path, str) or not path.strip():
            raise TaskflowError("deliverable path must be a non-empty string")
        if not path.startswith(prefix):
            raise TaskflowError(
                f"deliverable must be under {prefix}: {path}",
            )
        parts = Path(path).parts
        if any(part in ("", ".", "..") for part in parts):
            raise TaskflowError(f"invalid deliverable path: {path}")


def _parse_dag_line(line: str) -> DagTask | None:
    match = re.match(
        r"^\s*-\s+\[(?P<marker>[ x~!\u2192])\]\s+"
        r"(?P<id>[A-Za-z0-9_-]+)\s+(?:\u2014|-)\s+"
        r"(?P<title>.*?)(?:\s+\((?P<meta>.*)\))?\s*$",
        line,
    )
    if not match:
        return None

    marker = match.group("marker")
    meta_text = match.group("meta") or ""
    assigned_match = re.search(r"assigned:\s*([^,)]+)", meta_text)
    assigned_to = assigned_match.group(1).strip() if assigned_match else ""
    depends_match = re.search(r"depends:\s*([^)]+)", meta_text)
    depends_on = []
    if depends_match:
        depends_on = [
            dep.strip()
            for dep in depends_match.group(1).split(",")
            if dep.strip()
        ]
    return DagTask(
        task_id=match.group("id"),
        title=match.group("title").strip(),
        assigned_to=assigned_to,
        depends_on=depends_on,
        status=MARKER_TO_STATUS[marker],
    )


def _dag_task_from_payload(payload: dict[str, Any]) -> DagTask:
    task_id = str(payload.get("taskId") or payload.get("task_id") or "").strip()
    title = str(payload.get("title") or "").strip()
    assigned_to = str(payload.get("assignedTo") or payload.get("assigned_to") or "").strip()
    depends_raw = payload.get("dependsOn", payload.get("depends_on", [])) or []
    if not isinstance(depends_raw, list):
        raise TaskflowError(f"dependsOn must be a list for task {task_id or '<missing>'}")
    depends_on = [_safe_id(str(dep)) for dep in depends_raw]
    if not task_id or not title or not assigned_to:
        raise TaskflowError("taskId, title, and assignedTo are required")
    return DagTask(
        task_id=_safe_id(task_id),
        title=title,
        assigned_to=assigned_to,
        depends_on=depends_on,
    )


def _find_task(tasks: list[DagTask], task_id: str) -> DagTask:
    safe_id = _safe_id(task_id)
    for task in tasks:
        if task.task_id == safe_id:
            return task
    raise TaskflowError(f"task not found in project graph: {task_id}")


def _replace_task_status(
    tasks: list[DagTask],
    task_id: str,
    status: str,
) -> list[DagTask]:
    safe_id = _safe_id(task_id)
    return [
        DagTask(
            task_id=task.task_id,
            title=task.title,
            assigned_to=task.assigned_to,
            depends_on=task.depends_on,
            status=status if task.task_id == safe_id else task.status,
        )
        for task in tasks
    ]


def _initial_plan(meta: ProjectMeta) -> str:
    return (
        f"# Team Project: {meta.title}\n\n"
        f"**ID**: {meta.project_id}\n"
        f"**Created**: {meta.created_at}\n\n"
        "## DAG Task Plan\n\n"
        "**Plan Type**: dag\n"
    )


def _dag_heading_index(lines: list[str]) -> int | None:
    for idx, line in enumerate(lines):
        if line.strip() == "## DAG Task Plan":
            return idx
    return None


def _execution_heading_index(lines: list[str]) -> int | None:
    dag_idx = _dag_heading_index(lines)
    loop_idx = _heading_index(lines, "## Loop Plan")
    candidates = [idx for idx in (dag_idx, loop_idx) if idx is not None]
    return min(candidates) if candidates else None


def _heading_index(lines: list[str], heading: str) -> int | None:
    for idx, line in enumerate(lines):
        if line.strip() == heading:
            return idx
    return None


def _plan_field(lines: list[str], field_name: str) -> str | None:
    prefix = f"**{field_name}**:"
    for line in lines:
        if line.startswith(prefix):
            return line[len(prefix) :].strip()
    return None


def _section_lines(lines: list[str], heading: str) -> list[str]:
    start = _heading_index(lines, heading)
    if start is None:
        return []
    end = len(lines)
    for idx in range(start + 1, len(lines)):
        if lines[idx].startswith("## "):
            end = idx
            break
    return lines[start + 1 : end]


def _parse_iteration(value: str) -> tuple[int, int]:
    match = re.match(r"^\s*(\d+)\s*/\s*(\d+)\s*$", value or "")
    if not match:
        raise TaskflowError(f"invalid loop iteration: {value}")
    return int(match.group(1)), int(match.group(2))


def _safe_loop_status(value: str) -> str:
    text = str(value or "").strip().lower()
    allowed = {"running", "waiting_results", "evaluating", "waiting_user", "completed", "blocked"}
    if text not in allowed:
        raise TaskflowError(f"invalid loop status: {value}")
    return text


def _safe_loop_decision(value: str) -> str:
    text = str(value or "").strip().lower()
    allowed = {"continue", "stop_success", "stop_blocked", "ask_user", "replan"}
    if text not in allowed:
        raise TaskflowError(f"invalid loop decision: {value}")
    return text


def _safe_id(value: str) -> str:
    text = str(value or "").strip()
    if not re.fullmatch(r"[A-Za-z0-9_-]+", text):
        raise TaskflowError(f"invalid id: {value}")
    return text


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _single_line(value: str) -> str:
    return re.sub(r"\s+", " ", value).strip()


def _read_json(path: Path) -> dict[str, Any]:
    if not path.exists():
        raise TaskflowError(f"file not found: {path}")
    return json.loads(path.read_text())


def _write_json(path: Path, data: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n")


def _drop_none(data: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in data.items() if value is not None}


def _project_exists(store: TaskStore, project_id: str) -> bool:
    if isinstance(store, FileSystemTaskStore):
        return (store._project_dir(project_id) / "meta.json").exists()

    try:
        store.read_project_meta(project_id)
        return True
    except TaskflowError as exc:
        if "file not found" in str(exc):
            return False
        raise
