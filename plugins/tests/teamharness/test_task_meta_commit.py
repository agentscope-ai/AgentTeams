"""#1471: the worker task sync commits meta.json conditionally.

The worker write-back mirrors the whole task record. Before this, that
included meta.json through a plain filesync push with no version binding, so
a sync started from a cached copy could erase what the Controller's
CancelTask had persisted (status=cancelled, cancel_reason, cancelled_at,
replacement_task_id and the resolved continuation).

Covers:
1. the steady-state sync commits meta through the conditional endpoint and
   the plain push never carries meta.json;
2. a stale local copy (in_progress) merged against a remote cancelled record
   keeps the cancellation — status, fields and continuation — and also
   repairs the local copy;
3. a 409 (the meta changed after the read) re-reads, re-merges and converges;
4. a permanent refusal (403/400/428/503) fails closed — no fallback push of
   meta.json;
5. the first creation (no remote meta) commits create-if-absent
   (If-None-Match: *) — the plain push never carries meta.json, and a
   create/cancel interleave loses the create (409) and converges on the
   cancelled record via a versioned re-read;
6. a failed version read fails closed before anything is pushed;
7. rolling upgrade (fail closed, #1498): a controller that predates the
   task-meta endpoint (404/405 with a non-endpoint body) is recognized by
   the commit client with the controller_upgrade_required error code and
   the sync FAILS closed — meta.json is not written or overwritten, the
   concurrent cancellation survives verbatim, and the stderr log carries
   the Controller-first upgrade requirement — while the endpoint's own
   JSON 404 stays a fail-closed refusal;
8. no AGENTTEAMS_CONTROLLER_URL at all (a pre-endpoint deployment): the
   commit client signals endpoint_missing + controller_upgrade_required
   for the unset URL and the same fail-closed path applies — the sync
   reports the error instead of degrading to the pre-#1471 unconditional
   push, which clobbered a concurrent cancellation with in_progress and
   reported success.
"""

from __future__ import annotations

import importlib.util
import io
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
MCP_DIR = REPO_ROOT / "plugins" / "teamharness" / "mcp"

def _load_server() -> Any:
    spec = importlib.util.spec_from_file_location(
        "teamharness_mcp_server_task_meta_test",
        MCP_DIR / "server.py",
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(MCP_DIR))
    try:
        spec.loader.exec_module(module)
    finally:
        sys.path.remove(str(MCP_DIR))
    return module


@pytest.fixture()
def server() -> Any:
    return _load_server()


class FakeTaskStorage:
    """The filesync surface `_sync_task` uses, against an in-memory remote.

    push/stat/cat/etag only; the local tree is the real one under the
    workspace so the merge writes are observable.
    """

    def __init__(self) -> None:
        self.remote: dict[str, bytes] = {}
        self.versions: dict[str, int] = {}
        self.calls: list[tuple[str, str]] = []
        self.read_error: str | None = None
        # A concurrent Controller write (e.g. CancelTask) that lands while
        # the sync is in flight: applied to the remote meta at the first
        # push — after the versioned read, before any meta write. That is
        # the read→commit window a conditional commit would have closed
        # with a 409; an old controller's unconditional push clobbers it.
        self.pending_concurrent_meta: tuple[str, dict[str, Any]] | None = None

    # -- helpers ---------------------------------------------------------
    def put(self, path: str, payload: dict[str, Any]) -> str:
        self.remote[path] = json.dumps(payload).encode("utf-8")
        self.versions[path] = self.versions.get(path, 0) + 1
        return self.etag_of(path)

    def etag_of(self, path: str) -> str:
        return f"v{self.versions.get(path, 0)}"

    def body(self, path: str) -> dict[str, Any]:
        return json.loads(self.remote[path].decode("utf-8"))

    # -- the filesync callable ------------------------------------------
    def __call__(self, arguments: dict[str, Any]) -> dict[str, Any]:
        action = str(arguments.get("action") or "")
        path = str(arguments.get("path") or "")
        self.calls.append((action, path))
        if action == "etag":
            if self.read_error is not None:
                return {"ok": False, "error": self.read_error}
            if path not in self.remote:
                return {"ok": False, "error": "object does not exist"}
            return {"ok": True, "etag": self.etag_of(path)}
        if action == "cat":
            if path not in self.remote:
                return {"ok": False, "error": "object does not exist"}
            return {"ok": True, "content": self.remote[path].decode("utf-8")}
        if action == "stat":
            return {"ok": path in self.remote}
        if action == "push":
            self._push(arguments, path)
            return {"ok": True}
        return {"ok": False, "error": f"unsupported action {action}"}

    def _push(self, arguments: dict[str, Any], path: str) -> None:
        if self.pending_concurrent_meta is not None:
            meta_path, payload = self.pending_concurrent_meta
            self.remote[meta_path] = json.dumps(payload).encode("utf-8")
            self.versions[meta_path] = self.versions.get(meta_path, 0) + 1
            self.pending_concurrent_meta = None
        workspace = Path(str(arguments["workspaceDir"]))
        exclude = set(arguments.get("exclude") or [])
        local = workspace / path
        if local.is_file():
            self.remote[path] = local.read_bytes()
            self.versions[path] = self.versions.get(path, 0) + 1
            return
        if not local.is_dir():
            return
        for candidate in sorted(local.rglob("*")):
            if not candidate.is_file():
                continue
            relative = candidate.relative_to(local)
            if candidate.name in exclude:
                continue
            key = f"{path}/{relative.as_posix()}"
            self.remote[key] = candidate.read_bytes()
            self.versions[key] = self.versions.get(key, 0) + 1

    # -- assertions ------------------------------------------------------
    def pushed_meta(self) -> list[str]:
        return [path for action, path in self.calls if action == "push" and path.endswith("/meta.json")]


def _task_meta(**overrides: Any) -> dict[str, Any]:
    meta = {
        "task_id": "t1",
        "project_id": "p1",
        "status": "in_progress",
        "assigned_to": "worker:alpha",
        "room_id": "!room:example.org",
    }
    meta.update(overrides)
    return meta


def _write_task(workspace: Path, meta: dict[str, Any], files: dict[str, str] | None = None) -> None:
    task_dir = workspace / "shared" / "tasks" / "t1"
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
    for name, content in (files or {}).items():
        (task_dir / name).write_text(content, encoding="utf-8")


def _arguments(workspace: Path) -> dict[str, Any]:
    return {"workspaceDir": str(workspace), "action": "taskflow", "payload": {"taskId": "t1"}}


def _install(monkeypatch: Any, server: Any, storage: FakeTaskStorage, commits: list[dict[str, Any]], commit_result: Any = None) -> None:
    monkeypatch.setattr(server, "_filesync", storage)

    queue = list(commit_result or [])

    def fake_commit(
        task_id: str,
        meta: dict[str, Any],
        etag: str | None,
        team: str = "",
        if_none_match: bool = False,
    ) -> dict[str, Any]:
        commits.append({
            "task_id": task_id,
            "meta": dict(meta),
            "etag": etag,
            "team": team,
            "if_none_match": if_none_match,
        })
        if queue:
            return queue.pop(0)
        path = f"shared/tasks/{task_id}/meta.json"
        if if_none_match:
            # Create-if-absent (If-None-Match: *): the object must not
            # exist; a concurrent create/cancel is a 409, never a clobber.
            if path in storage.remote:
                return {"ok": False, "conflict": True, "error": "409"}
            storage.put(path, meta)
            return {"ok": True, "etag": storage.etag_of(path)}
        # Steady state: a strict CAS against the version the read saw.
        current = storage.versions.get(path)
        if current is not None and etag != f"v{current}":
            return {"ok": False, "conflict": True, "error": "409"}
        storage.put(path, meta)
        return {"ok": True, "etag": storage.etag_of(path)}

    monkeypatch.setattr(server.sync, "_commit_task_meta", fake_commit)


def test_sync_task_commits_meta_conditionally(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """The steady-state sync: files push without meta, meta via the endpoint."""
    storage = FakeTaskStorage()
    etag = storage.put("shared/tasks/t1/meta.json", _task_meta())
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    _install(monkeypatch, server, storage, commits)

    assert server._sync_task(_arguments(tmp_path), "t1") is True

    assert storage.pushed_meta() == []
    assert any(action == "push" and "exclude" not in {} for action, _ in storage.calls)
    assert len(commits) == 1
    assert commits[0]["etag"] == etag
    assert commits[0]["meta"]["status"] == "in_progress"
    # The derived file still reached storage.
    assert "shared/tasks/t1/result.md" in storage.remote


def test_sync_task_keeps_the_remote_cancellation(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """A stale local copy cannot reopen a cancelled task."""
    cancelled = _task_meta(
        status="cancelled",
        cancel_reason="operator stop",
        cancelled_at="2026-11-01T00:00:00Z",
        continuation={"status": "resolved", "resolution": "cancelled"},
    )
    storage = FakeTaskStorage()
    storage.put("shared/tasks/t1/meta.json", cancelled)
    _write_task(tmp_path, _task_meta(), {"result.md": "late result"})
    commits: list[dict[str, Any]] = []
    _install(monkeypatch, server, storage, commits)

    assert server._sync_task(_arguments(tmp_path), "t1") is True

    committed = commits[0]["meta"]
    assert committed["status"] == "cancelled"
    assert committed["cancel_reason"] == "operator stop"
    assert committed["cancelled_at"] == "2026-11-01T00:00:00Z"
    assert committed["continuation"] == {"status": "resolved", "resolution": "cancelled"}
    # The local copy is repaired too (the next gate read sees the truth).
    local = json.loads((tmp_path / "shared" / "tasks" / "t1" / "meta.json").read_text(encoding="utf-8"))
    assert local["status"] == "cancelled"


def test_sync_task_retries_after_a_conflict(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """409 -> re-read, re-merge, re-commit."""
    storage = FakeTaskStorage()
    storage.put("shared/tasks/t1/meta.json", _task_meta())
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    _install(
        monkeypatch,
        server,
        storage,
        commits,
        commit_result=[{"ok": False, "conflict": True, "error": "409"}],
    )

    assert server._sync_task(_arguments(tmp_path), "t1") is True
    assert len(commits) == 2


def test_sync_task_fails_closed_on_a_permanent_refusal(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """403/400/428/503 -> False, and never an unconditional meta push."""
    storage = FakeTaskStorage()
    storage.put("shared/tasks/t1/meta.json", _task_meta())
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    _install(
        monkeypatch,
        server,
        storage,
        commits,
        commit_result=[{"ok": False, "error": "HTTP 403: the task cancellation is server-owned"}],
    )

    assert server._sync_task(_arguments(tmp_path), "t1") is False
    assert storage.pushed_meta() == []


def test_sync_task_first_push_commits_create_if_absent(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """No remote meta: the first creation is conditional (If-None-Match:
    *), never an unguarded directory push that could clobber a concurrent
    create + cancel."""
    storage = FakeTaskStorage()
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    _install(monkeypatch, server, storage, commits)

    assert server._sync_task(_arguments(tmp_path), "t1") is True

    # The meta reached storage ONLY through the create-if-absent commit;
    # the plain push never carried it.
    assert storage.pushed_meta() == []
    assert "shared/tasks/t1/meta.json" in storage.remote
    assert "shared/tasks/t1/result.md" in storage.remote
    assert len(commits) == 1
    assert commits[0]["if_none_match"] is True
    assert commits[0]["etag"] is None
    assert commits[0]["meta"]["status"] == "in_progress"


def test_sync_task_create_cancel_interleaving_keeps_cancellation(
    server: Any, tmp_path: Path, monkeypatch: Any
) -> None:
    """The maintainer's P1 repro, in the maintainer's order: the initial
    versioned read sees no remote meta; then another writer creates the
    task AND the Controller cancels it; the sync's first create must then
    409 (the object exists), re-read the cancelled record versioned, merge
    the cancellation, and commit it with If-Match — the cancelled state
    must survive the sync."""
    storage = FakeTaskStorage()
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    interleaved = {"done": False}

    def fake_commit(
        task_id: str,
        meta: dict[str, Any],
        etag: str | None,
        team: str = "",
        if_none_match: bool = False,
    ) -> dict[str, Any]:
        commits.append({
            "task_id": task_id,
            "meta": dict(meta),
            "etag": etag,
            "team": team,
            "if_none_match": if_none_match,
        })
        path = f"shared/tasks/{task_id}/meta.json"
        if if_none_match:
            if interleaved["done"]:
                # The object exists now: create-if-absent fails.
                return {"ok": False, "conflict": True, "error": "409"}
            interleaved["done"] = True
            # The interleave: another writer creates the task, the
            # Controller cancels it — both land before this create.
            storage.put(path, _task_meta())
            storage.put(path, _task_meta(
                status="cancelled",
                cancel_reason="operator stop",
                cancelled_at="2026-11-01T00:00:00Z",
                continuation={"status": "resolved", "resolution": "cancelled"},
            ))
            return {"ok": False, "conflict": True, "error": "409"}
        # Steady state: a strict CAS against the version the read saw.
        current = storage.versions.get(path)
        if etag != f"v{current}":
            return {"ok": False, "conflict": True, "error": "409"}
        storage.put(path, meta)
        return {"ok": True, "etag": storage.etag_of(path)}

    monkeypatch.setattr(server, "_filesync", storage)
    monkeypatch.setattr(server.sync, "_commit_task_meta", fake_commit)

    assert server._sync_task(_arguments(tmp_path), "t1") is True

    # The cancellation survived the sync, verbatim.
    final = storage.body("shared/tasks/t1/meta.json")
    assert final["status"] == "cancelled"
    assert final["cancel_reason"] == "operator stop"
    assert final["cancelled_at"] == "2026-11-01T00:00:00Z"
    assert final["continuation"] == {"status": "resolved", "resolution": "cancelled"}
    # The first attempt was create-if-absent (and lost to the interleave);
    # the converged commit was bound to the cancelled version.
    assert commits[0]["if_none_match"] is True
    assert commits[-1]["if_none_match"] is False
    assert commits[-1]["meta"]["status"] == "cancelled"
    # The plain push never carried meta.json.
    assert storage.pushed_meta() == []


def test_sync_task_fails_closed_when_endpoint_missing(
    server: Any, tmp_path: Path, monkeypatch: Any, capfd: Any
) -> None:
    """Rolling upgrade (fail closed, #1498): the controller predates the
    task-meta endpoint and answers the conditional commit with the old
    mux's plain-text 404. The sync must FAIL closed: it reports False,
    the commit client's error code is controller_upgrade_required, and
    meta.json is NOT written or overwritten — the Controller's
    cancellation, which lands after the versioned read (the window a
    conditional commit would have closed with a 409), must survive
    verbatim. The pre-#1471 fallback (an unconditional push that
    re-wrote the cancelled record with in_progress and reported success)
    is gone. The REAL _commit_task_meta runs against the faked HTTP
    layer — faking the commit client would hide the exact regression."""
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.invalid:8080")
    monkeypatch.delenv("AGENTTEAMS_AUTH_TOKEN", raising=False)
    monkeypatch.delenv("AGENTTEAMS_AUTH_TOKEN_FILE", raising=False)

    def plain_text_404(request: Any, timeout: float | None = None) -> Any:
        raise urllib.error.HTTPError(
            "http://controller.invalid:8080/api/v1/tasks/t1/meta",
            404, "404 page not found", {}, io.BytesIO(b"404 page not found\n"),
        )

    monkeypatch.setattr(urllib.request, "urlopen", plain_text_404)

    cancelled = _task_meta(
        status="cancelled",
        cancel_reason="operator stop",
        cancelled_at="2026-11-01T00:00:00Z",
        continuation={"status": "resolved", "resolution": "cancelled"},
    )
    storage = FakeTaskStorage()
    # The versioned read sees the task in flight (pre-cancellation)...
    storage.put("shared/tasks/t1/meta.json", _task_meta())
    # ...and the Controller's CancelTask lands while the sync is in
    # flight (after the read, before any meta write): the removed
    # fallback's version-less push clobbered exactly this write with
    # the local in_progress state.
    storage.pending_concurrent_meta = ("shared/tasks/t1/meta.json", cancelled)
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})

    # The error code: the real commit client against the plain-text 404.
    commit = server.sync._commit_task_meta("t1", {}, "v1")
    assert commit.get("ok") is False, commit
    assert commit.get("code") == "controller_upgrade_required", commit
    assert commit.get("endpoint_missing") is True, commit

    # The same controller behaviour, seen through the whole sync.
    monkeypatch.setattr(server, "_filesync", storage)
    assert server._sync_task(_arguments(tmp_path), "t1") is False

    # meta.json was not written or overwritten: the concurrent
    # cancellation survives verbatim (status and fields).
    final = storage.body("shared/tasks/t1/meta.json")
    assert final == cancelled
    assert final["status"] == "cancelled"
    # The plain push never carried meta.json (derived files only).
    assert storage.pushed_meta() == []
    # The Controller-first upgrade requirement is on stderr, with the
    # error code.
    captured = capfd.readouterr()
    assert "controller_upgrade_required" in captured.err
    assert "upgraded first" in captured.err


def test_sync_task_fails_closed_when_controller_url_unset(
    server: Any, tmp_path: Path, monkeypatch: Any, capfd: Any
) -> None:
    """No AGENTTEAMS_CONTROLLER_URL at all (a pre-endpoint deployment):
    the commit client signals endpoint_missing +
    controller_upgrade_required for the unset URL and the sync FAILS
    closed — meta.json is not written or overwritten, the concurrent
    cancellation survives verbatim, and the stderr log carries the
    Controller-first upgrade requirement. The pre-#1471 fallback (the
    unconditional push that clobbered a cancelled record with
    in_progress and reported success) is gone. The REAL
    _commit_task_meta runs here — faking it would hide the exact bug."""
    monkeypatch.delenv("AGENTTEAMS_CONTROLLER_URL", raising=False)
    cancelled = _task_meta(
        status="cancelled",
        cancel_reason="operator stop",
        cancelled_at="2026-11-01T00:00:00Z",
        continuation={"status": "resolved", "resolution": "cancelled"},
    )
    storage = FakeTaskStorage()
    # The versioned read sees the task in flight (pre-cancellation)...
    storage.put("shared/tasks/t1/meta.json", _task_meta())
    # ...and the Controller's CancelTask lands while the sync is in
    # flight (after the read, before any meta write) — the window the
    # removed fallback's version-less push clobbered with the local
    # in_progress state.
    storage.pending_concurrent_meta = ("shared/tasks/t1/meta.json", cancelled)
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})

    monkeypatch.setattr(server, "_filesync", storage)

    # The commit client's contract for an unset URL: the
    # controller_upgrade_required fail-closed signal.
    commit = server.sync._commit_task_meta("t1", {}, "v1")
    assert commit.get("ok") is False, commit
    assert commit.get("code") == "controller_upgrade_required", commit
    assert commit.get("endpoint_missing") is True, commit
    assert "AGENTTEAMS_CONTROLLER_URL is not set" in commit["error"], commit

    assert server._sync_task(_arguments(tmp_path), "t1") is False

    # meta.json was not written or overwritten: the concurrent
    # cancellation survives verbatim (status and fields).
    final = storage.body("shared/tasks/t1/meta.json")
    assert final == cancelled
    assert final["status"] == "cancelled"
    # The plain push never carried meta.json (derived files only).
    assert storage.pushed_meta() == []
    captured = capfd.readouterr()
    assert "controller_upgrade_required" in captured.err
    assert "AGENTTEAMS_CONTROLLER_URL is not set" in captured.err


def test_commit_task_meta_maps_old_controller_responses(server: Any, monkeypatch: Any) -> None:
    """The commit client must tell the two 404s apart: the endpoint's own
    JSON refusal (task not found / unknown team) is a fail-closed
    permanent refusal, while the old controller's mux 404/405 (plain text,
    the route does not exist) is the rolling-upgrade signal the sync
    fails closed on — controller_upgrade_required, meta.json not
    written, Controller-first upgrade requirement logged."""
    monkeypatch.setenv("AGENTTEAMS_CONTROLLER_URL", "http://controller.invalid:8080")
    monkeypatch.delenv("AGENTTEAMS_AUTH_TOKEN", raising=False)
    monkeypatch.delenv("AGENTTEAMS_AUTH_TOKEN_FILE", raising=False)
    body = {"task_id": "t1", "project_id": "p1", "status": "in_progress"}

    def ok_response(payload: bytes) -> Any:
        class _Response:
            status = 200

            def read(self) -> bytes:
                return payload

            def __enter__(self) -> Any:
                return self

            def __exit__(self, *args: Any) -> bool:
                return False

        return _Response()

    def error_response(status: int, payload: bytes) -> Any:
        return urllib.error.HTTPError(
            "http://controller.invalid:8080/api/v1/tasks/t1/meta",
            status, "error", {}, io.BytesIO(payload),
        )

    cases = [
        # (status, body, endpoint_missing)
        (404, b"404 page not found\n", True),
        (405, b"405 method not allowed\n", True),
        (404, b'{"message": "task not found"}\n', False),
    ]
    for status, payload, want_missing in cases:
        def fake_urlopen(
            request: Any, timeout: float | None = None,
            _status: int = status, _payload: bytes = payload,
        ) -> Any:
            raise error_response(_status, _payload)

        monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)
        result = server.sync._commit_task_meta("t1", body, None, if_none_match=True)
        assert result.get("ok") is False, result
        assert bool(result.get("endpoint_missing")) is want_missing, result
        # The endpoint-missing signal carries the controller_upgrade_required
        # error code; every other refusal carries none.
        assert (result.get("code") == "controller_upgrade_required") is want_missing, result
        assert "error" in result, result

    # 200: the commit lands and carries the new etag.
    monkeypatch.setattr(urllib.request, "urlopen", lambda request, timeout=None: ok_response(b'{"etag": "v9"}'))
    result = server.sync._commit_task_meta("t1", body, None, if_none_match=True)
    assert result == {"ok": True, "etag": "v9"}

    # 409: a conflict (the object appeared / changed) — never endpoint_missing.
    def conflict_urlopen(request: Any, timeout: float | None = None) -> Any:
        raise error_response(409, b'{"code": "conflict"}')

    monkeypatch.setattr(urllib.request, "urlopen", conflict_urlopen)
    result = server.sync._commit_task_meta("t1", body, "v1")
    assert result.get("ok") is False
    assert result.get("conflict") is True
    assert "endpoint_missing" not in result


def test_sync_task_fails_closed_when_the_version_read_fails(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """An unreadable version means the remote state is unknown: push nothing."""
    storage = FakeTaskStorage()
    storage.read_error = "storage backend unavailable"
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    _install(monkeypatch, server, storage, commits)

    assert server._sync_task(_arguments(tmp_path), "t1") is False
    assert storage.calls == [("etag", "shared/tasks/t1/meta.json")] * len(storage.calls)
    assert storage.remote == {}
