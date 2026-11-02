# shared task/project state sync and pull (W1-a2: real module)
from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.config import _runtime_team_name
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
    """Push a task's derived files and its meta.json to shared storage,
    commit-safe against a concurrent Controller cancellation (#1471).

    The conflict-bearing object (meta.json) is never written by an
    unguarded push once a remote version exists: every attempt

    1. reads the remote meta ONCE, versioned
       (``_remote_meta_consistent_read``: filesync ``etag`` =
       ``mc stat --json`` -> ``cat`` -> ``etag`` again) — the committed
       body and the commit's version come from the same consistent read,
       with that version as the If-Match value; a version flip between
       the bracketing stats discards the body and retries the read;
    2. merges the controller-owned cancellation from that snapshot into
       the local meta and re-writes it (``_merge_task_cancellation_state``):
       a snapshot with status exactly ``cancelled`` carries
       ``status`` / ``cancel_reason`` / ``cancelled_at`` /
       ``replacement_task_id`` / ``continuation`` verbatim into the
       merged body (a cached copy that still says ``submitted`` /
       ``in_progress`` must not reopen a cancelled task), while the
       worker keeps the fields it owns (result fence, progress,
       history);
    3. pushes the task files EXCLUDING meta.json (derived files only:
       spec.md, result.md, base/, result artifacts);
    4. commits the merged meta conditionally against the version the
       consistent read observed (``_commit_task_meta``: PUT
       /api/v1/tasks/{id}/meta with If-Match, ``?team=`` for
       team-scoped workspaces): 409 (the meta changed after the read —
       a cancellation or another writer) retries the whole versioned
       read + merge + commit; every other failure (400/403/404/428/503
       — noncanonical or missing status, server-owned cancellation,
       unknown task, If-Match rejected, backend without strict
       conditional writes — and network errors) fails closed;
    5. verifies the commit by re-reading the remote meta: if the merged
       meta carries a cancellation that is absent from the re-read — or,
       when it carries none, if the live ETag is not the one the commit
       returned (a cancellation landed between the commit and the
       re-read) — re-read, re-merge and retry, bounded to
       _SYNC_TASK_MAX_ATTEMPTS with _SYNC_TASK_RETRY_BACKOFFS.

    When the consistent read observes no version the two outcomes are
    kept apart: the remote object ABSENT (no read error) is the normal
    first push — there is nothing to match against, so the file push
    INCLUDING meta.json (no conditional commit, no exclusion) is the
    commit and its result is returned; a read ERROR fails closed — the
    remote version is unknown, so a push including meta.json could
    clobber a concurrent cancellation, and an unbound body is never
    committed.

    Returns True only when the meta commit is verified (or the task
    carries no local meta.json, in which case the file push is the whole
    commit; or the remote meta is absent, in which case the first-push
    file push — including meta.json — is the commit). An exhausted retry
    or a fail-closed read/commit returns False — never a silent
    success.
    """
    meta_path = f"shared/tasks/{task_id}/meta.json"
    state_path = _task_state_path(arguments, task_id)
    has_local_meta = state_path.is_file()
    # Team-scoped workspaces store tasks under teams/<team>/shared/tasks/:
    # qualify the commit with ?team= so the id resolves in this
    # workspace's own scope (the global scope sends no qualifier).
    team = str(_runtime_team_name() or "").strip()

    for attempt in range(1, _SYNC_TASK_MAX_ATTEMPTS + 1):
        if attempt > 1:
            time.sleep(_SYNC_TASK_RETRY_BACKOFFS[attempt - 2])
        merged: dict[str, Any] | None = None
        etag: str | None = None
        if has_local_meta:
            # The ONE versioned read for this attempt: the snapshot for
            # the merge and the If-Match version for the commit come
            # from the same consistent read, so the committed body can
            # never be bound to a version it was not read under.
            snapshot, etag, read_error = _remote_meta_consistent_read(arguments, meta_path)
            if etag is None:
                if read_error is not None:
                    # A probe or the body read failed, or the version
                    # flipped on every bounded read attempt: the remote
                    # version is unknown, so a push including meta.json
                    # could clobber a concurrent cancellation.  Fail
                    # closed — an unbound body is never committed.
                    return False
                # The remote object is absent (the normal first push):
                # nothing to match against, so the file push INCLUDING
                # meta.json is the commit — no conditional commit, no
                # exclusion — and its result is the result of the sync.
                return _push_task_files(
                    arguments, task_id, exclude, result_paths, include_meta=True
                )
            merged = _merge_task_cancellation_state(_read_json(state_path), snapshot)
            _write_json(state_path, merged)
        if not _push_task_files(arguments, task_id, exclude, result_paths, include_meta=False):
            return False
        if not has_local_meta:
            return True
        commit = _commit_task_meta(task_id, merged, etag, team)
        if not commit.get("ok"):
            if commit.get("conflict"):
                # The meta changed after the read (a cancellation or
                # another writer): re-read (versioned), re-merge and
                # re-commit below.
                continue
            # 400/403/404/428/503, network, or an unexpected response:
            # fail closed; never degrade to an unconditional push.
            return False
        expected = _task_cancellation_state(merged)
        verified, _verify_error = _remote_meta_snapshot(arguments, meta_path)
        if _cancellation_state_missing(expected, verified):
            continue
        if not expected["cancelled"]:
            # Empty expectation: the re-read must also prove the
            # committed version is still the live one — a cancellation
            # can land between the commit and the re-read, and a bare
            # "nothing expected" would early-return over it.
            verified_etag, _verify_etag_error = _remote_meta_etag(arguments, meta_path)
            if verified_etag != commit.get("etag"):
                continue
        return True
    return False


def _push_task_files(
    arguments: dict[str, Any],
    task_id: str,
    exclude: list[str] | None,
    result_paths: list[str] | None,
    *,
    include_meta: bool,
) -> bool:
    """The derived-file half of a task sync — everything the plain
    filesync push is allowed to write (spec.md, result.md, base/, result
    artifacts), never the meta commit.

    ``include_meta`` is the first-push mode: the remote meta does not
    exist yet, so there is no version to match against and the plain
    push INCLUDING meta.json is the commit (``_sync_task`` decides this
    from the versioned read, not here). In the steady state the
    directory mirror excludes meta.json and the single-file path list
    can never contain it: the meta reaches storage only through
    ``_commit_task_meta``.
    """
    task_prefix = f"shared/tasks/{task_id}"
    if result_paths is not None:
        local_task_dir = _task_dir(arguments, task_id)
        meta_path = f"{task_prefix}/meta.json"
        payload_paths: list[str] = []
        if (local_task_dir / "result.md").is_file():
            payload_paths.append(f"{task_prefix}/result.md")
        for path in result_paths:
            if path != meta_path and path not in payload_paths:
                payload_paths.append(path)
        for path in payload_paths:
            for action in ("push", "stat"):
                sync_args = dict(arguments)
                sync_args.update({"action": action, "path": path})
                if not _filesync(sync_args).get("ok"):
                    return False
        if include_meta:
            for action in ("push", "stat"):
                commit_args = dict(arguments)
                commit_args.update({"action": action, "path": meta_path})
                if not _filesync(commit_args).get("ok"):
                    return False
        return True
    dir_args = dict(arguments)
    dir_args.update({
        "action": "push",
        "path": task_prefix,
    })
    dir_exclude = list(exclude) if exclude is not None else []
    if not include_meta and "meta.json" not in dir_exclude:
        dir_exclude.append("meta.json")
    if dir_exclude:
        dir_args["exclude"] = dir_exclude
    return bool(_filesync(dir_args).get("ok"))


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
# #1471: the task meta commit has the same 409 semantics as the project
# commit, so it shares the same bounded retry budget (one set of
# constants for both conditional-commit flows).
_SYNC_TASK_MAX_ATTEMPTS = _SYNC_PROJECT_MAX_ATTEMPTS
_SYNC_TASK_RETRY_BACKOFFS = _SYNC_PROJECT_RETRY_BACKOFFS
# Bounded retries for the versioned meta read: a version flip between the
# bracketing stats discards the body and re-runs the read, but a writer in
# flight on every attempt must not loop the sync forever.
_SYNC_META_READ_ATTEMPTS = 3


def _remote_meta_snapshot(
    arguments: dict[str, Any], meta_path: str
) -> tuple[dict[str, Any] | None, str | None]:
    """Read the remote meta object (project or task) without touching
    local state.

    Filesync ``cat`` (``mc cat`` through the same alias/env resolution as
    pull/push): unlike ``pull`` it never writes the local tree.  Returns
    (meta, error):

    - (meta, None)  -> remote object read (possibly empty)
    - (None, None)  -> remote object does not exist (normal first push)
    - (None, reason)-> the read failed; the caller proceeds without a
      snapshot (fail-open, mirroring the single-file push guard's probe
      semantics — a transient storage outage must not wedge meta writes)
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
            return None, "remote meta is not valid JSON"
        if not isinstance(meta, dict):
            return None, "remote meta is not a JSON object"
        return meta, None
    error = str(result.get("error") or "")
    if "does not exist" in error.lower():
        return None, None
    return None, error


def _merge_project_pause_state(
    local_meta: dict[str, Any] | None,
    snapshot: dict[str, Any] | None,
    *,
    authored_status: str | None = None,
) -> dict[str, Any]:
    """Merge the controller-owned pause state from a remote snapshot into
    the local project meta (the local copy stays the write base).

    #1402 rules — the Controller is the single writer of the pause state,
    so the snapshot is authoritative for it and the local copy only owns
    the fields the worker writes:

    - project ``status``: controller-owned, and the Controller DOES
      transition it after creation (``PauseProject`` -> ``paused``,
      ``ResumeProject`` -> ``active``, both strict CAS). So an ordinary
      progress write takes the snapshot's value: a cache pulled before a
      resume must not write ``paused`` back. Two deliberate transitions
      are still allowed to move it forward, and only forward:

      * ``authored_status="paused"`` (MCP ``pause_project``): escalate to
        paused — a worker may pause, and a pause is fail-closed;
      * ``authored_status="completed"`` (MCP ``complete_project``): the
        terminal transition of the worker's own run.

      A remote ``paused`` always wins, so no client write can release a
      durable pause (an agent cannot self-resolve; the Controller's
      ``ResumeProject`` is the release) — that is the pre-existing MCP
      behaviour and it is kept.
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
    remote_status = str(snapshot.get("status") or "")
    if remote_status == "paused":
        # A durable pause is never cleared by a client write (round 10);
        # only the Controller's ResumeProject releases it.
        merged["status"] = "paused"
    elif authored_status == "paused":
        # Deliberate MCP pause: escalate to paused (fail-closed).
        merged["status"] = "paused"
    elif authored_status == "completed":
        # Deliberate MCP completion: the worker's own terminal transition.
        merged["status"] = "completed"
    elif "status" in snapshot:
        # Ordinary progress write: the snapshot owns the status. A remote
        # ``active``/``completed`` wins over a stale local ``paused``
        # (round 14) — the reverse of round 10, which the durable state
        # already covers above.
        merged["status"] = snapshot["status"]
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


def _remote_meta_etag(
    arguments: dict[str, Any], meta_path: str
) -> tuple[str | None, str | None]:
    """The ETag of the remote meta object (project or task; filesync
    ``etag`` = ``mc stat --json``): (etag, None) when present, (None,
    None) when the object does not exist, (None, reason) when the read
    failed. A conditional commit must match the version it read, so every
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
        return None, "the remote meta stat returned no etag"
    error = str(result.get("error") or "")
    if "does not exist" in error.lower():
        return None, None
    return None, error


def _remote_meta_consistent_read(
    arguments: dict[str, Any], meta_path: str
) -> tuple[dict[str, Any] | None, str | None, str | None]:
    """One versioned read of the remote meta object (project or task),
    in the order stat -> cat -> stat: the body is bracketed by two
    version reads that must agree.  Returns (meta, etag, error):

    - (meta, etag, None)  -> the body was read under version ``etag``, and
      the etag is the only version this body may be merged AND committed
      with (If-Match);
    - (None, None, None)  -> the remote object does not exist (normal
      first push);
    - (None, None, reason) -> the read failed: a probe or the body read
      errored, or the version changed between the bracketing stats on
      every bounded attempt (a concurrent write is in flight).

    A body whose surrounding versions disagree is discarded and the read
    is retried, bounded to _SYNC_META_READ_ATTEMPTS.  A pause or
    cancellation that lands between the body read and the trailing stat
    therefore shows up as a version change on THIS read, not as a stale
    body committed under a fresh version — the window the maintainer's
    #1402 P1 named.  The caller must not merge or commit without a
    consistent read: a body unbound from its version is exactly the
    clobber this exists to close.
    """
    for _ in range(_SYNC_META_READ_ATTEMPTS):
        first, error = _remote_meta_etag(arguments, meta_path)
        if first is None:
            if error:
                return None, None, error
            return None, None, None  # the remote object does not exist
        snapshot, error = _remote_meta_snapshot(arguments, meta_path)
        if error:
            return None, None, error
        second, error = _remote_meta_etag(arguments, meta_path)
        if error:
            return None, None, error
        if second is None or second != first:
            # Version flip between the bracketing stats (the object can
            # even disappear): this body is not bound to any version —
            # discard it and retry the read.
            continue
        return snapshot, second, None
    return None, None, (
        "the remote meta version changed between the bracketing "
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


# --- #1471: the task-level counterpart of the project conditional commit.
# The conflict-bearing object is the task meta.json — the object the
# Controller rewrites under a strict ETag CAS (PutObjectIfMatchStrict, both
# the CancelTask write and the conditional commit endpoint PUT
# /api/v1/tasks/{id}/meta).  _sync_task must therefore never commit it
# through an unguarded push: it snapshots the remote meta, merges the
# controller-owned cancellation, commits the merged meta conditionally
# against the observed ETag, and verifies the commit with a bounded retry.
#
# taskCancellationFields are the fields CancelTask writes in the same
# read-modify-write batch as status=cancelled. A cancelled task is
# terminal, so a later commit may keep them but never change or drop them
# — the same server-owned rule the Go endpoint enforces (403 on a change).
_TASK_CANCELLATION_FIELDS = ("cancel_reason", "cancelled_at", "replacement_task_id")


def _merge_task_cancellation_state(
    local_meta: dict[str, Any] | None,
    snapshot: dict[str, Any] | None,
) -> dict[str, Any]:
    """Merge the controller-owned cancellation from a remote snapshot into
    the local task meta (the local copy stays the write base).

    #1471 rules — a cancelled task is terminal and the Controller is the
    single writer of the cancellation state, so the snapshot is
    authoritative for exactly the fields it owns:

    - snapshot ``status`` exactly ``cancelled``: the merged body carries
      ``status="cancelled"`` plus the snapshot's ``cancel_reason`` /
      ``cancelled_at`` / ``replacement_task_id`` / ``continuation``
      VERBATIM. A stale local cache that still says ``submitted`` /
      ``in_progress`` must not reopen the task, and a continuation the
      worker authored (its own completed run) is superseded by the
      server-owned ``{status: "resolved", resolution: "cancelled"}`` one;
    - otherwise: the local copy is the base unchanged — the worker owns
      the rest (result fence, progress, transition history) and no other
      controller-side writer moves a task's status.
    """
    merged = dict(local_meta or {})
    if snapshot is None:
        return merged
    if str(snapshot.get("status") or "") != "cancelled":
        return merged
    merged["status"] = "cancelled"
    for field in _TASK_CANCELLATION_FIELDS:
        if field in snapshot:
            merged[field] = snapshot[field]
    if "continuation" in snapshot:
        merged["continuation"] = snapshot["continuation"]
    return merged


def _task_cancellation_state(meta: dict[str, Any] | None) -> dict[str, Any]:
    """The cancellation state a committed task meta must retain.

    Unset when the meta carries no cancellation, so the verify step is
    vacuous for a write that authored and observed none (the empty case
    still re-checks the live ETag — see ``_sync_task``)."""
    meta = meta if isinstance(meta, dict) else {}
    if meta.get("status") != "cancelled":
        return {"cancelled": False, "fields": {}, "continuation": None}
    fields = {field: meta[field] for field in _TASK_CANCELLATION_FIELDS if field in meta}
    return {
        "cancelled": True,
        "fields": fields,
        "continuation": meta.get("continuation"),
    }


def _cancellation_state_missing(
    expected: dict[str, Any],
    remote_meta: dict[str, Any] | None,
) -> bool:
    """True when any cancellation state expected in the committed meta is
    absent from the remote copy — or the remote copy could not be read at
    all (``remote_meta is None``: a verified commit is required, so an
    unverifiable one counts as missing)."""
    if remote_meta is None:
        return True
    if not expected["cancelled"]:
        return False
    if remote_meta.get("status") != "cancelled":
        return True
    for field, value in expected["fields"].items():
        if field not in remote_meta or remote_meta[field] != value:
            return True
    if expected["continuation"] is not None and remote_meta.get("continuation") != expected["continuation"]:
        return True
    return False


def _commit_task_meta(
    task_id: str, meta: dict[str, Any], etag: str, team: str = ""
) -> dict[str, Any]:
    """Commit the merged meta through the controller's conditional
    endpoint: PUT {AGENTTEAMS_CONTROLLER_URL}/api/v1/tasks/{id}/meta with
    If-Match: <etag> (stdlib urllib only, 15s timeout).

    ``team`` (the workspace's runtime team name, empty for the global
    scope) is sent as the ``?team=`` qualifier so a task id that exists in
    several teams resolves in this workspace's own scope.

    Returns:

    - {"ok": True, "etag": <new>}  -> 200: the meta landed under the
      observed ETag and the response carries the new one;
    - {"ok": False, "conflict"}    -> 409: the meta changed after the
      read (a cancellation or another writer landed); re-snapshot,
      re-merge and retry;
    - {"ok": False, ...}           -> everything else, fail-closed: 400
      (noncanonical or missing status), 403 (the cancellation is
      server-owned / the task is outside this caller's team), 404 (the
      controller predates the endpoint or the task is unknown), 428
      (If-Match missing/unsatisfied), 503 (the backend cannot perform the
      strict conditional write) and network errors. The caller must never
      degrade to an unconditional push.
    """
    base = _controller_base_url()
    if not base:
        return {
            "ok": False,
            "error": "AGENTTEAMS_CONTROLLER_URL is not set; the conditional task meta commit is unavailable",
        }
    url = f"{base}/api/v1/tasks/{urllib.parse.quote(task_id, safe='')}/meta"
    if team:
        url += f"?team={urllib.parse.quote(team, safe='')}"
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
            "error": f"controller unreachable for the conditional task meta commit (retryable): {exc}",
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
            "error": "the task meta changed after the read (409); re-snapshot and re-commit",
        }
    if status in (400, 403, 404, 428, 503):
        # Every one of these is a permanent refusal for THIS body/version
        # (noncanonical or missing status, server-owned cancellation,
        # unknown task, If-Match rejected, backend without strict
        # conditional writes): fail closed; never degrade to an
        # unconditional push, and never retry the same body.
        return {
            "ok": False,
            "error": f"the controller rejected the conditional task meta commit ({status}): {text[:300]}",
        }
    return {
        "ok": False,
        "error": f"the controller rejected the conditional task meta commit ({status}): {text[:300]}",
    }


def _sync_project(
    arguments: dict[str, Any],
    project_id: str,
    *,
    authored_status: str | None = None,
) -> bool:
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

    1. read the remote meta ONCE, versioned (``_remote_meta_consistent_read``:
       filesync ``etag`` = ``mc stat --json`` -> ``cat`` -> ``etag`` again):
       the committed body and the commit's version come from the same
       consistent read — only a body whose surrounding versions match is
       merged AND committed, with that version as the If-Match value; a
       version flip between the bracketing stats discards the body and
       retries the read, bounded to _SYNC_META_READ_ATTEMPTS;
    2. merge the controller-owned pause state from that snapshot into the
       local meta and re-write it (``_merge_project_pause_state``): the
       snapshot is authoritative for ``status`` / ``loop.pause_sources`` /
       ``loop.paused_by_project`` (a stale local cache never re-adds a
       released source or a resumed status), except for the two deliberate
       forward transitions (``authored_status`` = ``paused`` from MCP
       pause, ``completed`` from MCP complete), while the worker keeps the
       fields it owns (progress, iteration, task plan) — so every project
       writer inherits the protection;
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
            snapshot, etag, read_error = _remote_meta_consistent_read(
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
            merged = _merge_project_pause_state(
                _read_json(state_path), snapshot, authored_status=authored_status
            )
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
        verified, _verify_error = _remote_meta_snapshot(arguments, meta_path)
        if _pause_state_missing(expected, verified):
            continue
        if not any(expected.values()):
            # Empty expectation: the re-read must also prove the committed
            # version is still the live one — a pause can land between the
            # commit and the re-read, and a bare "nothing expected" would
            # early-return over it.
            verified_etag, _verify_etag_error = _remote_meta_etag(arguments, meta_path)
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


def _remote_task_meta_etag(
    arguments: dict[str, Any], task_id: str
) -> tuple[str | None, str | None]:
    """The ETag of the task meta object (filesync ``etag`` = ``mc stat``).

    (etag, None) present · (None, None) absent · (None, reason) probe
    failure. The task meta is the object every cross-actor task write
    rewrites, so the commit guard compares the version it read with the
    version present immediately before the mirror.
    """
    stat_args = dict(arguments)
    stat_args.update({"action": "etag", "path": f"shared/tasks/{task_id}/meta.json"})
    result = _filesync(stat_args)
    if result.get("ok"):
        etag = result.get("etag")
        if etag:
            return str(etag), None
        return None, "the remote task meta stat returned no etag"
    error = str(result.get("error") or "")
    if "does not exist" in error.lower():
        return None, None
    return None, error


def _remote_task_meta_body(arguments: dict[str, Any], task_id: str) -> dict[str, Any] | None:
    """Read the committed task meta without touching local state."""
    cat_args = dict(arguments)
    cat_args.update({"action": "cat", "path": f"shared/tasks/{task_id}/meta.json"})
    result = _filesync(cat_args)
    if not result.get("ok"):
        return None
    content = result.get("content")
    if not content:
        return None
    try:
        body = json.loads(content)
    except ValueError:
        return None
    return body if isinstance(body, dict) else None


_TASK_META_READ_ATTEMPTS = 3


def _load_authoritative_task_versioned(
    arguments: dict[str, Any], task_id: str
) -> tuple[dict[str, Any], str | None]:
    """Authoritative task plus the ETag of the version it was read under,
    bound together by a bracket read (stat -> pull -> stat).

    The bracket invariant (#1467 round 17 window): ``mc`` has no atomic
    read-with-version, so the pulled body is bracketed by two version
    reads that must agree. Only when the trailing stat repeats the
    leading one is the pulled body the object of that exact version —
    on MinIO every put is a new object version with a new etag, so an
    unchanged etag across the pull proves the body came from that
    version. A body read under a version the trailing stat no longer
    reports is discarded, and the whole bracket is re-read, bounded to
    _TASK_META_READ_ATTEMPTS (a writer in flight on every attempt must
    not loop the read forever).

    This closes the window the previous order left open: the body was
    pulled FIRST and stat-ed AFTER, so a freeze committed between the
    pull and the stat bound the OLD body to the NEW etag — the pre-write
    stat and the post-write read-back both then matched the peer's
    version, and the whole-directory mirror erased the freeze with a
    silent success.

    Every failure is fail-closed — there is no unbound fallback, because
    the guarded commit path writes the whole task directory on top of
    whatever the base is:

    - stat #1 probe error -> refuse: the remote version is unknown, so
      the base cannot be verified (transport errors included);
    - stat #1 confirms the meta absent -> "task not found" (the guarded
      commit path only writes a task that already exists in shared
      storage);
    - pull failure -> refuse: writing on a stale local copy would
      clobber whatever a newer writer persisted;
    - stat #2 probe error -> refuse (same as stat #1);
    - stat #2 != stat #1 (or the object disappeared) -> discard this
      body and re-run the bracket.
    """
    for _ in range(_TASK_META_READ_ATTEMPTS):
        first, error = _remote_task_meta_etag(arguments, task_id)
        if error:
            raise ValueError(
                "cannot read the authoritative task version from shared storage; "
                "refusing to write on an unverified base (retry)"
            )
        if first is None:
            raise ValueError("task not found")
        if not _pull_task(arguments, task_id):
            raise ValueError(
                "cannot pull the authoritative task state from shared storage; "
                "refusing to write on a stale local copy (retry)"
            )
        second, error = _remote_task_meta_etag(arguments, task_id)
        if error:
            raise ValueError(
                "cannot read the authoritative task version from shared storage; "
                "refusing to write on an unverified base (retry)"
            )
        if second is None or second != first:
            # A peer committed between the bracketing stats (the object
            # can even disappear): the pulled body is not bound to any
            # version — discard it and re-read the whole bracket.
            continue
        task = _read_json(_task_state_path(arguments, task_id))
        if not task:
            raise ValueError("task not found")
        if not task.get("task_id") and task.get("taskId"):
            task["task_id"] = str(task["taskId"])
        return task, second
    raise ValueError(
        "cannot bind the task body to the version it was read from: "
        "the task changed concurrently on every read attempt; refusing "
        "to write on an unverified base (retry)"
    )


def _task_meta_commit_landed(
    arguments: dict[str, Any], task_id: str, task: dict[str, Any]
) -> bool:
    """True when the committed remote meta carries exactly this task.

    Directory mirroring is not atomic, so a peer commit can land between
    the guard stat and the mirror. The post-commit read is the only place
    that can tell whose write is the current one: a mismatch means this
    attempt did not land and the caller must reload, re-apply and retry.
    """
    remote = _remote_task_meta_body(arguments, task_id)
    if remote is None:
        return False
    return json.dumps(remote, sort_keys=True, ensure_ascii=False) == json.dumps(
        task, sort_keys=True, ensure_ascii=False
    )


_TASK_COMMIT_ATTEMPTS = 3


def _commit_task_guarded(
    arguments: dict[str, Any],
    task_id: str,
    mutate: Callable[[dict[str, Any]], Any],
    *,
    action: str,
    exclude: list[str] | None = None,
    result_paths: list[str] | None = None,
) -> tuple[dict[str, Any], Any, bool]:
    """Load → mutate → commit a task with a version guard (#1467 round 17,
    round 19: the commit itself is conditional).

    The commit mirrors the WHOLE task directory, so writing on a base a
    peer has already replaced erases that peer's write — the reported case
    is a freeze committed between the authoritative pull and the mirror.
    Every attempt reloads the authoritative task (the stat -> pull -> stat
    bracket binds the body to the version it was read under), re-applies
    the caller's mutation on the fresh base, refuses to proceed when the
    base moved before the write (the pre-write stat, kept as a fast
    signal), pushes the derived files WITHOUT the meta, and commits the
    meta conditionally against the version the bracket read observed
    (``_commit_task_meta``, If-Match). The conditional commit is the
    authoritative landing check — there is no post-write read-back: a 409
    means a peer committed during the commit window (from the pre-write
    stat to the endpoint), so the attempt re-reads and re-applies on the
    peer's version; any other refusal fails closed instead of degrading
    to an unconditional push. Exhausted attempts fail closed; a silent
    overwrite is never an outcome.
    """
    team = str(_runtime_team_name() or "").strip()
    last_conflict = "the task changed concurrently"
    for _attempt in range(1, _TASK_COMMIT_ATTEMPTS + 1):
        task, base_etag = _load_authoritative_task_versioned(arguments, task_id)
        result = mutate(task)
        current, error = _remote_task_meta_etag(arguments, task_id)
        if error:
            raise ValueError(
                f"{action}: cannot verify the authoritative task version before "
                "the commit; refusing to write (retry)"
            )
        if base_etag is not None and current != base_etag:
            last_conflict = "a peer committed a newer task version during the commit window"
            continue
        _write_task(arguments, task)
        if not _push_task_files(
            arguments,
            task_id,
            exclude=exclude or [],
            result_paths=result_paths,
            include_meta=False,
        ):
            raise ValueError(f"{action}: task sync failed (retry)")
        commit = _commit_task_meta(task_id, task, base_etag, team)
        if not commit.get("ok"):
            if commit.get("conflict"):
                last_conflict = "a peer committed a newer task version during the commit window"
                continue
            raise ValueError(
                f"{action}: the conditional task meta commit failed: "
                f"{commit.get('error') or 'unknown controller error'}; failing "
                "closed (never degrading to an unconditional push)"
            )
        return task, result, True
    raise ValueError(f"{action}: {last_conflict}; reload and retry")


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


