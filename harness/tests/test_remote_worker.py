"""Unit tests for the remote worker's scoped .harness/ setup push."""
from __future__ import annotations

from pathlib import Path

import pytest

from harness_worker.config import WorkerConfig
from harness_worker import remote_worker
from harness_worker.remote_worker import RemoteWorker


class FakeSync:
    """Minimal stand-in for FileSync covering what _push_harness_setup uses."""

    def __init__(self, worker_name: str, remote: dict[str, str] | None = None) -> None:
        self._prefix = f"agents/{worker_name}"
        self._remote = remote or {}

    def _ensure_alias(self) -> None:  # noqa: D401 - no-op
        pass

    def _object_path(self, key: str) -> str:
        return f"hiclaw/bucket/{key}"

    def _cat(self, key: str):
        return self._remote.get(key)


def _make_worker(tmp_path: Path, *, harnessignore: str | None = None) -> tuple[RemoteWorker, list[list[str]]]:
    config = WorkerConfig(
        worker_name="w",
        minio_endpoint="http://minio",
        minio_access_key="k",
        minio_secret_key="s",
        install_dir=tmp_path,
    )
    worker = RemoteWorker(config)
    worker.sync = FakeSync("w")

    home = config.harness_home
    home.mkdir(parents=True, exist_ok=True)
    (home / "mcp-local.json").write_text('{"mcpServers": {}}')
    (home / "claude.settings.json").write_text('{"model": "x"}')
    (home / "claudeignore").write_text("*.log")
    (home / ".env").write_text("HICLAW_WORKER_MATRIX_TOKEN=secret")
    (home / "sessions").mkdir(exist_ok=True)
    (home / "sessions" / "current").write_text("sess-123")
    if harnessignore is not None:
        (home / ".harnessignore").write_text(harnessignore)

    # Capture mc cp invocations instead of touching MinIO.
    cp_calls: list[list[str]] = []

    def fake_mc(*args: str, check: bool = True):
        cp_calls.append(list(args))

        class R:
            returncode = 0

        return R()

    remote_worker._mc = fake_mc  # type: ignore[assignment]
    return worker, cp_calls


def _pushed_rel(worker: RemoteWorker) -> set[str]:
    return set(worker._push_harness_setup(since=0))


def test_pushes_setup_files_but_never_env(tmp_path: Path) -> None:
    worker, _ = _make_worker(tmp_path)
    pushed = _pushed_rel(worker)
    assert ".harness/mcp-local.json" in pushed
    assert ".harness/claude.settings.json" in pushed
    assert ".harness/claudeignore" in pushed
    assert ".harness/sessions/current" in pushed
    # Secrets are never pushed.
    assert ".harness/.env" not in pushed
    assert not any(p.endswith(".env") for p in pushed)


def test_skips_when_remote_identical(tmp_path: Path) -> None:
    worker, _ = _make_worker(tmp_path)
    # Pretend MinIO already holds an identical mcp-local.json.
    worker.sync._remote["agents/w/.harness/mcp-local.json"] = '{"mcpServers": {}}'
    pushed = _pushed_rel(worker)
    assert ".harness/mcp-local.json" not in pushed  # unchanged → skipped
    assert ".harness/claude.settings.json" in pushed  # still changed


def test_harnessignore_opts_out_files(tmp_path: Path) -> None:
    worker, _ = _make_worker(
        tmp_path,
        harnessignore="# local-only\nmcp-local.json\nsessions/\n",
    )
    pushed = _pushed_rel(worker)
    assert ".harness/mcp-local.json" not in pushed     # file pattern
    assert ".harness/sessions/current" not in pushed   # dir pattern
    assert ".harness/claude.settings.json" in pushed   # not ignored


def test_harnessignore_itself_never_pushed(tmp_path: Path) -> None:
    worker, _ = _make_worker(tmp_path, harnessignore="nothing\n")
    pushed = _pushed_rel(worker)
    assert ".harness/.harnessignore" not in pushed


@pytest.mark.parametrize(
    "rel,patterns,expected",
    [
        ("mcp-local.json", ["mcp-local.json"], True),
        ("sessions/current", ["sessions/"], True),
        ("sessions/current", ["sessions"], True),  # first-component match
        ("claude.settings.json", ["*.json"], True),
        ("claude.settings.json", ["mcp-local.json"], False),
        ("sessions/current", ["other/"], False),
    ],
)
def test_is_ignored_matching(rel: str, patterns: list[str], expected: bool) -> None:
    assert RemoteWorker._is_ignored(rel, patterns) is expected
