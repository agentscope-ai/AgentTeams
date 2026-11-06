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
5. the first push (no remote meta) still pushes meta.json — there is no
   version to match;
6. a failed version read fails closed before anything is pushed.
"""

from __future__ import annotations

import importlib.util
import json
import sys
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

    def fake_commit(task_id: str, meta: dict[str, Any], etag: str, team: str = "") -> dict[str, Any]:
        commits.append({"task_id": task_id, "meta": dict(meta), "etag": etag, "team": team})
        if queue:
            return queue.pop(0)
        storage.put(f"shared/tasks/{task_id}/meta.json", meta)
        return {"ok": True, "etag": storage.etag_of(f"shared/tasks/{task_id}/meta.json")}

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


def test_sync_task_first_push_includes_meta(server: Any, tmp_path: Path, monkeypatch: Any) -> None:
    """No remote meta: nothing to match, the plain push is the commit."""
    storage = FakeTaskStorage()
    _write_task(tmp_path, _task_meta(), {"result.md": "done"})
    commits: list[dict[str, Any]] = []
    _install(monkeypatch, server, storage, commits)

    assert server._sync_task(_arguments(tmp_path), "t1") is True
    assert "shared/tasks/t1/meta.json" in storage.remote
    assert commits == []


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
