"""Tests for HICLAW_QUIET_ROOMS -> MATRIX_FILTER_TOOL_MESSAGES/THINKING env
derivation in hermes_worker.bridge._matrix_env.

Phase 5b "quiet rooms" (mechanism only, env-gated default-off): the bridge
used to hardcode these two MATRIX_FILTER_* values to "true" (dead knobs —
nothing read them). They are now derived from HICLAW_QUIET_ROOMS (default
false) so that flipping the gate is what makes
hermes_matrix.policies.should_suppress_outbound() actually suppress
tool/thinking chatter in the overlay adapter's send_message_event wrapper.

This intentionally lives outside test_bridge.py to keep this step's diff
scoped to a dedicated file, mirroring the copaw-side precedent
(test_heartbeat_config.py / test_quiet_rooms.py).
"""
from __future__ import annotations

from hermes_worker.bridge import _matrix_env


def _make_cfg():
    return {
        "channels": {
            "matrix": {
                "enabled": True,
                "homeserver": "http://hiclaw-controller:6167",
                "accessToken": "tok",
                "userId": "@alice:matrix.local",
            }
        },
    }


def test_quiet_rooms_unset_defaults_filters_false(monkeypatch):
    monkeypatch.delenv("HICLAW_QUIET_ROOMS", raising=False)

    env = _matrix_env(_make_cfg())

    assert env["MATRIX_FILTER_TOOL_MESSAGES"] == "false"
    assert env["MATRIX_FILTER_THINKING"] == "false"


def test_quiet_rooms_false_keeps_filters_false(monkeypatch):
    monkeypatch.setenv("HICLAW_QUIET_ROOMS", "false")

    env = _matrix_env(_make_cfg())

    assert env["MATRIX_FILTER_TOOL_MESSAGES"] == "false"
    assert env["MATRIX_FILTER_THINKING"] == "false"


def test_quiet_rooms_truthy_derives_filters_true(monkeypatch):
    monkeypatch.setenv("HICLAW_QUIET_ROOMS", "true")

    env = _matrix_env(_make_cfg())

    assert env["MATRIX_FILTER_TOOL_MESSAGES"] == "true"
    assert env["MATRIX_FILTER_THINKING"] == "true"


def test_quiet_rooms_accepts_common_truthy_spellings(monkeypatch):
    for value in ("1", "yes", "on", "TRUE"):
        monkeypatch.setenv("HICLAW_QUIET_ROOMS", value)
        env = _matrix_env(_make_cfg())
        assert env["MATRIX_FILTER_TOOL_MESSAGES"] == "true", f"failed for {value!r}"
        assert env["MATRIX_FILTER_THINKING"] == "true", f"failed for {value!r}"
