# -*- coding: utf-8 -*-
"""TH-11: report bodies sent through the message tool are capped at 5 lines.

- a body longer than 5 lines is truncated to the first 5 lines plus a
  pointer line ``…中间 N 行见 shared/reports/report-<digest>.md``;
- the full text is written to that path only when the message is
  actually sent (a dryRun shows the exact pointer without writing);
- the @target survives the truncation: mentions are computed on the
  full text before truncation and stay in m.mentions;
- bodies of 5 lines or fewer pass through unchanged with no warning;
- without a resolvable workspace the message passes through unchanged;
- self-trigger types (PROJECT_REQUESTED) keep their exact body.
"""

from __future__ import annotations

import hashlib
import json
import sys
import urllib.error
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


ROOM = "!report-room:matrix.local"
TARGET = f"room:{ROOM}"
LEADER = "@lead:matrix.local"


class _FakeResponse:
    def __init__(self, payload: dict[str, Any]) -> None:
        self._payload = json.dumps(payload).encode("utf-8")

    def read(self) -> bytes:
        return self._payload

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc: Any) -> bool:
        return False


class _SendCapture:
    """Stands in for urlopen on the Matrix send path (PUT only)."""

    def __init__(self) -> None:
        self.requests: list[tuple[str, dict[str, Any]]] = []

    def urlopen(self, request: Any, timeout: Any = None) -> _FakeResponse:
        url = request.full_url if hasattr(request, "full_url") else str(request)
        body = json.loads(request.data.decode("utf-8") or "{}")
        self.requests.append((url, body))
        return _FakeResponse({"event_id": "$evt-1"})


def _report_relpath(text: str) -> str:
    digest = hashlib.sha256(f"{ROOM}\0{text}".encode("utf-8")).hexdigest()[:16]
    return f"shared/reports/report-{digest}.md"


def _pointer(text: str) -> str:
    lines = text.splitlines()
    return "\n".join(lines[:5]) + f"\n…中间 {len(lines) - 5} 行见 {_report_relpath(text)}"


def _prepare(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    with_workspace: bool = True,
) -> _SendCapture | None:
    capture: _SendCapture | None = None
    for key in ("TEAMHARNESS_SHARED_DIR", "AGENTTEAMS_SHARED_DIR", "QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        monkeypatch.delenv(key, raising=False)
    if with_workspace:
        monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.local")
        monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
        capture = _SendCapture()
        monkeypatch.setattr(server.urllib.request, "urlopen", capture.urlopen)
    monkeypatch.setattr(
        server.message,
        "_record_matrix_outbound_to_session",
        lambda *a, **k: True,
    )
    return capture


def _send(
    tmp_path: Path,
    text: str,
    dry_run: bool = False,
    with_workspace_dir: bool = True,
    **extra: Any,
) -> dict[str, Any]:
    arguments: dict[str, Any] = {
        "action": "send",
        "channel": "matrix",
        "target": TARGET,
        "message": text,
    }
    if with_workspace_dir:
        arguments["workspaceDir"] = str(tmp_path)
    if dry_run:
        arguments["dryRun"] = True
    arguments.update(extra)
    return server._message(arguments)


def test_six_lines_truncated_with_pointer_and_mentions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(tmp_path, monkeypatch)
    text = "\n".join([f"{LEADER} report ready"] + [f"line {i}" for i in range(2, 7)])

    sent = _send(tmp_path, text, dry_run=True)

    assert sent["ok"] is True
    assert sent["dryRun"] is True
    assert sent["content"]["body"] == _pointer(text)
    # The @target (line 1 here) stays in the body AND in m.mentions.
    assert sent["content"]["m.mentions"] == {"user_ids": [LEADER]}
    assert sent["fullReportPath"] == _report_relpath(text)
    assert "report body exceeds 5 lines" in sent["warning"]
    # dryRun: pointer is exact, nothing written.
    assert not (tmp_path / "shared" / "reports").exists()


def test_ten_lines_pointer_reports_hidden_count(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(tmp_path, monkeypatch)
    text = "\n".join([f"{LEADER} status"] + [f"line {i}" for i in range(2, 11)])

    sent = _send(tmp_path, text, dry_run=True)

    assert sent["ok"] is True
    assert sent["content"]["body"].endswith(f"\n…中间 5 行见 {_report_relpath(text)}")
    assert sent["content"]["body"].splitlines()[-2] == "line 5"
    assert len(sent["content"]["body"].splitlines()) == 6


def test_five_lines_pass_through_without_warning(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(tmp_path, monkeypatch)
    text = "\n".join([f"{LEADER} report ready"] + [f"line {i}" for i in range(2, 6)])

    sent = _send(tmp_path, text, dry_run=True)

    assert sent["ok"] is True
    assert sent["content"]["body"] == text
    assert "fullReportPath" not in sent
    assert "warning" not in sent


def test_real_send_writes_full_report_and_puts_truncated_body(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    assert capture is not None
    text = "\n".join([f"{LEADER} report ready"] + [f"line {i}" for i in range(2, 7)])

    sent = _send(tmp_path, text)

    assert sent["ok"] is True
    assert sent["messageId"] == "$evt-1"
    url, body = capture.requests[-1]
    assert "/send/m.room.message/" in url
    assert body["body"] == _pointer(text)
    assert body["m.mentions"] == {"user_ids": [LEADER]}
    report = tmp_path / _report_relpath(text)
    assert report.read_text(encoding="utf-8") == text + "\n"


def test_mention_only_in_hidden_part_still_in_m_mentions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(tmp_path, monkeypatch)
    # The @target appears only on line 8 (hidden by the truncation).
    text = "\n".join([f"line {i}" for i in range(1, 9)] + [f"{LEADER} final"])

    sent = _send(tmp_path, text, dry_run=True)

    assert sent["ok"] is True
    assert LEADER not in sent["content"]["body"]
    assert sent["content"]["m.mentions"] == {"user_ids": [LEADER]}


@pytest.mark.parametrize("operation", ["mkdir", "write_text"])
def test_report_write_failure_returns_tool_error_without_sending(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, operation: str
) -> None:
    capture = _prepare(tmp_path, monkeypatch)
    text = "\n".join(f"line {i}" for i in range(8))

    def fail_write(*args: Any, **kwargs: Any) -> None:
        raise PermissionError("report directory is not writable")

    monkeypatch.setattr(Path, operation, fail_write)
    sent = _send(tmp_path, text)

    assert sent["ok"] is False
    assert "report" in sent["error"]
    assert capture is not None and capture.requests == []


def test_no_workspace_passes_through_unchanged(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(tmp_path, monkeypatch, with_workspace=False)
    text = "\n".join([f"{LEADER} report ready"] + [f"line {i}" for i in range(2, 7)])

    sent = _send(tmp_path, text, dry_run=True, with_workspace_dir=False)

    assert sent["ok"] is True
    assert sent["content"]["body"] == text
    assert "fullReportPath" not in sent


def test_self_trigger_type_keeps_exact_body(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    _prepare(tmp_path, monkeypatch)
    # Line 1 names the target room: the PROJECT_REQUESTED visible-route
    # contract requires replyRoute.targetSession inside the body.
    text = "\n".join([f"{ROOM} project requested"] + [f"line {i}" for i in range(2, 9)])

    sent = _send(
        tmp_path,
        "",
        dry_run=True,
        message={"type": "PROJECT_REQUESTED", "text": text},
        sender={"session": "!source:matrix.local", "agent": "@me:matrix.local"},
        agentId="@me:matrix.local",
        replyRoute={"channel": "matrix", "targetSession": ROOM, "targetUser": "@me:matrix.local"},
    )

    assert sent["ok"] is True
    assert sent["trigger"]["status"] == "requested"
    # 8 lines, but the PROJECT_REQUESTED contract keeps the exact body.
    assert sent["content"]["body"] == text
    assert "fullReportPath" not in sent
