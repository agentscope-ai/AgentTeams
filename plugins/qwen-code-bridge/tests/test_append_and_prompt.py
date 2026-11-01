# -*- coding: utf-8 -*-
"""SSE run stream + chat mirror + bridge prompt section."""

# pylint: disable=redefined-outer-name
from __future__ import annotations

import json

import pytest
from qwen_code_bridge import prompt
from qwen_code_bridge.slash import append_to_session


@pytest.mark.asyncio
async def test_append_to_session_roles() -> None:
    saved = {}

    class FakeSession:
        async def get_session_state_dict(self, session_id, user_id="", channel=""):
            return {}

        async def update_session_state(self, session_id, key, value, user_id="", channel=""):
            saved.update(
                {
                    "session_id": session_id,
                    "key": key,
                    "value": value,
                    "user_id": user_id,
                    "channel": channel,
                }
            )

    ok = await append_to_session(
        FakeSession(),
        "qwen-code-bridge:" + "c" * 32,
        "default",
        "qwen-code-bridge",
        role="assistant",
        name="qwen-code",
        text="[running] x",
    )
    assert ok is True
    ctx = saved["value"]["context"]
    assert ctx[-1]["role"] == "assistant"
    assert ctx[-1]["content"][0]["text"] == "[running] x"


def test_bridge_prompt_content() -> None:
    text = prompt.bridge_prompt()
    assert "qwen-code-bridge" in text
    assert "/qcode" in text
    assert "STRICT→plan" in text
