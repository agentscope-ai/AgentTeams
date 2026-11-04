# -*- coding: utf-8 -*-
"""TH-10: creating a task room must invite the accountable users.

- the team leader and the human team members (admin included) join the
  createRoom invite after the admin, deduped, minus the creator;
- after the room exists (fresh or reused) the members are verified:
  joined or invited counts as present (idempotent for repeated
  create_task_room calls), missing users are re-invited, and the
  outcome is recorded as ``accountable: {invited, verified, reInvited}``;
- a failed verify is retryable: on the fresh path the room binding is
  written before the check, so the retry reuses the room instead of
  creating a second one.
"""

from __future__ import annotations

import json
import sys
import urllib.error
import urllib.parse
from pathlib import Path
from typing import Any

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


LEADER = "@leader:matrix.local"
HUMAN = "@human:matrix.local"
ADMIN = "@admin:matrix.local"
WORKER = "@worker-a:matrix.local"
CREATOR = "@creator:matrix.local"
ROOM = "!created:matrix.local"

CONFIG = {
    "team": {
        "name": "th10-team",
        "admin": {"name": "admin", "matrixUserId": ADMIN},
        "members": [
            {"name": "leader", "role": "team_leader", "matrixUserId": LEADER},
            {"name": "worker", "role": "worker", "matrixUserId": WORKER},
            {"name": "human", "role": "coordinator", "matrixUserId": HUMAN},
        ],
    },
}


class _Response:
    def __init__(self, payload: dict[str, Any]) -> None:
        self._payload = json.dumps(payload).encode("utf-8")

    def read(self) -> bytes:
        return self._payload

    def __enter__(self) -> "_Response":
        return self

    def __exit__(self, *exc: Any) -> bool:
        return False


class _MatrixFake:
    """Fake Matrix HTTP surface: createRoom, members, invite, room.meta."""

    def __init__(
        self,
        members: list[tuple[str, str]],
        invite_error: int | None = None,
    ) -> None:
        self.members = list(members)
        self.invite_error = invite_error
        self.requests: list[tuple[str, str, dict[str, Any]]] = []

    def urlopen(self, request: Any, timeout: Any = None) -> _Response:
        method = request.get_method()
        path = urllib.parse.urlparse(request.full_url).path
        raw = request.data.decode("utf-8") if request.data else ""
        body = json.loads(raw or "{}")
        self.requests.append((method, path, body))
        if method == "POST" and path.endswith("/createRoom"):
            return _Response({"room_id": ROOM})
        if method == "GET" and path.endswith("/members"):
            chunk = [
                {"state_key": user_id, "content": {"membership": membership}}
                for user_id, membership in self.members
            ]
            return _Response({"chunk": chunk})
        if method == "POST" and path.endswith("/invite"):
            if self.invite_error:
                raise urllib.error.HTTPError(path, self.invite_error, "forbidden", {}, None)
            return _Response({})
        if method == "PUT" and path.endswith("/state/room.meta/"):
            return _Response({"event_id": "$meta"})
        raise urllib.error.HTTPError(path, 404, "not found", {}, None)

    def invite_posts(self) -> list[dict[str, Any]]:
        return [
            body for method, path, body in self.requests if path.endswith("/invite")
        ]

    def create_bodies(self) -> list[dict[str, Any]]:
        return [
            body for method, path, body in self.requests if path.endswith("/createRoom")
        ]


def _prepare(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    members: list[tuple[str, str]],
    invite_error: int | None = None,
    with_team: bool = True,
    creator: str = CREATOR,
) -> _MatrixFake:
    fake = _MatrixFake(members, invite_error)
    if with_team:
        config_path = tmp_path / "runtime.json"
        config_path.write_text(json.dumps(CONFIG), encoding="utf-8")
        monkeypatch.setenv("TEAMHARNESS_RUNTIME_CONFIG", str(config_path))
    else:
        monkeypatch.delenv("TEAMHARNESS_RUNTIME_CONFIG", raising=False)
    for key in ("TEAMHARNESS_SHARED_DIR", "AGENTTEAMS_SHARED_DIR", "QWENPAW_WORKING_DIR", "COPAW_WORKING_DIR"):
        monkeypatch.delenv(key, raising=False)
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix.local")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    monkeypatch.setenv("AGENTTEAMS_MATRIX_USER_ID", creator)
    monkeypatch.setattr(server.urllib.request, "urlopen", fake.urlopen)
    return fake


def _create(
    workspace: Path | None,
    task_id: str = "th10-task",
    invite: list[str] | None = None,
) -> dict[str, Any]:
    arguments: dict[str, Any] = {
        "action": "create_task_room",
        "payload": {
            "taskId": task_id,
            "name": "th10",
            "source": "matrix",
            "invite": invite if invite is not None else [WORKER],
        },
    }
    if workspace is not None:
        arguments["workspaceDir"] = str(workspace)
    return server._roomflow(arguments)


def test_create_room_invites_accountable_and_records(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = _prepare(
        tmp_path,
        monkeypatch,
        [(WORKER, "join"), (ADMIN, "join"), (LEADER, "invite"), (HUMAN, "invite")],
    )

    created = _create(None)

    assert created["ok"] is True
    # invite = payload + admin + accountable (leader first, deduped).
    assert fake.create_bodies()[0]["invite"] == [WORKER, ADMIN, LEADER, HUMAN]
    assert created["accountable"] == {
        "invited": [LEADER, ADMIN, HUMAN],
        "verified": True,
        "reInvited": [],
    }
    assert fake.invite_posts() == []


def test_missing_accountable_get_reinvited_and_recorded(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = _prepare(tmp_path, monkeypatch, [(WORKER, "join")])

    created = _create(None)

    assert created["ok"] is True
    assert created["accountable"] == {
        "invited": [LEADER, ADMIN, HUMAN],
        "verified": True,
        "reInvited": [LEADER, ADMIN, HUMAN],
    }
    assert [post["user_id"] for post in fake.invite_posts()] == [LEADER, ADMIN, HUMAN]


def test_reuse_is_idempotent_without_duplicate_invite(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = _prepare(
        tmp_path,
        monkeypatch,
        [(WORKER, "join"), (ADMIN, "invite"), (LEADER, "invite"), (HUMAN, "invite")],
    )

    first = _create(tmp_path)
    assert first["ok"] is True
    assert first["accountable"]["reInvited"] == []

    second = _create(tmp_path)

    assert second["ok"] is True
    assert second["reused"] is True
    assert second["roomId"] == ROOM
    # One room; an existing invite already counts, so no invite POST.
    assert len(fake.create_bodies()) == 1
    assert fake.invite_posts() == []


def test_creator_is_excluded_from_accountable(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = _prepare(
        tmp_path,
        monkeypatch,
        [(WORKER, "join"), (ADMIN, "join"), (HUMAN, "invite")],
        creator=LEADER,
    )

    created = _create(None)

    assert created["ok"] is True
    body = fake.create_bodies()[0]
    assert LEADER not in body["invite"]
    assert body["invite"] == [WORKER, ADMIN, HUMAN]
    assert created["accountable"]["invited"] == [ADMIN, HUMAN]
    assert created["accountable"]["reInvited"] == []


def test_standalone_run_has_no_accountable(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = _prepare(tmp_path, monkeypatch, [(WORKER, "join")], with_team=False)

    created = _create(None)

    assert created["ok"] is True
    assert fake.create_bodies()[0]["invite"] == [WORKER]
    assert created["accountable"] == {"invited": [], "verified": True, "reInvited": []}
    assert fake.invite_posts() == []


def test_verify_failure_is_retryable_via_reuse(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake = _prepare(
        tmp_path,
        monkeypatch,
        [(WORKER, "join"), (ADMIN, "join")],
        invite_error=403,
    )

    first = _create(tmp_path)

    assert first["ok"] is False
    assert "Matrix API error: HTTP 403" in first["error"]
    assert len(fake.create_bodies()) == 1

    # Retry: the room binding was written before the failed check, so the
    # reuse path runs; everyone is present now -> ok, no second room.
    fake.members = [
        (WORKER, "join"),
        (ADMIN, "invite"),
        (LEADER, "invite"),
        (HUMAN, "invite"),
    ]
    fake.invite_error = None
    second = _create(tmp_path)

    assert second["ok"] is True
    assert second["reused"] is True
    assert len(fake.create_bodies()) == 1
    assert second["accountable"]["reInvited"] == []
