# signal contract emission, event listing, and retention (W1-a2: real module)
from __future__ import annotations

import base64
import datetime

import hashlib

import json
import os

import re

from pathlib import Path

from typing import Any

# Cross-group imports are top-level absolutes (`common.*`) — see the note in
# transitions.py: mcp/ on sys.path makes the identity identical in both
# `import server` and `import mcp.server` modes.
from mcp.common import refs
from mcp.common.config import _load_runtime_config, _runtime_team_name, _section
from mcp.common.io import _utc_timestamp, _write_json
from mcp.common.paths import _workspace_dir

_filesync = refs.proxy("_filesync")

# S2 signal events (INT-3, #1394 aligned). The 14-event list is single-sourced
# at plugins/teamharness/contracts/signal-mapping.json (cross-language golden
# fixture; F8 precedent — the Go emitter table is cross-checked against it in
# its tests, this table must match the contract's py:* emitters, asserted by
# plugins/tests/teamharness/test_signal_emission.py).
SIGNAL_CONTRACT_PATH = (
    Path(__file__).resolve().parents[2] / "contracts" / "signal-mapping.json"
)


_SIGNAL_EMITTERS: dict[str, tuple[str, str, str]] = {
    # emitter -> (source_event, event_type, origin); must match the contract
    # rows' source_events / event_type / the schema origin enum.
    "submit_task": ("task_submitted", "SIG-COMPLETE", "submit"),
    "accept_task_result": ("task_accepted", "SIG-COMPLETE", "submit"),
    "record_loop_iteration": ("loop_iteration_finished", "SIG-DECISION", "decision_lock"),
    "record_loop_iteration:review": ("loop_review_verdict", "SIG-VERDICT", "critic_verdict"),
    "illegal_transition_rejected": ("mismatch_recorded", "SIG-MISMATCH", "mismatch_recorded"),
}


def _signal_event_id(
    team_id: str,
    project_id: str,
    task_id: str,
    loop_id: str,
    event_type: str,
    source_event: str,
    iteration: int | None,
    fingerprint: str,
    attempt: int = 0,
    audit_seq: int = 0,
    discriminator: str = "",
) -> str:
    # Contract id_rule (v1.2): 'ev_' + sha256(team_id|project_id|task_id|
    # loop_id|event_type|source_event|iteration|fingerprint|attempt|
    # audit_seq)[:16]. fingerprint is the hex sha256 of the ref bytes after
    # the main write — the stable identity of the logical operation's
    # result; audit_seq is the project's persisted committed-operation
    # revision (the operation INSTANCE identity), so two distinct
    # operations whose post-write state bytes are byte-identical (e.g. a
    # second pause within the same wall-clock second) still hash distinct
    # inputs, while a retry of the same operation (same committed revision
    # + same state bytes) re-lands on the same id (consumer dedupes).
    # Must stay byte-identical to the Go signalEventID (both test suites
    # pin the same fixed vector).
    #
    # Rejection events (SIG-MISMATCH) append an extra final component: a
    # rejection commits no state, so the ref bytes — and therefore the
    # fingerprint — are identical across *different* rejected operations
    # on the same task state. The rejection discriminator
    # ``action|from|to`` is the logical-operation identity for this
    # family: appending it keeps distinct rejections on distinct ids
    # while a retry of the same rejection re-hashes identically. Events
    # without a discriminator hash the fixed ten-field v1.2 input
    # unchanged (the cross-language fixed vector is untouched).
    parts = [
        team_id,
        project_id,
        task_id,
        loop_id,
        event_type,
        source_event,
        str(iteration) if iteration is not None else "",
        fingerprint,
        str(attempt),
        str(audit_seq),
    ]
    if discriminator:
        parts.append(discriminator)
    digest = hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()
    return "ev_" + digest[:16]


def _project_audit_seq(project: dict[str, Any] | Any) -> int:
    # id_rule v1.2: the project's committed-operation revision as persisted
    # by the Go intervention endpoints (markAuditFields advances it per
    # committed write). This runtime only ever READS it — Python writes
    # always change state bytes, so no increment is needed here. 0 when
    # absent (projects written before v1.2).
    if not isinstance(project, dict):
        return 0
    seq = project.get("audit_seq")
    if isinstance(seq, bool) or not isinstance(seq, int):
        return 0
    return seq


# S2 event retention (#1702). The live stream under shared/events/{team_id}/
# used to grow unboundedly: one immutable file per event and no cleanup
# anywhere in the plugin, while every list_events read paid O(N) file reads.
# Events are the output of a project's lifecycle, so retention keys off the
# project's lifecycle state instead of a per-event TTL: once a project is
# terminal (project meta status == "completed" — the terminal state written
# by Go CompleteProject and Python complete_project) or its last activity is
# older than the configured window (retention.days, default 7), the
# project's live event files are grouped into content-addressed archive pieces
# shared/events/{team_id}/{project_id}-archive-YYYYMMDD-{content_hash}.json, the local source
# files are deleted after the MinIO mirror is pruned successfully. The archive
# object is NOT part of the live stream (list_events and the Go read surface
# both read live event files only); it is the retained audit copy. The
# semantics are registered in contracts/signal-mapping.json under "retention".

_RETENTION_DEFAULT_DAYS = 7
_RETENTION_SWEEP_EVERY_N_EMITS = 16
_RETENTION_MAX_SWEEP_AGE = datetime.timedelta(hours=24)
_RETENTION_STATE_NAME = ".retention.state"
_RECENT_WINDOW_MIN_FILES = 20
_ARCHIVE_NAME_RE = re.compile(r"^(?P<project_id>.+)-archive-(?P<date>\d{8})(?:-[0-9a-f]{16})?\.json$")


def _retention_days() -> int:
    """The retention window in days (configurable, default 7).

    The TEAMHARNESS_RETENTION_DAYS environment variable wins when set (the
    repo env-override pattern), else the TEAMHARNESS_RUNTIME_CONFIG
    retention.days key (JSON or YAML). Non-numeric or negative values fall
    back to the default.
    """
    raw = os.getenv("TEAMHARNESS_RETENTION_DAYS", "").strip()
    if raw:
        try:
            value = int(raw)
        except ValueError:
            return _RETENTION_DEFAULT_DAYS
        return value if value >= 0 else _RETENTION_DEFAULT_DAYS
    days = _section(_load_runtime_config(), "retention").get("days")
    if isinstance(days, bool):
        return _RETENTION_DEFAULT_DAYS
    if isinstance(days, (int, float)):
        value = int(days)
    else:
        # The simple-YAML scalar loader returns strings.
        try:
            value = int(str(days or "").strip())
        except ValueError:
            return _RETENTION_DEFAULT_DAYS
    return value if value >= 0 else _RETENTION_DEFAULT_DAYS


def _read_retention_state(events_dir: Path) -> dict[str, Any]:
    path = events_dir / _RETENTION_STATE_NAME
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def _file_mtime_or_zero(path: Path) -> float:
    try:
        return path.stat().st_mtime
    except OSError:
        return 0.0


def _project_last_activity(meta: dict[str, Any], meta_path: Path) -> datetime.datetime | None:
    """The project's last activity: the meta 'updated_at'/'created_at'
    field (RFC3339) when present, else the local meta file's mtime."""
    raw = str(meta.get("updated_at") or meta.get("created_at") or "").strip()
    if raw:
        try:
            parsed = datetime.datetime.fromisoformat(raw.replace("Z", "+00:00"))
        except ValueError:
            parsed = None
        if parsed is not None:
            if parsed.tzinfo is None:
                parsed = parsed.replace(tzinfo=datetime.timezone.utc)
            return parsed.astimezone(datetime.timezone.utc)
    try:
        return datetime.datetime.fromtimestamp(meta_path.stat().st_mtime, datetime.timezone.utc)
    except OSError:
        return None


def _event_project_id(workspace: Path, ev: dict[str, Any], task_project_cache: dict[str, str]) -> str:
    """Best-effort project attribution for one live event object.

    Order: payload.project_id (every Python emitter stamps it), then the
    task meta's project_id for task-level events (the SIG-MISMATCH emitters
    stamp neither), then a project path in the event's ref (project-level
    events carry shared/projects/{pid}/... — the same pattern as the Go
    read-side attribution). Returns "" when nothing resolves: such events
    are left live forever (never delete what cannot be attributed).
    """
    payload = ev.get("payload")
    if isinstance(payload, dict):
        pid = str(payload.get("project_id") or "").strip()
        if pid:
            return pid
    task_id = str(ev.get("task_id") or "").strip()
    if task_id:
        if task_id in task_project_cache:
            return task_project_cache[task_id]
        project_id = ""
        meta_path = workspace / "shared" / "tasks" / task_id / "meta.json"
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            meta = None
        if isinstance(meta, dict):
            project_id = str(meta.get("project_id") or "").strip()
        task_project_cache[task_id] = project_id
        if project_id:
            return project_id
    ref = str(ev.get("ref") or "")
    match = re.search(r"/projects/([^/]+)/", ref)
    if match:
        return match.group(1)
    return ""


def _read_archive(archive_path: Path) -> dict[str, Any]:
    """The existing archive object (extension target) or a fresh skeleton."""
    if archive_path.exists():
        try:
            data = json.loads(archive_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            data = None
        if isinstance(data, dict) and isinstance(data.get("events"), list):
            return data
    return {
        "archive": True,
        "version": 1,
        "team_id": "",
        "project_id": "",
        "archived_at": _utc_timestamp(),
        "reason": "",
        "event_count": 0,
        "events": [],
    }


def _retention_sweep(arguments: dict[str, Any], team_id: str) -> None:
    """Merge-and-archive the live event files of terminal or expired projects.

    Best-effort end to end: every failure is swallowed here or by the
    caller, so retention never blocks an emit or a read. Per project the
    order is: write the archive locally -> push the archive to the MinIO
    mirror -> prune the mirrored source objects -> delete the local source
    files. When the archive push fails, the local source files are kept
    (nothing is ever lost; the next sweep retries).
    """
    workspace = _workspace_dir(arguments)
    events_dir = workspace / "shared" / "events" / team_id
    if not events_dir.is_dir():
        return
    days = _retention_days()
    now = datetime.datetime.now(datetime.timezone.utc)

    # Candidate projects from the local project metas.
    candidates: dict[str, str] = {}
    projects_root = workspace / "shared" / "projects"
    if projects_root.is_dir():
        for meta_path in sorted(projects_root.glob("*/meta.json")):
            try:
                meta = json.loads(meta_path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if not isinstance(meta, dict):
                continue
            project_id = meta_path.parent.name
            status = str(meta.get("status") or "").strip().lower()
            if status == "completed":
                candidates[project_id] = "project_completed"
                continue
            if days > 0:
                last_activity = _project_last_activity(meta, meta_path)
                if last_activity is not None and (now - last_activity) > datetime.timedelta(days=days):
                    candidates[project_id] = "expired"
    if not candidates:
        return

    # Live event files (archive objects are retention output, never input:
    # they carry no top-level event_id and are skipped by name).
    live_files: list[tuple[Path, dict[str, Any]]] = []
    for path in sorted(events_dir.glob("*.json")):
        if not path.is_file() or _ARCHIVE_NAME_RE.match(path.name):
            continue
        try:
            ev = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if isinstance(ev, dict) and isinstance(ev.get("event_id"), str):
            live_files.append((path, ev))

    # Attribute each live event to a candidate project (events that cannot
    # be attributed to a candidate stay live).
    by_project: dict[str, list[tuple[Path, dict[str, Any]]]] = {pid: [] for pid in candidates}
    task_project_cache: dict[str, str] = {}
    for path, ev in live_files:
        project_id = _event_project_id(workspace, ev, task_project_cache)
        if project_id in by_project:
            by_project[project_id].append((path, ev))

    today = now.strftime("%Y%m%d")
    for project_id, items in by_project.items():
        if not items:
            continue
        if candidates[project_id] == "expired":
            # Project metadata can lag task/event activity. Keep the entire
            # active stream when any of its events or tasks is still recent.
            activity = [_project_last_activity({"updated_at": e.get("occurred_at")}, p) for p, e in items]
            for task_id in {str(e.get("task_id") or "") for _, e in items} - {""}:
                task_path = workspace / "shared" / "tasks" / task_id / "meta.json"
                try:
                    task = json.loads(task_path.read_text(encoding="utf-8"))
                except (OSError, ValueError):
                    continue
                if isinstance(task, dict):
                    activity.append(_project_last_activity(task, task_path))
            if any(t is not None and now - t <= datetime.timedelta(days=days) for t in activity):
                continue
        merged = [e for _, e in items]
        merged.sort(key=lambda e: (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")))
        # Different workspaces may hold different subsets of a project. A
        # content-addressed piece cannot overwrite another subset's archive.
        digest = hashlib.sha256(json.dumps(merged, sort_keys=True, separators=(",", ":")).encode()).hexdigest()[:16]
        archive_name = f"{project_id}-archive-{today}-{digest}.json"
        archive_path = events_dir / archive_name
        archive = _read_archive(archive_path)
        archive["archive"] = True
        archive["version"] = 1
        archive["team_id"] = team_id
        archive["project_id"] = project_id
        archive["reason"] = candidates[project_id]
        archive["archived_at"] = _utc_timestamp()
        archive["events"] = merged
        archive["event_count"] = len(archive["events"])
        _write_json(archive_path, archive)

        # Mirror the archive BEFORE deleting anything local.
        sync_args = dict(arguments)
        sync_args.update({"action": "push", "path": f"shared/events/{team_id}/{archive_name}"})
        try:
            result = _filesync(sync_args)
        except Exception as exc:
            result = {"ok": False, "error": str(exc)}
        if not (isinstance(result, dict) and result.get("ok")):
            # Keep the local source files; the next sweep retries.
            continue

        # Keep each local source as the retry record until remote cleanup
        # succeeds. Existing archive membership is not a publication receipt.
        prune_failures = 0
        for path, _ev in items:
            prune_args = dict(arguments)
            prune_args.update({"action": "delete", "path": f"shared/events/{team_id}/{path.name}"})
            try:
                prune = _filesync(prune_args)
            except Exception as exc:
                prune = {"ok": False, "error": str(exc)}
            if not (isinstance(prune, dict) and prune.get("ok")):
                error = str((prune or {}).get("error") or "").lower()
                if "does not exist" not in error:
                    prune_failures += 1
                    if prune_failures >= 5:
                        break
                    continue
            try:
                path.unlink()
            except OSError:
                pass  # Retry local cleanup on a later sweep.


def _maybe_run_retention(arguments: dict[str, Any], team_id: str, *, emit_occurred: bool) -> None:
    """Throttled retention trigger (#1702 requirement c).

    Called at the tail of every signal event emit and on every list_events
    call. The check itself is O(1) — one small state file under
    shared/events/{team_id}/ — while the full sweep runs at most every
    _RETENTION_SWEEP_EVERY_N_EMITS emits, plus a max-lag backstop so an idle
    team's terminal projects still get archived on the next emit or
    list_events. Never raises: retention must never block the caller's
    flow.
    """
    try:
        events_dir = _workspace_dir(arguments) / "shared" / "events" / team_id
        if not events_dir.is_dir():
            return
        state = _read_retention_state(events_dir)
        count = state.get("emit_count")
        if isinstance(count, bool) or not isinstance(count, int):
            count = 0
        now = datetime.datetime.now(datetime.timezone.utc)
        last: datetime.datetime | None = None
        last_raw = str(state.get("last_sweep_at") or "").strip()
        if last_raw:
            try:
                parsed = datetime.datetime.fromisoformat(last_raw.replace("Z", "+00:00"))
                if parsed.tzinfo is None:
                    parsed = parsed.replace(tzinfo=datetime.timezone.utc)
                last = parsed.astimezone(datetime.timezone.utc)
            except ValueError:
                last = None
        if emit_occurred:
            count += 1
        due = (
            (emit_occurred and count >= _RETENTION_SWEEP_EVERY_N_EMITS)
            or last is None
            or (now - last) >= _RETENTION_MAX_SWEEP_AGE
        )
        if not due:
            if emit_occurred:
                _write_json(events_dir / _RETENTION_STATE_NAME, {
                    "emit_count": count,
                    "last_sweep_at": last_raw,
                })
            return
        _retention_sweep(arguments, team_id)
        _write_json(events_dir / _RETENTION_STATE_NAME, {
            "emit_count": 0,
            "last_sweep_at": _utc_timestamp(),
        })
    except Exception:
        pass  # retention must never block the caller's flow


def _emit_signal_event(
    arguments: dict[str, Any],
    emitter: str,
    *,
    task_id: str = "",
    project_id: str = "",
    loop_id: str | None = None,
    iteration_index: int | None = None,
    ref: str = "",
    ref_bytes: bytes = b"",
    payload: dict[str, Any] | None = None,
    audit_seq: int = 0,
    discriminator: str = "",
) -> bool:
    """Append one S2 signal event object under shared/events/{team_id}/.

    Best-effort: never raises and never rolls back the caller's write (the
    main write already happened; the event is a convenience, same policy as
    the Go side). Idempotent: the deterministic event id makes a re-emit
    land on the same object key, which is then skipped. The local write is
    followed by a best-effort single-file mirror push (failure warn-only,
    the local object stays). Returns True when the object was written.

    audit_seq (contract id_rule v1.2) is the project's persisted
    committed-operation revision — the operation instance identity that
    distinguishes two operations whose post-write state bytes would
    otherwise be identical; 0 when the project carries none.

    discriminator (contract id_rule v1.2, rejection family): a rejection
    commits no state, so the ref bytes do not differ between distinct
    rejected operations. The ``action|from|to`` discriminator is appended
    to the identity so distinct rejections get distinct ids while a retry
    of the same rejection stays idempotent.
    """
    try:
        source_event, event_type, origin = _SIGNAL_EMITTERS[emitter]
        team_name = str(_runtime_team_name() or "").strip()
        team_id = team_name or "global"
        # The fingerprint of the post-write ref state plus the committed
        # audit_seq are the stable identity of this logical operation
        # (contract id_rule v1.2) — hashed into the event id so distinct
        # operations never share a key while a retry of the same operation
        # re-lands on the original.
        fingerprint = hashlib.sha256(ref_bytes).hexdigest()
        event_id = _signal_event_id(
            team_id,
            project_id,
            task_id,
            loop_id or "",
            event_type,
            source_event,
            iteration_index,
            fingerprint,
            0,
            audit_seq,
            discriminator,
        )
        path = _workspace_dir(arguments) / "shared" / "events" / team_id / f"{event_id}.json"
        if path.exists():
            return False  # idempotent re-emit: keep the original object
        event = {
            "event_id": event_id,
            "task_id": task_id,
            "loop_id": loop_id,
            "event_type": event_type,
            "is_signal": True,
            "source_event": {"origin": origin, "request_id": None},
            "ref": ref,
            "fingerprint": fingerprint,
            "iteration_index": iteration_index,
            "payload": payload or {},
            "occurred_at": _utc_timestamp(),
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(event, ensure_ascii=False, indent=2), encoding="utf-8")
    except Exception:
        return False  # warn-only: never fail the main flow over an event
    try:
        sync_args = dict(arguments)
        sync_args.update({
            "action": "push",
            "path": f"shared/events/{team_id}/{event_id}.json",
        })
        _filesync(sync_args)
    except Exception:
        pass  # mirror push is warn-only
    # Retention trigger (#1702, requirement c): an O(1) state-file check per
    # emit; the full sweep runs every _RETENTION_SWEEP_EVERY_N_EMITS emits.
    # Never raises.
    _maybe_run_retention(arguments, team_id, emit_occurred=True)
    return True


def _list_events(arguments: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    """S2 event stream read (#1394 list_events, read-only, cursor pagination).

    Reads the LOCAL mirror under shared/events/{team}/ — the same read
    pattern as the other projectflow read actions. Controller-side emits
    become visible once the worker's next mirror cycle lands them (the
    consumer dedupes by event_id regardless).

    Cursor contract (#1433 round 2): the cursor is opaque to callers but
    shared ACROSS the two read surfaces — this action and the controller's
    GET /api/v1/projects/{id}/signal-events both emit
    ``base64url("<occurred_at>\x00<event_id>")`` and a page must resume
    across the boundary in either direction. Go emits unpadded
    (RawURLEncoding), Python emits padded (urlsafe_b64encode), so BOTH
    sides must accept BOTH forms (this decoder re-pads before decoding).
    If either side changes its cursor encoding, sync the other side's
    decoder and this note.

    Retention (#1702): before reading, a throttled best-effort sweep may
    merge-and-archive a terminal or expired project's live event files
    (event-store maintenance, not a project/task state mutation — see the
    durability.json note). Archive objects are NOT part of the live stream
    on either read surface, so archived events leave list_events results.

    Fast path (#1702, contract "retention.fast_path"): the payload key
    `recent` (boolean, opt-in) reads only the most-recently-written
    `max(20, 4 * limit)` live event files (mtime order — event ids are
    sha256-based, so filename order is not time order) and serves pages
    newest-first, resuming strictly before the page's oldest event.
    Approximate whenever the window does not cover every live file
    (declared in the response with "approximate": true); the default read
    is unchanged.
    """
    team = str(payload.get("team") or _runtime_team_name() or "").strip() or "global"
    # Best-effort lazy retention trigger (#1702 requirement c): before
    # reading the stream, a throttled sweep may merge-and-archive a
    # terminal or expired project's live event files (event-store
    # maintenance, not a project/task state mutation — durability.json
    # note). Never raises.
    _maybe_run_retention(arguments, team, emit_occurred=False)
    since = str(payload.get("since") or "").strip()
    types = payload.get("types")
    if isinstance(types, list) and types:
        type_set = {str(t) for t in types}
    elif types:
        type_set = {str(types)}
    else:
        type_set = None
    task_id = str(payload.get("taskId") or payload.get("task_id") or "").strip() or None
    recent = bool(payload.get("recent") or False)
    try:
        limit = int(payload.get("limit") or 50)
    except (TypeError, ValueError):
        limit = 50
    limit = max(1, min(limit, 200))
    cursor = str(payload.get("cursor") or "").strip()
    after: tuple[str, str] | None = None
    if cursor:
        try:
            # Re-pad to a multiple of 4: the Go read surface (#1433) emits
            # unpadded base64url cursors, and both forms must be accepted.
            padded = cursor + "=" * (-len(cursor) % 4)
            decoded = base64.urlsafe_b64decode(padded.encode("ascii")).decode("utf-8")
            after_ts, after_id = decoded.rsplit("\x00", 1)
            after = (after_ts, after_id)
        except Exception:
            return {
                "ok": False,
                "tool": "projectflow",
                "action": "list_events",
                "error": "invalid cursor",
            }
    events: list[dict[str, Any]] = []
    events_dir = _workspace_dir(arguments) / "shared" / "events" / team
    live_files_total = 0
    window_files_read = 0
    if events_dir.is_dir():
        if recent:
            # mtime-sorted recent window (#1702 fast path): event ids are
            # sha256-based (contract id_rule), so filename order is NOT
            # time order — take the most-recently-written live files
            # instead. Archive objects are skipped by name: they are
            # retention output, not live stream content.
            files = [
                p for p in events_dir.glob("*.json")
                if p.is_file() and not _ARCHIVE_NAME_RE.match(p.name)
            ]
            files.sort(key=_file_mtime_or_zero, reverse=True)
            live_files_total = len(files)
            window = files[: max(_RECENT_WINDOW_MIN_FILES, 4 * limit)]
            window_files_read = len(window)
            for path in window:
                try:
                    ev = json.loads(path.read_text(encoding="utf-8"))
                except Exception:
                    continue
                if not isinstance(ev, dict) or not isinstance(ev.get("event_id"), str):
                    continue
                events.append(ev)
        else:
            for path in sorted(events_dir.glob("*.json")):
                if _ARCHIVE_NAME_RE.match(path.name):
                    continue
                try:
                    ev = json.loads(path.read_text(encoding="utf-8"))
                except Exception:
                    continue
                if not isinstance(ev, dict) or not isinstance(ev.get("event_id"), str):
                    continue
                events.append(ev)
    if type_set is not None:
        events = [e for e in events if e.get("event_type") in type_set]
    if task_id:
        events = [e for e in events if str(e.get("task_id") or "") == task_id]
    if since:
        # RFC3339 UTC 'Z' timestamps: lexicographic == chronological.
        events = [e for e in events if str(e.get("occurred_at") or "") >= since]
    if recent:
        # Newest first within the window; the cursor anchor is the page's
        # OLDEST event and the next page resumes strictly BEFORE it (same
        # base64url(ts \x00 id) encoding; recent-mode cursors only resume
        # in recent mode — contract "retention.fast_path").
        events.sort(
            key=lambda e: (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")),
            reverse=True,
        )
        if after is not None:
            events = [
                e for e in events
                if (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")) < after
            ]
        page = events[:limit]
        next_cursor: str | None = None
        if len(events) > limit:
            last_ts = str(events[limit - 1].get("occurred_at") or "")
            last_id = str(events[limit - 1].get("event_id") or "")
            next_cursor = base64.urlsafe_b64encode(f"{last_ts}\x00{last_id}".encode("utf-8")).decode("ascii")
        return {
            "ok": True,
            "tool": "projectflow",
            "action": "list_events",
            "team": team,
            "events": page,
            "count": len(page),
            "next_cursor": next_cursor,
            "recent": True,
            "window_files": window_files_read,
            "approximate": live_files_total > window_files_read,
        }
    events.sort(key=lambda e: (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")))
    if after is not None:
        events = [
            e for e in events
            if (str(e.get("occurred_at") or ""), str(e.get("event_id") or "")) > after
        ]
    page = events[:limit]
    next_cursor: str | None = None
    if len(events) > limit:
        last_ts = str(events[limit - 1].get("occurred_at") or "")
        last_id = str(events[limit - 1].get("event_id") or "")
        next_cursor = base64.urlsafe_b64encode(f"{last_ts}\x00{last_id}".encode("utf-8")).decode("ascii")
    return {
        "ok": True,
        "tool": "projectflow",
        "action": "list_events",
        "team": team,
        "events": page,
        "count": len(page),
        "next_cursor": next_cursor,
    }


