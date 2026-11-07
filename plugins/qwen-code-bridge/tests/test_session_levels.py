# -*- coding: utf-8 -*-
"""Per-session approval-level memory (client-only switch bridge)."""

import pytest

from qwen_code_bridge import session_levels


@pytest.fixture(autouse=True)
def _isolate(monkeypatch, tmp_path):
    monkeypatch.setattr(
        session_levels, "_path", lambda: tmp_path / "session-levels.json"
    )
    session_levels.reset_cache()
    yield
    session_levels.reset_cache()


def test_record_and_read_back() -> None:
    session_levels.record("s1", "OFF")
    assert session_levels.last_level("s1") == "OFF"


def test_record_normalises_and_ignores_invalid() -> None:
    session_levels.record("s2", "auto")
    assert session_levels.last_level("s2") == "AUTO"
    session_levels.record("s2", "bogus")
    assert session_levels.last_level("s2") == "AUTO"
    session_levels.record("", "OFF")
    assert session_levels.last_level("") == ""


def test_unknown_session_is_empty() -> None:
    assert session_levels.last_level("nope") == ""


def test_record_is_idempotent() -> None:
    session_levels.record("s3", "SMART")
    session_levels.record("s3", "SMART")
    assert session_levels.last_level("s3") == "SMART"
