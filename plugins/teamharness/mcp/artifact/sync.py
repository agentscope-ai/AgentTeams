# shared task/project state sync and pull (W1-a2: real module)
from __future__ import annotations

from typing import Any

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.io import _read_json
from mcp.common.payload import _first_text
from mcp.common.paths import _project_dir, _project_state_path, _task_dir, _task_state_path

# Call-time lookups (common.refs): monkeypatch-sensitive names resolve
# against the bound mcp/server.py namespace at call time.
_filesync = refs.proxy("_filesync")
_write_json = refs.proxy("_write_json")
_write_project_plan = refs.proxy("_write_project_plan")
_write_task = refs.proxy("_write_task")


def _sync_task(
    arguments: dict[str, Any],
    task_id: str,
    exclude: list[str] | None = None,
    result_paths: list[str] | None = None,
) -> bool:
    if result_paths is not None:
        task_prefix = f"shared/tasks/{task_id}"
        local_task_dir = _task_dir(arguments, task_id)
        payload_paths: list[str] = []
        if (local_task_dir / "result.md").is_file():
            payload_paths.append(f"{task_prefix}/result.md")
        for path in result_paths:
            if path not in payload_paths:
                payload_paths.append(path)
        for path in payload_paths:
            for action in ("push", "stat"):
                sync_args = dict(arguments)
                sync_args.update({"action": action, "path": path})
                if not _filesync(sync_args).get("ok"):
                    return False
        commit_path = f"{task_prefix}/meta.json"
        for action in ("push", "stat"):
            commit_args = dict(arguments)
            commit_args.update({"action": action, "path": commit_path})
            if not _filesync(commit_args).get("ok"):
                return False
        return True
    sync_args = dict(arguments)
    sync_args.update({
        "action": "push",
        "path": f"shared/tasks/{task_id}",
    })
    if exclude is not None:
        sync_args["exclude"] = exclude
    result = _filesync(sync_args)
    return bool(result.get("ok"))


def _sync_project(arguments: dict[str, Any], project_id: str) -> bool:
    """Push a project directory (meta.json + plan.md + result.md) to shared
    storage.

    Project state is written locally by projectflow, and — unlike tasks,
    which are pushed via _sync_task — was previously only synced to MinIO at
    worker startup (mirror_all). Without this push, a Controller-level project
    API (or dashboard) would read a stale startup snapshot. Mirrors the
    _sync_task pattern.
    """
    sync_args = dict(arguments)
    sync_args.update({
        "action": "push",
        "path": f"shared/projects/{project_id}",
    })
    result = _filesync(sync_args)
    return bool(result.get("ok"))


def _pull_task(arguments: dict[str, Any], task_id: str) -> bool:
    existing = _read_json(_task_state_path(arguments, task_id))
    sync_args = dict(arguments)
    sync_args.update({
        "action": "pull",
        "path": f"shared/tasks/{task_id}",
    })
    result = _filesync(sync_args)
    if not result.get("ok"):
        return False
    if existing:
        task = _read_json(_task_state_path(arguments, task_id))
        if task:
            for snake, camel in (
                ("room_id", "roomId"),
                ("assigned_to", "assignedTo"),
                ("project_id", "projectId"),
            ):
                if _first_text(task.get(snake), task.get(camel)):
                    continue
                preserved = _first_text(existing.get(snake), existing.get(camel))
                if preserved:
                    task[snake] = preserved
            _write_task(arguments, task)
    return True


# mc phrases an absent object/prefix with these markers; every other
# failure (network, auth, backend) must stay fail-closed.
_ABSENT_STORAGE_MARKERS = (
    "does not exist",
    "not found",
    "no such key",
    "no such object",
    "nosuchkey",
)


def _remote_task_absent(arguments: dict[str, Any], task_id: str) -> bool:
    """True only when shared storage CONFIRMS the task meta is absent.

    ``pull`` (mc mirror of the task directory) fails for a missing source
    exactly like it fails for a storage outage, so a failed pull on its own
    cannot tell "brand-new task" from "unreadable task". A stat probe on the
    meta object separates them: mc's absence phrasings mean the caller is
    creating the task (first delegation), anything else stays fail-closed.
    """
    probe = dict(arguments)
    probe.update({"action": "stat", "path": f"shared/tasks/{task_id}/meta.json"})
    result = _filesync(probe)
    if result.get("ok"):
        return False
    error = str(result.get("error") or "").lower()
    return any(marker in error for marker in _ABSENT_STORAGE_MARKERS)


def _load_authoritative_task_optional(
    arguments: dict[str, Any], task_id: str
) -> dict[str, Any] | None:
    """W2 round 10 (issue #1467): pull, then read; None when no task exists.

    Same fail-closed pull as ``_load_authoritative_task``: a failed pull
    raises (the caller must not write on a stale local copy). A task that
    is genuinely absent from shared storage is NOT an error here — the
    caller keeps the legacy no-task compatibility while protocol
    enforcement is off — so the absence is reported as ``None`` instead of
    raising.
    """
    if not _pull_task(arguments, task_id):
        # A failed pull is ambiguous: a brand-new task has no remote prefix
        # at all (the first delegation), while a storage failure has a
        # remote copy the caller must not overwrite. Only a CONFIRMED
        # absence takes the creation path; everything else fails closed.
        if _remote_task_absent(arguments, task_id):
            return None
        raise ValueError(
            "cannot pull the authoritative task state from shared storage; "
            "refusing to write on a stale local copy (retry)"
        )
    task = _read_json(_task_state_path(arguments, task_id))
    if not task:
        return None
    if not task.get("task_id") and task.get("taskId"):
        task["task_id"] = str(task["taskId"])
    return task


def _load_authoritative_task(arguments: dict[str, Any], task_id: str) -> dict[str, Any]:
    """W2 round 9 (issue #1467): authoritative task read for a
    cross-actor task write.

    _sync_task pushes the WHOLE local task directory (mc mirror), so
    writing on a stale local copy overwrites whatever a newer writer
    persisted: a review written onto a leader's cached submitted/open
    task clobbered a decision_status=frozen with its approval records
    and decision_status_history (the maintainer's round-9 repro). The
    guarded entries (review_task / cancel_task / accept_task_result)
    only ever write a task that already exists in shared storage
    (submitted or in flight), so the pull must succeed: on any failure
    the caller fails closed instead of writing the stale copy back.
    """
    task = _load_authoritative_task_optional(arguments, task_id)
    if task is None:
        raise ValueError("task not found")
    return task


def _pull_project(arguments: dict[str, Any], project_id: str) -> bool:
    """Pull a project's meta.json from shared storage (single file) so a
    Controller-level write (pause / resume / replan) takes effect on the next
    read.

    Mirrors _pull_task's field-preservation pattern, but for the single
    authoritative file (shared/projects/{id}/meta.json) instead of a task
    directory (E3): plan.md/result.md are derived/data files and the
    Controller only ever writes meta.json. The pull uses a single-file
    `mc cp` (the normalized path has 4 segments, so _normalize_shared_path
    treats it as a file, not a directory), which is an order of magnitude
    cheaper than a directory mirror.

    After the pull, if the authoritative fields (status / tasks / plan_type)
    differ from the pre-pull local copy, plan.md is re-rendered from the new
    meta (D2) so the derived plan document stays consistent with the single
    source of truth. Fields that the remote copy omits (older versions) are
    back-filled from the local copy to avoid clobbering them.

    Returns True on a successful pull (even when the remote object does not
    exist — the pull is best-effort); callers ignore the return value and
    proceed to read local state, which yields "project not found" naturally
    when the project does not exist.
    """
    existing = _read_json(_project_state_path(arguments, project_id))
    sync_args = dict(arguments)
    sync_args.update({
        "action": "pull",
        "path": f"shared/projects/{project_id}/meta.json",
    })
    result = _filesync(sync_args)
    if not result.get("ok"):
        return False
    pulled = _read_json(_project_state_path(arguments, project_id))
    if not pulled:
        return False

    changed = any(pulled.get(key) != existing.get(key) for key in ("status", "tasks", "plan_type"))
    if existing:
        # Back-fill fields the remote copy omits (older versions). String
        # fields use _first_text; reply_route is a dict and must be preserved
        # as-is when the remote omits it.
        for snake, camel in (
            ("source_room_id", "sourceRoomId"),
            ("team_id", "teamId"),
        ):
            if _first_text(pulled.get(snake), pulled.get(camel)):
                continue
            preserved = _first_text(existing.get(snake), existing.get(camel))
            if preserved:
                pulled[snake] = preserved
        if not pulled.get("reply_route") and existing.get("reply_route"):
            pulled["reply_route"] = existing["reply_route"]
        _write_json(_project_state_path(arguments, project_id), pulled)
    if changed:
        _write_project_plan(_project_dir(arguments, project_id), pulled)
    return True


# All four are monkeypatch-sensitive (15-name set); no self-calls, so no
# rebinding to proxies is needed.
refs.define("_sync_task", _sync_task)
refs.define("_sync_project", _sync_project)
refs.define("_pull_task", _pull_task)
refs.define("_pull_project", _pull_project)
refs.define("_load_authoritative_task", _load_authoritative_task)
refs.define("_load_authoritative_task_optional", _load_authoritative_task_optional)

# Rebind to a proxy so a test monkeypatch of server._pull_task is visible
# inside this module (same pattern as taskflow's _write_task).
_pull_task = refs.proxy("_pull_task")


