# shared task/project state sync and pull (W1-a2: real module)
from __future__ import annotations

import json
import time
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


# #1402: the conflict-bearing object is the project meta.json — the object
# the Go LoopHandler rewrites under a strict ETag CAS (PutObjectIfMatchStrict
# in agentteams-controller/internal/server/loop_handler.go).  _sync_project
# must therefore never commit it through the unguarded directory mirror:
# it snapshots the remote meta, merges the controller-owned pause state,
# pushes meta.json through the guarded single-file path, and verifies the
# commit with a bounded retry (an exhausted retry is an error, never a
# silent success — same stance as the Go CAS loop).
_SYNC_PROJECT_MAX_ATTEMPTS = 3
_SYNC_PROJECT_RETRY_BACKOFFS = (0.1, 0.25)


def _remote_project_meta_snapshot(
    arguments: dict[str, Any], meta_path: str
) -> tuple[dict[str, Any] | None, str | None]:
    """Read the remote project meta without touching local state.

    Filesync ``cat`` (``mc cat`` through the same alias/env resolution as
    pull/push): unlike ``pull`` it never writes the local tree.  Returns
    (meta, error):

    - (meta, None)  -> remote object read (possibly empty)
    - (None, None)  -> remote object does not exist (normal first push)
    - (None, reason)-> the read failed; the caller proceeds without a
      snapshot (fail-open, mirroring the single-file push guard's probe
      semantics — a transient storage outage must not wedge project writes)
    """
    cat_args = dict(arguments)
    cat_args.update({"action": "cat", "path": meta_path})
    result = _filesync(cat_args)
    if result.get("ok"):
        content = result.get("content")
        if not content:
            return {}, None
        try:
            meta = json.loads(content)
        except ValueError:
            return None, "remote project meta is not valid JSON"
        if not isinstance(meta, dict):
            return None, "remote project meta is not a JSON object"
        return meta, None
    error = str(result.get("error") or "")
    if "does not exist" in error.lower():
        return None, None
    return None, error


def _merge_project_pause_state(
    local_meta: dict[str, Any] | None,
    snapshot: dict[str, Any] | None,
) -> dict[str, Any]:
    """Merge the controller-owned pause state from a remote snapshot into
    the local project meta (the local copy stays the write base).

    #1402 rules — a worker write preserves pause state it did not author:

    - project ``status``: a remote ``paused`` is authoritative (never
      write ``active`` over it); every other status is the local writer's
      to own;
    - ``loop.pause_sources``: union of remote and local — a worker never
      removes a source it did not add; removal stays with the
      Controller's CAS write;
    - ``loop.paused_by_project``: the remote value wins;
    - loop keys present remotely but unmodeled locally: carried over
      (mirror of the Go ``loopMeta.Extra`` round-trip rule).
    """
    merged = dict(local_meta or {})
    if not snapshot:
        return merged
    if str(snapshot.get("status") or "") == "paused":
        merged["status"] = "paused"
    snapshot_loop = snapshot.get("loop")
    if not isinstance(snapshot_loop, dict) or not snapshot_loop:
        return merged
    local_loop = merged.get("loop")
    loop = dict(local_loop) if isinstance(local_loop, dict) else {}
    for key, value in snapshot_loop.items():
        if key not in loop:
            loop[key] = value
    local_sources = loop.get("pause_sources")
    remote_sources = snapshot_loop.get("pause_sources")
    sources = {
        source
        for source in (local_sources if isinstance(local_sources, list) else [])
        if isinstance(source, str) and source.strip()
    }
    sources.update(
        source
        for source in (remote_sources if isinstance(remote_sources, list) else [])
        if isinstance(source, str) and source.strip()
    )
    if sources:
        loop["pause_sources"] = sorted(sources)
    if "paused_by_project" in snapshot_loop:
        loop["paused_by_project"] = snapshot_loop["paused_by_project"]
    merged["loop"] = loop
    return merged


def _project_pause_state(meta: dict[str, Any] | None) -> dict[str, Any]:
    """The pause state a committed project meta must retain.

    Every field unset when the meta carries no pause state, so the verify
    step is vacuous for a write that authored and observed none.
    """
    meta = meta if isinstance(meta, dict) else {}
    loop = meta.get("loop")
    if not isinstance(loop, dict):
        loop = {}
    sources = loop.get("pause_sources")
    return {
        "status_paused": meta.get("status") == "paused",
        "pause_sources": sorted(
            {
                source
                for source in (sources if isinstance(sources, list) else [])
                if isinstance(source, str) and source.strip()
            }
        )
        or None,
        "paused_by_project": loop.get("paused_by_project") is True,
    }


def _pause_state_missing(
    expected: dict[str, Any],
    remote_meta: dict[str, Any] | None,
) -> bool:
    """True when any pause state expected in the committed meta is absent
    from the remote copy — or the remote copy could not be read at all
    (``remote_meta is None``: a verified commit is required, so an
    unverifiable one counts as missing)."""
    if remote_meta is None:
        return True
    loop = remote_meta.get("loop")
    if not isinstance(loop, dict):
        loop = {}
    if expected["status_paused"] and remote_meta.get("status") != "paused":
        return True
    if expected["pause_sources"]:
        sources = loop.get("pause_sources")
        remote = {
            source
            for source in (sources if isinstance(sources, list) else [])
            if isinstance(source, str)
        }
        if not set(expected["pause_sources"]).issubset(remote):
            return True
    if expected["paused_by_project"] and loop.get("paused_by_project") is not True:
        return True
    return False


def _sync_project(arguments: dict[str, Any], project_id: str) -> bool:
    """Push a project's derived files and its meta.json to shared storage,
    commit-safe against a concurrent Controller pause (#1402).

    Project state is written locally by projectflow, and — unlike tasks,
    which are pushed via _sync_task — was previously only synced to MinIO at
    worker startup (mirror_all). Without this push, a Controller-level
    project API (or dashboard) would read a stale startup snapshot. Mirrors
    the _sync_task pattern.

    The conflict-bearing object (meta.json) is never written by the
    unguarded directory mirror; every attempt:

    1. snapshot the remote meta without touching local state (filesync
       ``cat``; a failed snapshot is fail-open — the push proceeds without
       a merge, the same stance as the guard's probe);
    2. merge the controller-owned pause state from the snapshot into the
       local meta and re-write it (``_merge_project_pause_state``), so
       every project writer (lifecycle transitions, project flow, state)
       inherits the protection;
    3. push the project directory EXCLUDING meta.json (derived files only:
       plan.md, result.md, ...);
    4. push meta.json through the guarded single-file path;
    5. verify the commit by re-reading the remote meta: if any pause state
       observed in the snapshot (carried into the merged set) is absent,
       re-snapshot, re-merge and retry — bounded to
       _SYNC_PROJECT_MAX_ATTEMPTS with _SYNC_PROJECT_RETRY_BACKOFFS.

    Returns True only when the meta commit is verified (or the project
    carries no local meta.json, in which case the verified directory push
    is the whole commit).  An exhausted retry returns False — never a
    silent success.
    """
    meta_path = f"shared/projects/{project_id}/meta.json"
    dir_path = f"shared/projects/{project_id}"
    state_path = _project_state_path(arguments, project_id)
    has_local_meta = state_path.is_file()

    for attempt in range(1, _SYNC_PROJECT_MAX_ATTEMPTS + 1):
        if attempt > 1:
            time.sleep(_SYNC_PROJECT_RETRY_BACKOFFS[attempt - 2])
        snapshot, _snapshot_error = _remote_project_meta_snapshot(arguments, meta_path)
        merged: dict[str, Any] | None = None
        if has_local_meta:
            merged = _merge_project_pause_state(_read_json(state_path), snapshot)
            _write_json(state_path, merged)
        dir_args = dict(arguments)
        dir_args.update({
            "action": "push",
            "path": dir_path,
            "exclude": ["meta.json"],
        })
        if not _filesync(dir_args).get("ok"):
            continue
        if has_local_meta:
            meta_args = dict(arguments)
            meta_args.update({"action": "push", "path": meta_path})
            if not _filesync(meta_args).get("ok"):
                continue
            expected = _project_pause_state(merged)
            if not any(expected.values()):
                return True
            verified, _verify_error = _remote_project_meta_snapshot(arguments, meta_path)
            if _pause_state_missing(expected, verified):
                continue
        return True
    return False


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


