"""An invited Agent joining asynchronously is readiness, not a model decision."""
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "teamharness/mcp"))
import server  # Bootstrap the portable mcp package.
from mcp.room import notify


@pytest.fixture
def membership(monkeypatch):
    monkeypatch.setenv("AGENTTEAMS_MATRIX_URL", "http://matrix")
    monkeypatch.setenv("AGENTTEAMS_WORKER_MATRIX_TOKEN", "test-token")
    elapsed = [0.0]
    monkeypatch.setattr(notify.time, "monotonic", lambda: elapsed[0])
    monkeypatch.setattr(notify.time, "sleep", lambda seconds: elapsed.__setitem__(0, elapsed[0] + seconds))
    return elapsed


def test_invited_member_can_join_before_delegation_without_model_retry(membership, monkeypatch):
    calls = []
    def members(room, states):
        calls.append(states)
        return ["@worker:test"] if states == {"join", "invite"} or membership[0] >= 0.4 else []
    monkeypatch.setattr(notify, "_matrix_room_member_user_ids", members)
    assert notify._validate_assignee_membership("!task:test", "@worker:test")["ok"]
    assert 0.4 <= membership[0] <= 5
    assert calls[-1] == {"join"}


@pytest.mark.parametrize("invited", [False, True])
def test_missing_or_unaccepted_invitation_never_allows_assignment(membership, monkeypatch, invited):
    monkeypatch.setattr(notify, "_matrix_room_member_user_ids", lambda room, states:
        ["@worker:test"] if invited and states == {"join", "invite"} else [])
    result = notify._validate_assignee_membership("!task:test", "@worker:test")
    assert not result["ok"] and "not a joined member" in result["error"]
    assert membership[0] <= 5.01
    if not invited:
        assert membership[0] == 0


def test_membership_read_failure_does_not_guess_joined_state(membership, monkeypatch):
    def members(room, states):
        if states == {"join", "invite"}:
            return ["@worker:test"]
        if membership[0]:
            raise OSError("Matrix unavailable")
        return []
    monkeypatch.setattr(notify, "_matrix_room_member_user_ids", members)
    result = notify._validate_assignee_membership("!task:test", "@worker:test")
    assert not result["ok"] and "cannot verify room membership" in result["error"]
