"""Artifact publish must notify the room with an @ summary event.

The m.file event alone notifies nobody; every TeamHarness file publish
sends a companion m.text event with m.mentions (MSC3952) + matrix.to
pill built by ``_matrix_content``. Transaction ids are derived from the
file event txn (``<file-txn>`` + ``-summary``) so a retried publish is
deduplicated at the homeserver instead of emitting a second summary.
"""

from __future__ import annotations

import hashlib
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os
from pathlib import Path
import re
import sys
import threading
import urllib.error
import urllib.parse
from typing import Any

import pytest

MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


ROOM = "!task:example.test"
LEADER = "@leader:example.test"


class _MatrixState:
    def __init__(self) -> None:
        self.uploads: list[tuple[str, bytes]] = []
        self.events: list[dict[str, Any]] = []
        self.seen_txns: dict[str, str] = {}
        self.room_events: dict[str, dict[str, str]] = {}
        # opt-in per-test behaviors; the defaults keep the stable-MXC mode
        self.fresh_mxc_each_upload = False
        self.drop_next_put_response = False


def _make_matrix_server(monkeypatch: pytest.MonkeyPatch, workspace: Path, state: _MatrixState):
    """Fake Matrix homeserver with REAL transaction-id dedup semantics.

    Uploads return a stable mxc per (filename, content) so a retried
    publish of the same file re-uploads to the same media URI. /send
    with a known txn returns the recorded event id without creating a
    second event (Matrix idempotency).
    """

    class _Handler(BaseHTTPRequestHandler):
        def _reply(self, code: int, payload: dict[str, Any]) -> None:
            data = json.dumps(payload).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_POST(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path != "/_matrix/media/v3/upload":
                self._reply(404, {})
                return
            query = urllib.parse.parse_qs(parsed.query)
            filename = query.get("filename", ["artifact"])[0]
            state.uploads.append((filename, body))
            digest = hashlib.sha256(
                f"{filename}:{body.decode('utf-8', 'replace')}".encode("utf-8")
            ).hexdigest()[:16]
            if state.fresh_mxc_each_upload:
                # real homeservers may mint a new media URI per upload
                digest = f"{digest}-{len(state.uploads)}"
            self._reply(200, {"content_uri": f"mxc://example.test/{digest}"})

        def do_GET(self) -> None:  # noqa: N802
            parts = [urllib.parse.unquote(part) for part in urllib.parse.urlparse(self.path).path.split("/") if part]
            # /_matrix/client/v3/rooms/<room>/event/<id>
            if len(parts) == 7 and parts[0] == "_matrix" and parts[5] == "event":
                event = state.room_events.get(parts[6])
                if event:
                    self._reply(200, event)
                    return
            self._reply(404, {})

        def do_PUT(self) -> None:  # noqa: N802
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length)
            parsed = urllib.parse.urlparse(self.path)
            if "/send/m.room.message/" not in parsed.path:
                self._reply(404, {})
                return
            txn = parsed.path.rsplit("/", 1)[-1]
            if txn in state.seen_txns:
                self._reply(200, {"event_id": state.seen_txns[txn]})
                return
            event_id = f"$evt-{len(state.events) + 1}"
            state.seen_txns[txn] = event_id
            state.events.append({"txn": txn, "content": json.loads(body.decode("utf-8") or "{}")})
            if state.drop_next_put_response:
                # the server processed the PUT but the client loses the
                # response (connection closed without a reply)
                state.drop_next_put_response = False
                self.close_connection = True
                return
            self._reply(200, {"event_id": event_id})

        def log_message(self, *args: Any) -> None:  # silence
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", f"http://127.0.0.1:{httpd.server_address[1]}")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setenv("TEAMHARNESS_MATRIX_CONTEXT_FILE", str(workspace / "no-matrix-context.json"))
    monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
    return httpd


@pytest.fixture()
def matrix(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    state = _MatrixState()
    httpd = _make_matrix_server(monkeypatch, tmp_path, state)
    (tmp_path / "reports").mkdir(parents=True, exist_ok=True)
    (tmp_path / "reports" / "summary.md").write_text("The result is ready.\n" * 10, encoding="utf-8")
    yield state
    httpd.shutdown()


def _publish(workspace: Path, **overrides: Any) -> dict[str, Any]:
    arguments: dict[str, Any] = {
        "workspaceDir": str(workspace),
        "action": "publish_file",
        "path": "reports/summary.md",
        "target": f"room:{ROOM}",
        "filename": "summary.md",
    }
    arguments.update(overrides)
    response = server.call_tool("artifact", arguments)
    return json.loads(response["content"][0]["text"])


def _file_and_summary(state: _MatrixState, offset: int = 0) -> tuple[dict[str, Any], dict[str, Any]]:
    """The file event and its summary at the tail (or at a fixed offset)."""
    file_event = state.events[-2 + offset]
    summary_event = state.events[-1 + offset]
    assert file_event["content"].get("msgtype") == "m.file", file_event
    assert summary_event["content"].get("msgtype") == "m.text", summary_event
    return file_event, summary_event


def test_publish_sends_file_event_plus_mention_summary(matrix: _MatrixState, tmp_path: Path) -> None:
    """① valid MXID in m.mentions.user_ids, ② matrix.to pill in formatted_body."""
    published = _publish(tmp_path, mentions=[LEADER])
    assert published["ok"] is True
    artifact = published["artifact"]
    assert artifact["status"] == "published"
    assert artifact["mxcUri"].startswith("mxc://example.test/")
    assert artifact["eventId"]

    file_event, summary_event = _file_and_summary(matrix)
    content = summary_event["content"]
    assert content.get("m.mentions", {}).get("user_ids") == [LEADER]
    # matrix.to pill (the @ is percent-encoded, matching the message tool)
    assert "https://matrix.to/#/%40leader%3Aexample.test" in content["formatted_body"]
    assert "<a href=" in content["formatted_body"]
    # body keeps the readable fallback and names the file
    assert LEADER in content["body"]
    assert "已发布" in content["body"]
    assert "summary.md" in content["body"]
    # summary txn derives from the file txn (retry idempotency)
    assert summary_event["txn"] == file_event["txn"] + "-summary"
    # the file event itself keeps its contract (attachment metadata)
    assert file_event["content"]["body"] == "summary.md"
    assert file_event["content"]["url"].startswith("mxc://example.test/")
    assert file_event["content"]["info"]["size"] > 0


def test_invalid_mentions_are_dropped(matrix: _MatrixState, tmp_path: Path) -> None:
    published = _publish(tmp_path, mentions=["leader", "user:domain", " @user:example.test "])
    assert published["ok"] is True
    _, summary_event = _file_and_summary(matrix)
    content = summary_event["content"]
    # only the full MXID survives; short names and non-MXID text are dropped
    assert content.get("m.mentions", {}).get("user_ids") == ["@user:example.test"]
    assert "leader" not in content["body"].replace("@leader", "")
    assert "user:domain" not in content["body"]


def test_summary_without_resolvable_mention_is_plain_text(matrix: _MatrixState, tmp_path: Path) -> None:
    """③ no mention resolvable → the summary text is still sent, unmentioned."""
    published = _publish(tmp_path)
    assert published["ok"] is True
    file_event, summary_event = _file_and_summary(matrix)
    content = summary_event["content"]
    assert "m.mentions" not in content
    assert "已发布" in content["body"]
    assert "summary.md" in content["body"]
    assert "format" in content
    assert summary_event["txn"] == file_event["txn"] + "-summary"


def test_repeat_publish_deduplicates_summary_via_txn(matrix: _MatrixState, tmp_path: Path) -> None:
    """④ a retried publish of the same file re-PUTs the same txns: the
    homeserver deduplicates and no second summary event appears."""
    first = _publish(tmp_path, mentions=[LEADER])
    assert first["ok"] is True
    assert len(matrix.events) == 2

    second = _publish(tmp_path, mentions=[LEADER])
    assert second["ok"] is True
    # txn dedup: no new events, same media and event ids
    assert len(matrix.events) == 2, [event["txn"] for event in matrix.events]
    assert second["artifact"]["eventId"] == first["artifact"]["eventId"]
    assert second["artifact"]["mxcUri"] == first["artifact"]["mxcUri"]


def test_parent_event_sender_becomes_mention(matrix: _MatrixState, tmp_path: Path) -> None:
    matrix.room_events["$delegate-event"] = {"sender": LEADER}
    published = _publish(tmp_path, parentEventId="$delegate-event")
    assert published["ok"] is True
    file_event, summary_event = _file_and_summary(matrix)
    content = summary_event["content"]
    assert content.get("m.mentions", {}).get("user_ids") == [LEADER]
    assert file_event["content"]["m.relates_to"] == {
        "rel_type": "com.agentteams.attachment",
        "event_id": "$delegate-event",
    }


def test_parent_sender_lookup_failure_falls_open(matrix: _MatrixState, tmp_path: Path) -> None:
    """An unresolvable parent event must not block the publish: the
    summary goes out as plain text without a mention."""
    published = _publish(tmp_path, parentEventId="$unknown-event")
    assert published["ok"] is True
    file_event, summary_event = _file_and_summary(matrix)
    content = summary_event["content"]
    assert "m.mentions" not in content
    assert "已发布" in content["body"]


def test_task_leader_fallback_mention(matrix: _MatrixState, tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """No explicit mention, no parent event: the task's leader is mentioned."""
    task_dir = tmp_path / "shared" / "tasks" / "mention-task-01"
    task_dir.mkdir(parents=True, exist_ok=True)
    server._write_json(
        task_dir / "meta.json",
        {"task_id": "mention-task-01", "room_id": ROOM, "status": "in_progress"},
    )
    config = tmp_path / "runtime-config.json"
    config.write_text(
        json.dumps({"team": {"members": [{"role": "team-leader", "matrixUserId": LEADER}]}},
        ensure_ascii=False),
        encoding="utf-8",
    )
    monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config))

    published = _publish(tmp_path, taskId="mention-task-01")
    assert published["ok"] is True
    _, summary_event = _file_and_summary(matrix)
    content = summary_event["content"]
    assert content.get("m.mentions", {}).get("user_ids") == [LEADER]


def _write_task_workspace(workspace: Path, task_id: str) -> dict[str, Any]:
    task_dir = workspace / "shared" / "tasks" / task_id
    task_dir.mkdir(parents=True, exist_ok=True)
    (task_dir / "result.md").write_text("# Result\nDone.\n", encoding="utf-8")
    (task_dir / "notes.md").write_text("Extra notes.\n", encoding="utf-8")
    return {
        "task_id": task_id,
        "project_id": "mention-project",
        "room_id": ROOM,
        "status": "in_progress",
    }


def test_task_result_md_skips_summary_but_deliverables_notify(
    matrix: _MatrixState,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """submit_task's completion notification already names result.md and
    mentions the leader in the same action: result.md must not add a
    second @-summary, while extra deliverables keep the standard one."""
    task = _write_task_workspace(tmp_path, "mention-task-02")
    results = server._publish_task_artifacts(
        {"workspaceDir": str(tmp_path)},
        task,
        "mention-task-02",
        ["shared/tasks/mention-task-02/notes.md"],
        "",
    )
    assert [result["status"] for result in results] == ["published", "published"]

    assert len(matrix.events) == 3
    assert matrix.events[0]["content"]["msgtype"] == "m.file"  # result.md file
    assert matrix.events[1]["content"]["msgtype"] == "m.file"  # notes.md file
    assert matrix.events[2]["content"]["msgtype"] == "m.text"  # notes.md summary
    assert "notes.md" not in matrix.events[0]["content"]["body"]
    assert matrix.events[2]["txn"] == matrix.events[1]["txn"] + "-summary"


def test_project_result_md_summary_skipped_when_completion_event(
    matrix: _MatrixState,
    tmp_path: Path,
) -> None:
    project_dir = tmp_path / "shared" / "projects" / "mention-project"
    project_dir.mkdir(parents=True, exist_ok=True)
    (project_dir / "result.md").write_text("# Project result\n", encoding="utf-8")
    project = {
        "project_id": "mention-project",
        "reply_route": {"channel": "matrix", "target_session": ROOM},
    }

    # Completion notification already emitted for this action (event id
    # recorded) → the project result.md must not double-notify.
    results = server._publish_project_artifacts(
        {"workspaceDir": str(tmp_path)},
        project,
        "mention-project",
        "",
        "",
        completion_event_id="$project-completed",
    )
    assert results[0]["status"] == "published"
    assert len(matrix.events) == 1
    assert matrix.events[0]["content"]["msgtype"] == "m.file"

    # No completion event yet → the standard summary goes out.
    results = server._publish_project_artifacts(
        {"workspaceDir": str(tmp_path)},
        project,
        "mention-project",
        "",
        "",
    )
    assert results[0]["status"] == "published"
    file_event, summary_event = _file_and_summary(matrix)
    assert summary_event["txn"] == file_event["txn"] + "-summary"


def test_legacy_file_event_without_summary_unchanged(matrix: _MatrixState) -> None:
    """Requirement 4: callers that do not pass a summary keep the legacy
    single-event behavior with the random transaction id."""
    homeserver = os.environ["AGENTTEAMS_MATRIX_URL"]
    event_id = server._matrix_send_file_event(
        homeserver,
        "test-token",
        ROOM,
        "plain.txt",
        "mxc://example.test/legacy",
        10,
        "text/plain",
    )
    assert event_id == "$evt-1"
    assert len(matrix.events) == 1
    assert matrix.events[0]["content"]["msgtype"] == "m.file"
    assert re.fullmatch(r"teamharness-file-\d+-\d+-[0-9a-f]{32}", matrix.events[0]["txn"])


def test_summary_send_failure_does_not_fail_publish(
    matrix: _MatrixState,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """The file already reached the room; a broken summary must not turn
    the publish into a failure (best-effort notification)."""
    real_urlopen = server.urllib.request.urlopen

    def flaky_urlopen(request, timeout=None):
        url = request.full_url if hasattr(request, "full_url") else str(request)
        if "-summary" in url:
            raise urllib.error.URLError("summary endpoint down")
        return real_urlopen(request, timeout=timeout)

    monkeypatch.setattr(server.urllib.request, "urlopen", flaky_urlopen)
    published = _publish(tmp_path, mentions=[LEADER])
    assert published["ok"] is True
    assert published["artifact"]["status"] == "published"
    # only the file event made it to the room
    assert len(matrix.events) == 1
    assert matrix.events[0]["content"]["msgtype"] == "m.file"


def test_retry_after_fresh_mxc_deduplicates_via_preupload_txn(
    matrix: _MatrixState,
    tmp_path: Path,
) -> None:
    """Real homeservers may mint a different MXC URI for every upload of
    the same bytes. The publish txn is derived before the upload (room +
    filename + content hash), so the retry re-PUTs the same two txns and
    the room still gets exactly one file + one summary."""
    matrix.fresh_mxc_each_upload = True
    first = _publish(tmp_path, mentions=[LEADER])
    assert first["ok"] is True
    assert len(matrix.events) == 2

    second = _publish(tmp_path, mentions=[LEADER])
    assert second["ok"] is True
    # same content → same pre-upload txn, even though the MXC changed
    assert second["artifact"]["mxcUri"] != first["artifact"]["mxcUri"]
    assert second["artifact"]["eventId"] == first["artifact"]["eventId"]
    assert len(matrix.events) == 2, [event["txn"] for event in matrix.events]

    file_event, summary_event = _file_and_summary(matrix)
    assert re.fullmatch(r"teamharness-file-[0-9a-f]{24}", file_event["txn"])
    assert summary_event["txn"] == file_event["txn"] + "-summary"


def test_lost_send_response_retry_deduplicates_file_event(
    matrix: _MatrixState,
    tmp_path: Path,
) -> None:
    """The homeserver processed the file PUT but the client lost the
    response (the publish failed locally). The retry re-PUTs the same
    pre-upload txn: the file event deduplicates to the original event id
    and the summary is sent exactly once."""
    matrix.fresh_mxc_each_upload = True
    matrix.drop_next_put_response = True

    first = _publish(tmp_path, mentions=[LEADER])
    assert first["ok"] is False
    assert first["artifact"]["status"] == "failed"
    assert len(matrix.events) == 1
    assert matrix.events[0]["content"]["msgtype"] == "m.file"

    second = _publish(tmp_path, mentions=[LEADER])
    assert second["ok"] is True
    assert len(matrix.events) == 2, [event["txn"] for event in matrix.events]
    # the file event id is the one created by the lost first publish
    original_event_id = matrix.seen_txns[matrix.events[0]["txn"]]
    assert original_event_id == "$evt-1"
    assert second["artifact"]["eventId"] == original_event_id
    file_event, summary_event = _file_and_summary(matrix)
    assert summary_event["txn"] == file_event["txn"] + "-summary"


def test_publish_id_mints_new_txn_for_deliberate_republish(
    matrix: _MatrixState,
    tmp_path: Path,
) -> None:
    """Same content, deliberate second notification: a fresh publishId
    mints a new txn pair, so the room sees a second file + summary."""
    first = _publish(tmp_path, mentions=[LEADER])
    assert first["ok"] is True
    assert len(matrix.events) == 2

    second = _publish(tmp_path, mentions=[LEADER], publishId="attempt-2")
    assert second["ok"] is True
    assert len(matrix.events) == 4, [event["txn"] for event in matrix.events]

    first_file, _ = _file_and_summary(matrix, offset=2)
    second_file, _ = _file_and_summary(matrix)
    assert first_file["txn"] != second_file["txn"]
    assert re.fullmatch(r"teamharness-file-[0-9a-f]{24}", second_file["txn"])


def test_changed_content_mints_new_txn(matrix: _MatrixState, tmp_path: Path) -> None:
    """Changed content hashes to a new txn even without a publishId: the
    room is notified again."""
    first = _publish(tmp_path)
    assert first["ok"] is True
    assert len(matrix.events) == 2

    (tmp_path / "reports" / "summary.md").write_text("Revised result.\n" * 40, encoding="utf-8")
    second = _publish(tmp_path)
    assert second["ok"] is True
    assert len(matrix.events) == 4, [event["txn"] for event in matrix.events]
    first_file, _ = _file_and_summary(matrix, offset=2)
    second_file, _ = _file_and_summary(matrix)
    assert first_file["txn"] != second_file["txn"]
