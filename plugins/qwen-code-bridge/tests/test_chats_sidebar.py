# -*- coding: utf-8 -*-
"""Sidebar (chat-list) integration tests: groups / chats / rename payloads."""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import pytest

from qwen_code_bridge import chats


@pytest.fixture
def calls(monkeypatch):
    recorded: list[tuple[str, str, dict | None]] = []

    def fake_request(method, url, body, agent_id, timeout=5.0):  # noqa: ARG001
        recorded.append((method, url, body))
        if url.endswith("/api/chats") and method == "GET":
            return list(fake_request.chats)  # type: ignore[attr-defined]
        if url.endswith("/api/chats/groups") and method == "GET":
            return list(fake_request.groups)  # type: ignore[attr-defined]
        if url.endswith("/api/chats/groups") and method == "POST":
            return {"id": "g-new", "name": body["name"], "pinned": False}
        if url.endswith("/api/chats") and method == "POST":
            created = {"id": "chat-new", **(body or {})}
            fake_request.chats.append(created)  # type: ignore[attr-defined]
            return created
        if "/api/chats/" in url and method == "PUT":
            return {"id": url.rsplit("/", 1)[-1]}
        return None

    fake_request.groups = []  # type: ignore[attr-defined]
    fake_request.chats = []  # type: ignore[attr-defined]
    monkeypatch.setattr(chats, "_request", fake_request)
    return recorded, fake_request


def test_ensure_group_creates_and_pins(calls) -> None:
    recorded, _ = calls
    gid = chats.ensure_group("http://app", "agent-x")
    assert gid == "g-new"
    methods = [(m, u.rsplit("/", 1)[-1]) for m, u, _ in recorded]
    assert ("POST", "groups") in methods
    assert ("PUT", "g-new") in methods
    pin_body = [b for m, u, b in recorded if m == "PUT" and u.endswith("/g-new")]
    assert pin_body == [{"pinned": True}]


def test_ensure_group_reuses_existing_and_pins_if_needed(calls) -> None:
    recorded, fake = calls
    fake.groups = [{"id": "g-1", "name": "QwenCode", "pinned": False}]
    gid = chats.ensure_group("http://app", "agent-x")
    assert gid == "g-1"
    assert not any(m == "POST" for m, _, _ in recorded)
    assert [b for m, _, b in recorded if m == "PUT"] == [{"pinned": True}]

    recorded.clear()
    fake.groups = [{"id": "g-2", "name": "QwenCode", "pinned": True}]
    assert chats.ensure_group("http://app", "agent-x") == "g-2"
    assert recorded and all(m == "GET" for m, _, _ in recorded)


def test_create_run_chat_payload(calls) -> None:
    recorded, _ = calls
    chat_id = chats.create_run_chat(
        "http://app",
        "agent-x",
        name="t1 · qwen · running",
        session_id="qwen-code-bridge:abc",
        user_id="default",
        group_id="g-new",
        meta={"task_id": "abc"},
    )
    assert chat_id == "chat-new"
    method, url, body = recorded[-1]
    assert method == "POST" and url.endswith("/api/chats")
    assert body == {
        "name": "t1 · qwen · running",
        "session_id": "qwen-code-bridge:abc",
        "user_id": "default",
        "channel": "qwen-code-bridge",
        "source": "chat",
        "group_id": "g-new",
        "meta": {"task_id": "abc"},
    }


def test_rename_chat(calls) -> None:
    recorded, _ = calls
    assert chats.rename_chat("http://app", "agent-x", "chat-1", "t1 · qwen · done")
    method, url, body = recorded[-1]
    assert method == "PUT" and url.endswith("/api/chats/chat-1")
    assert body == {"name": "t1 · qwen · done"}
    assert chats.rename_chat("http://app", "agent-x", "", "x") is False


def test_run_chat_name() -> None:
    assert chats.run_chat_name("t1", "qwen", "running") == "t1 · qwen · running"


def test_ensure_direct_chat_reuses_or_creates(calls) -> None:
    recorded, fake = calls
    cid = chats.ensure_direct_chat("http://app", "agent-x", "g-1")
    assert cid == "chat-new"
    body = [b for m, u, b in recorded if m == "POST" and u.endswith("/api/chats")][-1]
    assert body["session_id"] == chats.DIRECT_SESSION_ID
    assert body["name"] == chats.DIRECT_CHAT_NAME
    # second call finds the existing chat (no extra POST)
    recorded.clear()
    assert chats.ensure_direct_chat("http://app", "agent-x", "g-1") == "chat-new"
    assert not any(m == "POST" for m, _, _ in recorded)
