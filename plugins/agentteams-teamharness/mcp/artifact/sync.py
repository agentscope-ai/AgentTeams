# shared task/project state sync and pull (W1-a2: real module)
from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
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
# the Controller rewrites under a strict ETag CAS (PutObjectIfMatchStrict,
# both the LoopHandler pause/resume writes and the conditional commit
# endpoint PUT /api/v1/projects/{id}/meta).  _sync_project must therefore
# never commit it through an unguarded push: it snapshots the remote meta,
# merges the controller-owned pause state, commits the merged meta
# conditionally against the observed ETag, and verifies the commit with a
# bounded retry (an exhausted retry is an error, never a silent success —
# same stance as the Go CAS loop).
_SYNC_PROJECT_MAX_ATTEMPTS = 3
_SYNC_PROJECT_RETRY_BACKOFFS = (0.1, 0.25)
# Bounded retries for the versioned meta read: a version flip between the
# bracketing stats discards the body and re-runs the read, but a writer in
# flight on every attempt must not loop the sync forever.
_SYNC_META_READ_ATTEMPTS = 3


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

    #1402 rules — the Controller is the single writer of the pause state,
    so the snapshot is authoritative for it and the local copy only owns
    the fields the worker writes:

    - project ``status``: a remote ``paused`` is authoritative (never
      write ``active`` over it); every other status is the local writer's
      to own — the agent-side pause/resume is this legacy channel. (This
      stays correct only while the Controller never transitions ``status``
      itself: it writes ``active`` on create and nothing after. A future
      Controller-side status transition must become remote-authoritative
      too, or a stale cache could resurrect it the same way.)
    - ``loop.pause_sources``: the remote value wins. The local copy never
      adds or keeps a source, so a cache pulled before an admin resume
      cannot write the released veto back on the next progress sync. The
      union this replaces protected nothing: the snapshot and the commit's
      If-Match come from the same versioned read, and a pause landing after
      it fails either the CAS (409 -> re-read -> merge) or the verify
      re-read — the union only resurrected stale local state;
    - ``loop.paused_by_project``: the remote value wins (an absent remote
      key drops the stale local copy);
    - loop keys present remotely but unmodeled locally: carried over
      (mirror of the Go ``loopMeta.Extra`` round-trip rule);
    - local-only loop keys (``current_iteration``, the task plan, ...):
      the local writer keeps them.
    """
    merged = dict(local_meta or {})
    if snapshot is None:
        return merged
    if str(snapshot.get("status") or "") == "paused":
        merged["status"] = "paused"
    snapshot_loop = snapshot.get("loop")
    snapshot_loop = snapshot_loop if isinstance(snapshot_loop, dict) else {}
    local_loop = merged.get("loop")
    loop = dict(local_loop) if isinstance(local_loop, dict) else {}
    for key, value in snapshot_loop.items():
        if key not in loop:
            loop[key] = value
    # Controller-owned keys: the snapshot is authoritative for the exact
    # lowercase keys; a stale local value is dropped, never unioned.
    if "pause_sources" in snapshot_loop:
        raw_sources = snapshot_loop.get("pause_sources")
        loop["pause_sources"] = sorted(
            {
                source
                for source in (raw_sources if isinstance(raw_sources, list) else [])
                if isinstance(source, str) and source.strip()
            }
        )
    else:
        loop.pop("pause_sources", None)
    if "paused_by_project" in snapshot_loop:
        loop["paused_by_project"] = snapshot_loop["paused_by_project"]
    else:
        loop.pop("paused_by_project", None)
    if loop:
        merged["loop"] = loop
    else:
        merged.pop("loop", None)
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


_COMMIT_META_TIMEOUT_SECONDS = 15


def _controller_base_url() -> str:
    """The controller base URL for the conditional meta commit endpoint
    (AGENTTEAMS_CONTROLLER_URL, trailing slash stripped)."""
    return os.environ.get("AGENTTEAMS_CONTROLLER_URL", "").strip().rstrip("/")


def _controller_bearer_token() -> str:
    """The worker bearer token for the controller, in the session's
    resolution order: AGENTTEAMS_AUTH_TOKEN, then the token file
    (AGENTTEAMS_AUTH_TOKEN_FILE)."""
    token = os.environ.get("AGENTTEAMS_AUTH_TOKEN", "").strip()
    if token:
        return token
    token_file = os.environ.get("AGENTTEAMS_AUTH_TOKEN_FILE", "").strip()
    if token_file:
        try:
            token = Path(token_file).read_text(encoding="utf-8").strip()
        except OSError:
            return ""
    return token


def _remote_project_meta_etag(
    arguments: dict[str, Any], meta_path: str
) -> tuple[str | None, str | None]:
    """The ETag of the remote project meta object (filesync ``etag`` =
    ``mc stat --json``): (etag, None) when present, (None, None) when the
    object does not exist, (None, reason) when the read failed. A
    conditional commit must match the version it read, so every
    non-present outcome is a sync failure for the caller — degrading to
    an unconditional write is not an option.
    """
    stat_args = dict(arguments)
    stat_args.update({"action": "etag", "path": meta_path})
    result = _filesync(stat_args)
    if result.get("ok"):
        etag = result.get("etag")
        if etag:
            return str(etag), None
        return None, "the remote project meta stat returned no etag"
    error = str(result.get("error") or "")
    if "does not exist" in error.lower():
        return None, None
    return None, error


def _remote_project_meta_consistent_read(
    arguments: dict[str, Any], meta_path: str
) -> tuple[dict[str, Any] | None, str | None, str | None]:
    """One versioned read of the remote project meta, in the order
    stat -> cat -> stat: the body is bracketed by two version reads that
    must agree.  Returns (meta, etag, error):

    - (meta, etag, None)  -> the body was read under version ``etag``, and
      the etag is the only version this body may be merged AND committed
      with (If-Match);
    - (None, None, None)  -> the remote object does not exist (normal
      first push);
    - (None, None, reason) -> the read failed: a probe or the body read
      errored, or the version changed between the bracketing stats on
      every bounded attempt (a concurrent write is in flight).

    A body whose surrounding versions disagree is discarded and the read
    is retried, bounded to _SYNC_META_READ_ATTEMPTS.  A pause that lands
    between the body read and the trailing stat therefore shows up as a
    version change on THIS read, not as a stale body committed under a
    fresh version — the window the maintainer's #1402 P1 named.  The
    caller must not merge or commit without a consistent read: a body
    unbound from its version is exactly the clobber this exists to close.
    """
    for _ in range(_SYNC_META_READ_ATTEMPTS):
        first, error = _remote_project_meta_etag(arguments, meta_path)
        if first is None:
            if error:
                return None, None, error
            return None, None, None  # the remote object does not exist
        snapshot, error = _remote_project_meta_snapshot(arguments, meta_path)
        if error:
            return None, None, error
        second, error = _remote_project_meta_etag(arguments, meta_path)
        if error:
            return None, None, error
        if second is None or second != first:
            # Version flip between the bracketing stats (the object can
            # even disappear): this body is not bound to any version —
            # discard it and retry the read.
            continue
        return snapshot, second, None
    return None, None, (
        "the remote project meta version changed between the bracketing "
        f"stats on every consistent read attempt ({_SYNC_META_READ_ATTEMPTS}); "
        "the body was discarded rather than committed unbound from its version"
    )


def _commit_project_meta(project_id: str, meta: dict[str, Any], etag: str) -> dict[str, Any]:
    """Commit the merged meta through the controller's conditional
    endpoint: PUT {AGENTTEAMS_CONTROLLER_URL}/api/v1/projects/{id}/meta
    with If-Match: <etag> (stdlib urllib only, 15s timeout).

    Returns:

    - {"ok": True, "etag": <new>}  -> 200: the meta landed under the
      observed ETag and the response carries the new one;
    - {"ok": False, "conflict"}    -> 409: the meta changed after the
      read (a pause or another writer landed); re-snapshot, re-merge and
      retry;
    - {"ok": False, ...}           -> everything else, fail-closed: 428
      (If-Match missing/unsatisfied), 503 (the backend cannot perform the
      strict conditional write), 404/405 (the controller is too old for
      the endpoint) and network errors. The caller must never degrade to
      an unconditional push.
    """
    base = _controller_base_url()
    if not base:
        return {
            "ok": False,
            "error": "AGENTTEAMS_CONTROLLER_URL is not set; the conditional project meta commit is unavailable",
        }
    url = f"{base}/api/v1/projects/{urllib.parse.quote(project_id, safe='')}/meta"
    request = urllib.request.Request(
        url,
        data=json.dumps(meta, ensure_ascii=False).encode("utf-8"),
        method="PUT",
    )
    request.add_header("Content-Type", "application/json")
    request.add_header("If-Match", etag)
    token = _controller_bearer_token()
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=_COMMIT_META_TIMEOUT_SECONDS) as response:
            status = response.status
            body = response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code
        try:
            body = exc.read()
        except OSError:
            body = b""
    except OSError as exc:  # URLError/timeout: the controller is unreachable
        return {
            "ok": False,
            "error": f"controller unreachable for the conditional project meta commit (retryable): {exc}",
        }
    text = body.decode("utf-8", errors="replace")
    if status == 200:
        try:
            payload = json.loads(text)
        except ValueError:
            payload = {}
        new_etag = payload.get("etag") if isinstance(payload, dict) else None
        if not new_etag:
            return {
                "ok": False,
                "error": "the controller accepted the conditional meta commit but returned no etag",
            }
        return {"ok": True, "etag": str(new_etag)}
    if status == 409:
        return {
            "ok": False,
            "conflict": True,
            "error": "the project meta changed after the read (409); re-snapshot and re-commit",
        }
    if status in (404, 405):
        return {
            "ok": False,
            "error": (
                f"the controller answered {status} for PUT /api/v1/projects/<id>/meta; it "
                "predates the conditional meta commit endpoint — fail closed instead of "
                "degrading to an unconditional push"
            ),
        }
    if status == 428:
        return {
            "ok": False,
            "error": "the controller rejected the conditional meta commit (428: If-Match missing or unsatisfied)",
        }
    if status == 503:
        return {
            "ok": False,
            "error": "the controller backend cannot perform the strict conditional meta write (503)",
        }
    return {
        "ok": False,
        "error": f"the controller rejected the conditional meta commit ({status}): {text[:300]}",
    }


def _sync_project(arguments: dict[str, Any], project_id: str) -> bool:
    """Push a project's derived files and its meta.json to shared storage,
    commit-safe against a concurrent Controller pause (#1402).

    Project state is written locally by projectflow, and — unlike tasks,
    which are pushed via _sync_task — was previously only synced to MinIO at
    worker startup (mirror_all). Without this push, a Controller-level
    project API (or dashboard) would read a stale startup snapshot. Mirrors
    the _sync_task pattern.

    The conflict-bearing object (meta.json) is never written by the
    unguarded directory mirror and never by an unconditional single-file
    push; every attempt:

    1. read the remote meta ONCE, versioned (``_remote_project_meta_consistent_read``:
       filesync ``etag`` = ``mc stat --json`` -> ``cat`` -> ``etag`` again):
       the committed body and the commit's version come from the same
       consistent read — only a body whose surrounding versions match is
       merged AND committed, with that version as the If-Match value; a
       version flip between the bracketing stats discards the body and
       retries the read, bounded to _SYNC_META_READ_ATTEMPTS;
    2. merge the controller-owned pause state from that snapshot into the
       local meta and re-write it (``_merge_project_pause_state``): the
       snapshot is authoritative for ``loop.pause_sources`` /
       ``loop.paused_by_project`` (a stale local cache never re-adds a
       released source) and a remote ``paused`` status is authoritative,
       while the worker keeps the fields it owns (progress, iteration,
       task plan) — so every project writer inherits the protection;
    3. push the project directory EXCLUDING meta.json (derived files only:
       plan.md, result.md, ...);
    4. commit the merged meta conditionally against the version the
       consistent read observed (``_commit_project_meta``: PUT
       /api/v1/projects/{id}/meta with If-Match): 409 (the meta changed
       after the read — a pause or another writer) retries the whole
       versioned read + merge + commit; every other failure (428/503/404/405
       — the controller is too old or its backend cannot strict-CAS — and
       network errors) fails closed;
    5. verify the commit by re-reading the remote meta: if any pause state
       carried into the merged set is absent — or, when the merged meta
       carries no pause state at all, if the live ETag is not the one the
       commit returned (a pause landed between the commit and the re-read)
       — re-read, re-merge and retry, bounded to
       _SYNC_PROJECT_MAX_ATTEMPTS with _SYNC_PROJECT_RETRY_BACKOFFS.

    When the consistent read observes no version the two outcomes are kept
    apart: the remote object ABSENT (no read error) is the normal first
    push — there is nothing to match against, so the directory push
    INCLUDING meta.json (no conditional commit, no exclusion) is the
    commit and its result is returned; a read ERROR (a probe or body read
    failure, or the version flipping on every bounded read attempt) fails
    closed — the remote version is unknown, so a push including meta.json
    could clobber a concurrent pause, and an unbound body is never
    committed (degrading to an unconditional write, or committing a body
    unbound from its version, would re-open the exact clobber this exists
    to close).

    Returns True only when the meta commit is verified (or the project
    carries no local meta.json, in which case the directory push is the
    whole commit; or the remote meta is absent, in which case the
    first-push directory push — including meta.json — is the commit).
    An exhausted retry or a fail-closed read error returns False — never
    a silent success.
    """
    meta_path = f"shared/projects/{project_id}/meta.json"
    dir_path = f"shared/projects/{project_id}"
    state_path = _project_state_path(arguments, project_id)
    has_local_meta = state_path.is_file()

    for attempt in range(1, _SYNC_PROJECT_MAX_ATTEMPTS + 1):
        if attempt > 1:
            time.sleep(_SYNC_PROJECT_RETRY_BACKOFFS[attempt - 2])
        merged: dict[str, Any] | None = None
        etag: str | None = None
        if has_local_meta:
            # The ONE versioned read for this attempt: the snapshot for the
            # merge and the If-Match version for the commit come from the
            # same consistent read, so the committed body can never be
            # bound to a version it was not read under.
            snapshot, etag, read_error = _remote_project_meta_consistent_read(
                arguments, meta_path
            )
            if etag is None:
                if read_error is not None:
                    # A probe or the body read failed, or the version
                    # flipped on every bounded read attempt: the remote
                    # version is unknown, so a push including meta.json
                    # could clobber a concurrent pause.  Fail closed — an
                    # unbound body is never committed.
                    return False
                # The remote object is absent (the normal first push):
                # nothing to match against, so the directory push
                # INCLUDING meta.json is the commit — no conditional
                # commit, no exclusion — and its result is the result of
                # the sync.
                first_push = dict(arguments)
                first_push.update({"action": "push", "path": dir_path})
                return bool(_filesync(first_push).get("ok"))
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
        if not has_local_meta:
            return True
        commit = _commit_project_meta(project_id, merged, etag)
        if not commit.get("ok"):
            if commit.get("conflict"):
                # The meta changed after the read (a pause or another
                # writer): re-read (versioned), re-merge and re-commit
                # below.
                continue
            # 428/503/404/405, network, or an unexpected response: fail
            # closed; never degrade to an unconditional push.
            return False
        expected = _project_pause_state(merged)
        verified, _verify_error = _remote_project_meta_snapshot(arguments, meta_path)
        if _pause_state_missing(expected, verified):
            continue
        if not any(expected.values()):
            # Empty expectation: the re-read must also prove the committed
            # version is still the live one — a pause can land between the
            # commit and the re-read, and a bare "nothing expected" would
            # early-return over it.
            verified_etag, _verify_etag_error = _remote_project_meta_etag(arguments, meta_path)
            if verified_etag != commit.get("etag"):
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


