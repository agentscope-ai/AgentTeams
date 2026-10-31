# -*- coding: utf-8 -*-
"""QwenCode live-status indicator derivation."""

from qwen_code_bridge import qwen_status


def test_idle():
    payload = {"sessions": [{"sessionId": "s", "activeWorkState": "idle", "hasActivePrompt": False}]}
    assert qwen_status.derive_indicator(payload) == "idle"
    assert qwen_status.is_busy(payload) is False


def test_busy_prompt():
    payload = {"sessions": [{"sessionId": "s", "hasActivePrompt": True, "activeWorkState": "idle"}]}
    assert qwen_status.derive_indicator(payload) == "busy"
    assert qwen_status.is_busy(payload) is True


def test_busy_background():
    payload = {"sessions": [{"sessionId": "s", "hasRunningBackgroundTasks": True}]}
    assert qwen_status.derive_indicator(payload) == "busy"


def test_waiting_permission_wins():
    payload = {"sessions": [{"isWaitingForPermission": True}, {"hasActivePrompt": True}]}
    assert qwen_status.derive_indicator(payload) == "waiting_permission"


def test_waiting_question():
    payload = {"sessions": [{"isWaitingForUserQuestion": True}]}
    assert qwen_status.derive_indicator(payload) == "waiting_question"


def test_offline():
    assert qwen_status.derive_indicator(None, reachable=False) == "offline"
    assert qwen_status.is_busy(None, reachable=False) is False
