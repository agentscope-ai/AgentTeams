"""Local taskflow state machine for HiClaw CoPaw agents."""

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
    "~": "in_progress",
    "x": "completed",
    "!": "blocked",
    "\u2192": "revision",
}
STATUS_TO_MARKER = {value: key for key, value in MARKER_TO_STATUS.items()}
RESULT_STATUSES = {"SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED"}


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
    meta = ProjectMeta(
        project_id=_safe_id(project_id),
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


def ready_tasks(store: TaskStore, *, project_id: str) -> list[DagTask]:
    """Return pending tasks whose dependencies are all completed."""
    tasks = parse_dag_tasks(store.read_project_plan(project_id))
    validate_dag(tasks)
    completed = {task.task_id for task in tasks if task.status == "completed"}
    return [
        task
        for task in tasks
        if task.status == "pending" and all(dep in completed for dep in task.depends_on)
    ]


def assign_task(
    store: TaskStore,
    *,
    project_id: str,
    task_id: str,
    spec: str,
    room_id: str | None = None,
) -> TaskMeta:
    """Create task meta/spec for a ready DAG item and mark it in progress."""
    if not spec or not spec.strip():
        raise TaskflowError("spec is required")

    plan = store.read_project_plan(project_id)
    tasks = parse_dag_tasks(plan)
    task = _find_task(tasks, task_id)
    if task.status != "pending":
        raise TaskflowError(f"task {task_id} is not pending")
    completed = {item.task_id for item in tasks if item.status == "completed"}
    missing = [dep for dep in task.depends_on if dep not in completed]
    if missing:
        raise TaskflowError(f"task {task_id} is blocked by: {', '.join(missing)}")

    meta = TaskMeta(
        task_id=task.task_id,
        project_id=project_id,
        task_title=task.title,
        assigned_to=task.assigned_to,
        room_id=room_id,
        status="assigned",
        depends_on=task.depends_on,
        assigned_at=_now(),
    )
    store.write_task_meta(meta)
    store.write_task_spec(task.task_id, spec)
    updated = _replace_task_status(tasks, task.task_id, "in_progress")
    store.write_project_plan(project_id, replace_dag_tasks(plan, updated))
    return meta


def complete_task(
    store: TaskStore,
    *,
    project_id: str,
    task_id: str,
) -> TaskResult:
    """Complete a graph task after the Leader has synced and reviewed result.md."""
    result = store.read_task_result(task_id)
    if result.status not in {"SUCCESS", "SUCCESS_WITH_NOTES"}:
        raise TaskflowError(f"cannot complete task with result status: {result.status}")

    plan = store.read_project_plan(project_id)
    tasks = parse_dag_tasks(plan)
    task = _find_task(tasks, task_id)
    if task.status != "in_progress":
        raise TaskflowError(f"task {task_id} is not in progress")
    updated = _replace_task_status(tasks, task_id, "completed")
    store.write_project_plan(project_id, replace_dag_tasks(plan, updated))
    return result


def ack_task(store: TaskStore, *, task_id: str) -> TaskMeta:
    """Mark a local task as acknowledged/in progress without touching graph."""
    meta = store.read_task_meta(task_id)
    meta.status = "in_progress"
    meta.acknowledged_at = meta.acknowledged_at or _now()
    store.write_task_meta(meta)
    return meta


def submit_task(
    store: TaskStore,
    *,
    task_id: str,
    result: TaskResult | None = None,
) -> TaskMeta:
    """Mark a local task submitted after result.md exists and is valid."""
    if result is not None:
        store.write_task_result(task_id, result)
    else:
        store.read_task_result(task_id)
    meta = store.read_task_meta(task_id)
    meta.status = "submitted"
    meta.submitted_at = _now()
    store.write_task_meta(meta)
    return meta


def parse_dag_tasks(plan: str) -> list[DagTask]:
    """Parse DAG task lines from a project plan."""
    tasks: list[DagTask] = []
    for line in (plan or "").splitlines():
        task = _parse_dag_line(line)
        if task:
            tasks.append(task)
    return tasks


def replace_dag_tasks(plan: str, tasks: list[DagTask]) -> str:
    """Replace the DAG task section while preserving project header/suffix."""
    lines = (plan or "").splitlines()
    heading_index = _dag_heading_index(lines)
    if heading_index is None:
        lines.extend(["", "## DAG Task Plan"])
        heading_index = len(lines) - 1

    suffix_index = len(lines)
    for idx in range(heading_index + 1, len(lines)):
        if lines[idx].startswith("## ") and lines[idx].strip() != "## DAG Task Plan":
            suffix_index = idx
            break

    prefix = lines[: heading_index + 1]
    suffix = lines[suffix_index:]
    rendered = [render_dag_task(task) for task in tasks]
    new_lines = prefix + [""] + rendered
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
        f"**Status**: {meta.status}\n"
        f"**Created**: {meta.created_at}\n\n"
        "## DAG Task Plan\n"
    )


def _dag_heading_index(lines: list[str]) -> int | None:
    for idx, line in enumerate(lines):
        if line.strip() == "## DAG Task Plan":
            return idx
    return None


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
