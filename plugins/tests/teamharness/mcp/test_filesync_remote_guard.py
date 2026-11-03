"""B13 stale-overwrite guard tests for the filesync single-file push path.

2026-09-21 incident: a stale local view pushed over newer MinIO state
(meta.json false state, noticed 14.7h later). ``_filesync`` now probes the
remote object with ``mc stat --json`` before ``mc cp`` and refuses to
overwrite a remote copy newer than the local file beyond a 5s clock
tolerance; probe/parse failures pass the push through with a warning.

The ``mc`` binary is faked at ``subprocess.run`` (same pattern as
test_continuation.py); no network or real storage is needed.
"""

from __future__ import annotations

import json
import os
from datetime import datetime, timezone
from pathlib import Path
import subprocess
import sys
from typing import Any

import pytest

MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402

BASE = 1760000000.0  # 2025-10-09T06:13:20Z
PATH = "shared/projects/p1/meta.json"


@pytest.fixture(autouse=True)
def _isolated_storage_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Pin storage-prefix resolution so the remote path is deterministic."""
    for key in (
        "TEAMHARNESS_RUNTIME_CONFIG",
        "AGENTTEAMS_SHARED_STORAGE_PREFIX",
        "AGENTTEAMS_STORAGE_PREFIX",
    ):
        monkeypatch.delenv(key, raising=False)


def _completed(returncode: int = 0, stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(args=["mc"], returncode=returncode, stdout=stdout, stderr=stderr)


def _iso(epoch: float) -> str:
    return datetime.fromtimestamp(epoch, tz=timezone.utc).isoformat()


def _nano_z(epoch: float) -> str:
    """MinIO-style RFC3339Nano with Z suffix (sub-millisecond precision)."""
    stamp = datetime.fromtimestamp(epoch, tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
    return f"{stamp}.123456789Z"


def _mc_date_line(epoch: float) -> str:
    """The `mc stat` text Date: value, e.g. '2025-10-09 06:15:00 UTC'."""
    return datetime.fromtimestamp(epoch, tz=timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")


class McRunner:
    """Fake ``subprocess.run`` dispatching on the mc subcommand."""

    def __init__(
        self,
        stat_json: subprocess.CompletedProcess[str] | None,
        stat_text: subprocess.CompletedProcess[str] | None = None,
        cp: subprocess.CompletedProcess[str] | None = None,
    ) -> None:
        self.stat_json = stat_json
        self.stat_text = stat_text
        self.cp = cp if cp is not None else _completed(0, stdout="`local` -> `remote`\n")
        self.calls: list[list[str]] = []

    def __call__(self, command: list[str], **_kwargs: Any) -> subprocess.CompletedProcess[str]:
        self.calls.append(list(command))
        if command[:3] == ["mc", "stat", "--json"]:
            assert self.stat_json is not None, "stat probe was not expected here"
            return self.stat_json
        if command[:2] == ["mc", "stat"]:
            assert self.stat_text is not None, "text stat fallback was not expected here"
            return self.stat_text
        if command[:2] == ["mc", "cp"]:
            return self.cp
        raise AssertionError(f"unexpected mc command: {command}")

    @property
    def cp_calls(self) -> list[list[str]]:
        return [call for call in self.calls if call[:2] == ["mc", "cp"]]


def _write_local(workspace: Path, epoch: float) -> Path:
    local = workspace / PATH
    local.parent.mkdir(parents=True, exist_ok=True)
    local.write_text(json.dumps({"project_id": "p1"}), encoding="utf-8")
    os.utime(local, (epoch, epoch))
    return local


def _push(workspace: Path, dry_run: bool = False) -> dict[str, Any]:
    arguments: dict[str, Any] = {"action": "push", "path": PATH, "workspaceDir": str(workspace)}
    if dry_run:
        arguments["dryRun"] = True
    return server._filesync(arguments)


def test_push_rejects_newer_remote_and_skips_cp(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    raw = _nano_z(BASE + 100)
    runner = McRunner(stat_json=_completed(0, stdout=json.dumps({"lastModified": raw})))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is False
    assert result["conflict"] is True
    assert result["tool"] == "filesync"
    assert result["action"] == "push"
    assert result["path"] == PATH
    assert result["remotePath"] == PATH
    assert result["remoteLastModified"] == raw
    assert result["localMtime"].startswith(_iso(BASE))
    assert result["error"] == "remote copy is newer than the local file; pull before pushing again"
    assert set(result) == {
        "ok", "conflict", "tool", "action", "path", "remotePath",
        "localMtime", "remoteLastModified", "error",
    }
    assert runner.cp_calls == []


@pytest.mark.parametrize(
    ("delta", "conflicts"),
    [(4, False), (6, True)],
    ids=["within_tolerance", "beyond_tolerance"],
)
def test_push_clock_tolerance_boundary(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    delta: int,
    conflicts: bool,
) -> None:
    runner = McRunner(stat_json=_completed(0, stdout=json.dumps({"lastModified": _iso(BASE + delta)})))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    if conflicts:
        assert result["ok"] is False
        assert result["conflict"] is True
        assert runner.cp_calls == []
    else:
        assert result["ok"] is True
        assert len(runner.cp_calls) == 1


def test_push_allows_older_remote(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    runner = McRunner(stat_json=_completed(0, stdout=json.dumps({"lastModified": _iso(BASE - 100)})))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is True
    assert len(runner.cp_calls) == 1
    assert "warning" not in result


def test_push_allows_absent_remote_without_warning(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    runner = McRunner(stat_json=_completed(
        1, stderr="<ERROR> Object or prefix 'shared/projects/p1/meta.json' does not exist.\n"
    ))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is True
    assert len(runner.cp_calls) == 1
    assert "warning" not in result


def test_push_stale_edited_local_passes_guard_documented_limitation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Stale-read/edit/push sequence (documented limitation, pinned).

    Timeline: A pulls v1 (local mtime = BASE); B publishes v2 to the
    remote (remote lastModified = BASE + 100); A then edits its stale v1
    copy (local mtime = BASE + 200, *after* B's write).  The guard
    compares the local last edit time against the remote write time and
    sees local-newer, so it permits the push — overwriting v2.

    Closing this case requires a recorded remote baseline/version or a
    conditional put (follow-up); this test pins the current best-effort
    behavior for unedited copies and makes the gap explicit.
    """
    runner = McRunner(stat_json=_completed(0, stdout=json.dumps({"lastModified": _iso(BASE + 100)})))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE + 200)

    result = _push(tmp_path)

    # Current behavior: the guard passes (local edit is newer than the
    # remote write) and the overwrite proceeds.  Do NOT change this to a
    # rejection without also implementing the baseline/version follow-up.
    assert result["ok"] is True
    assert len(runner.cp_calls) == 1
    assert "warning" not in result


def test_push_probe_failure_passes_through_with_warning(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    runner = McRunner(stat_json=_completed(
        1, stderr="<ERROR> dial tcp 10.0.0.1:9000: connect: connection refused\n"
    ))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is True
    assert len(runner.cp_calls) == 1
    assert "remote stat probe failed" in result["warning"]


def test_push_unparseable_remote_mtime_passes_with_warning(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    runner = McRunner(
        stat_json=_completed(0, stdout=json.dumps({"Size": 123})),
        stat_text=_completed(0, stdout="Name      : meta.json\nSize      : 123 B\n"),
    )
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is True
    assert len(runner.cp_calls) == 1
    assert "lastModified" in result["warning"]


def test_push_text_date_fallback_rejects_newer_remote(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    date_line = _mc_date_line(BASE + 100)
    runner = McRunner(
        stat_json=_completed(0, stdout="not-json{"),
        stat_text=_completed(0, stdout=f"Name      : meta.json\nDate      : {date_line}\nSize      : 1 B\n"),
    )
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is False
    assert result["conflict"] is True
    assert result["remoteLastModified"] == date_line
    assert runner.cp_calls == []


def test_push_cp_failure_keeps_guard_warning(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    runner = McRunner(
        stat_json=_completed(1, stderr="<ERROR> dial tcp 10.0.0.1:9000: connect: connection refused\n"),
        cp=_completed(1, stderr="<ERROR> InsufficientStorage: Bucket storage is on its full capacity\n"),
    )
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path)

    assert result["ok"] is False
    assert "InsufficientStorage" in result["error"]
    assert "remote stat probe failed" in result["warning"]


def test_push_dry_run_does_not_probe(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    runner = McRunner(stat_json=_completed(0, stdout=json.dumps({"lastModified": _nano_z(BASE + 100)})))
    monkeypatch.setattr(server.subprocess, "run", runner)
    _write_local(tmp_path, BASE)

    result = _push(tmp_path, dry_run=True)

    assert result["ok"] is True
    assert result["dryRun"] is True
    assert runner.calls == []


# --- known-limitation regression (review follow-up, 2026-10-04) -----------
#
# The guard compares the remote lastModified with the local file's LAST
# EDIT TIME, not the version the local file was derived from:
#   1. A pulls version 1            (local mtime = T1)
#   2. B publishes version 2        (remote lastModified = T2 > T1)
#   3. A edits its stale version 1  (local mtime = T3 > T2)
# The guard now sees the local file as newer and lets the push through,
# overwriting version 2. Catching this needs a remote baseline captured at
# pull time (tracked separately); this test pins the CURRENT best-effort
# behavior so the limitation is explicit and a future fix has a regression
# to update.
def test_stale_read_edit_push_is_not_detected_known_limitation(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    # Remote holds version 2, published at BASE + 100.
    runner = McRunner(stat_json=_completed(0, stdout=json.dumps({"lastModified": _iso(BASE + 100)})))
    monkeypatch.setattr(server.subprocess, "run", runner)
    local = _write_local(tmp_path, BASE)  # A's pull of version 1 (mtime = BASE)
    # A edits its stale copy AFTER B's publish: content changes, mtime
    # moves to BASE + 200 (newer than the remote's BASE + 100).
    local.write_text(
        json.dumps({"project_id": "p1", "note": "stale edit"}), encoding="utf-8"
    )
    os.utime(local, (BASE + 200, BASE + 200))

    result = _push(tmp_path)

    # Current best-effort behavior: the guard cannot tell the local file
    # is based on a stale version (its mtime is newer than the remote), so
    # the push proceeds. Documented limitation, not a guard pass/fail.
    assert result["ok"] is True
    assert len(runner.cp_calls) == 1
    assert "warning" not in result
