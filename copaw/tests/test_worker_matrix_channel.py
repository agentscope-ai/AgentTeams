"""Tests for the AgentTeams CoPaw worker Matrix channel."""

import asyncio
from types import SimpleNamespace

from copaw_worker.matrix_channel import MatrixChannel


class _FakeClient:
    def __init__(self):
        self.rooms = {}
        self.sent = []

    async def room_send(self, room_id, message_type, content, **kwargs):
        self.sent.append((room_id, message_type, content, kwargs))
        return SimpleNamespace(event_id=f"$sent{len(self.sent)}")


async def _noop_typing(_room_id, _typing):
    return None


def _make_channel(user_id: str = "@dag-team-1-lead:hs.local") -> MatrixChannel:
    ch = MatrixChannel.__new__(MatrixChannel)
    ch._user_id = user_id
    ch._client = _FakeClient()
    ch._send_typing = _noop_typing
    return ch


def _write_team_leader_runtime(tmp_path):
    working_dir = tmp_path / "leader" / ".copaw"
    runtime_dir = tmp_path / "leader" / "runtime"
    runtime_dir.mkdir(parents=True)
    (runtime_dir / "runtime.yaml").write_text(
        "kind: MemberRuntimeConfig\n"
        "member:\n"
        "  role: team_leader\n"
        "team:\n"
        "  name: dag-team-1\n"
        "  teamRoomId: \"!team-room:hs.local\"\n"
        "  leaderDmRoomId: \"!leader-dm:hs.local\"\n",
        encoding="utf-8",
    )
    return working_dir


def test_worker_channel_suppresses_team_leader_dm_internal_preamble(
    tmp_path, monkeypatch,
):
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(_write_team_leader_runtime(tmp_path)),
    )
    ch = _make_channel()

    for text in (
        "I'll coordinate the team. Let me first check the team organization.",
        "Good, I have a thorough understanding of all the skills. "
        "Now let me check the team organization and available workers.",
        "I have 2 workers available: a dev worker and a QA worker. "
        "Now let me design the DAG plan and create the project.",
    ):
        asyncio.run(ch.send("!leader-dm:hs.local", text))

    assert ch._client.sent == []


def test_worker_channel_suppression_reads_default_workspace_runtime(
    tmp_path, monkeypatch,
):
    copaw_root = _write_team_leader_runtime(tmp_path)
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(copaw_root / "workspaces" / "default"),
    )
    ch = _make_channel()

    asyncio.run(
        ch.send(
            "!leader-dm:hs.local",
            "I'll coordinate the team. Let me first check the team organization.",
        ),
    )

    assert ch._client.sent == []


def test_worker_channel_keeps_team_assignment_reroute(tmp_path, monkeypatch):
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(_write_team_leader_runtime(tmp_path)),
    )
    ch = _make_channel()

    asyncio.run(
        ch.send(
            "!leader-dm:hs.local",
            "@dag-team-1-dev:hs.local Task assigned: implement the API.",
        ),
    )

    assert ch._client.sent[0][0] == "!team-room:hs.local"
    assert ch._client.sent[0][2]["m.mentions"] == {
        "user_ids": ["@dag-team-1-dev:hs.local"],
    }


def test_worker_channel_suppresses_no_reply(tmp_path, monkeypatch):
    monkeypatch.setenv(
        "COPAW_WORKING_DIR",
        str(_write_team_leader_runtime(tmp_path)),
    )
    ch = _make_channel()

    asyncio.run(ch.send("!leader-dm:hs.local", "NO_REPLY"))

    assert ch._client.sent == []
