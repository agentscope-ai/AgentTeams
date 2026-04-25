import json
import sys
import types

import pytest

from copaw_worker.hooks.tools.message import (
    build_matrix_text_content,
    extract_matrix_mentions,
    message,
    parse_matrix_target,
    validate_matrix_message_policy,
)


def _response_json(response):
    return json.loads(response.content[0].text)


def test_parse_matrix_room_targets():
    for raw in (
        "room:!abc:matrix.local",
        "matrix:room:!abc:matrix.local",
        "!abc:matrix.local",
    ):
        target = parse_matrix_target(raw)
        assert target.kind == "room"
        assert target.identifier == "!abc:matrix.local"


def test_parse_matrix_user_targets():
    for raw in (
        "user:@alice:matrix.local",
        "matrix:user:@alice:matrix.local",
        "@alice:matrix.local",
    ):
        target = parse_matrix_target(raw)
        assert target.kind == "user"
        assert target.identifier == "@alice:matrix.local"


def test_parse_matrix_target_rejects_invalid_value():
    with pytest.raises(ValueError, match="target must be"):
        parse_matrix_target("alice")


def test_build_matrix_text_content_adds_mentions():
    content = build_matrix_text_content(
        "hello",
        ["@alice:matrix.local"],
    )

    assert content["body"].startswith("@alice:matrix.local ")
    assert content["m.mentions"] == {"user_ids": ["@alice:matrix.local"]}
    assert "https://matrix.to/#/%40alice%3Amatrix.local" in content["formatted_body"]


def test_build_matrix_text_content_rewrites_existing_mentions_once():
    content = build_matrix_text_content(
        "@alice:matrix.local please handle",
        extract_matrix_mentions("@alice:matrix.local please handle"),
    )

    assert content["body"].count("@alice:matrix.local") == 1
    assert content["m.mentions"] == {"user_ids": ["@alice:matrix.local"]}
    assert "https://matrix.to/#/%40alice%3Amatrix.local" in content["formatted_body"]


def test_build_matrix_text_content_renders_minimal_markdown():
    content = build_matrix_text_content(
        "**Issue**: run `copaw-sync`\n```\ntasks/st-01/spec.md\n```",
        [],
    )

    formatted_body = content["formatted_body"]
    assert "<strong>Issue</strong>" in formatted_body
    assert "<code>copaw-sync</code>" in formatted_body
    assert "<pre><code>tasks/st-01/spec.md</code></pre>" in formatted_body
    assert "**Issue**" not in formatted_body
    assert "```" not in formatted_body


def test_validate_matrix_message_policy_blocks_status_ping():
    with pytest.raises(ValueError, match="status symbol"):
        validate_matrix_message_policy(
            "@alice:matrix.local 🟢",
            ["@alice:matrix.local"],
        )


def test_validate_matrix_message_policy_allows_substantive_ping():
    validate_matrix_message_policy(
        "@alice:matrix.local Please investigate file-sync for task st-01.",
        ["@alice:matrix.local"],
    )


@pytest.mark.asyncio
async def test_message_tool_dry_run_returns_content():
    response = await message(
        action="send",
        channel="matrix",
        target="room:!abc:matrix.local",
        message="@alice:matrix.local hello",
        dryRun=True,
    )
    payload = _response_json(response)

    assert payload["ok"] is True
    assert payload["dryRun"] is True
    assert payload["roomId"] == "!abc:matrix.local"
    assert payload["mentions"] == ["@alice:matrix.local"]
    assert payload["content"]["m.mentions"] == {
        "user_ids": ["@alice:matrix.local"],
    }


@pytest.mark.asyncio
async def test_message_tool_blocks_status_ping():
    response = await message(
        action="send",
        channel="matrix",
        target="room:!abc:matrix.local",
        message="@alice:matrix.local 🟢",
        dryRun=True,
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "ping-pong loops" in payload["error"]


@pytest.mark.asyncio
async def test_message_tool_rejects_user_target_for_now():
    response = await message(
        action="send",
        channel="matrix",
        target="user:@alice:matrix.local",
        message="@alice:matrix.local hello",
        dryRun=True,
    )
    payload = _response_json(response)

    assert payload["ok"] is False
    assert "user targets are not supported yet" in payload["error"]


def test_install_tool_hooks_registers_message(monkeypatch):
    import copaw_worker.hooks as hooks

    hooks._TOOL_HOOK_INSTALLED = False
    hooks._PINGPONG_HOOK_INSTALLED = False

    class FakeToolkit:
        def __init__(self):
            self.registered = []

        def register_tool_function(self, func, namesake_strategy="skip"):
            self.registered.append((func.__name__, namesake_strategy))

    class FakeAgent:
        def _create_toolkit(self):
            return FakeToolkit()

    class FakeRunner:
        async def query_handler(self, msgs, request=None, **kwargs):
            if False:
                yield msgs, request, kwargs

    fake_agent_module = types.ModuleType("copaw.agents.react_agent")
    fake_agent_module.CoPawAgent = FakeAgent
    fake_runner_module = types.ModuleType("copaw.app.runner.runner")
    fake_runner_module.AgentRunner = FakeRunner

    monkeypatch.setitem(sys.modules, "copaw", types.ModuleType("copaw"))
    monkeypatch.setitem(sys.modules, "copaw.agents", types.ModuleType("copaw.agents"))
    monkeypatch.setitem(sys.modules, "copaw.agents.react_agent", fake_agent_module)
    monkeypatch.setitem(sys.modules, "copaw.app", types.ModuleType("copaw.app"))
    monkeypatch.setitem(sys.modules, "copaw.app.runner", types.ModuleType("copaw.app.runner"))
    monkeypatch.setitem(sys.modules, "copaw.app.runner.runner", fake_runner_module)

    hooks.install_tool_hooks()
    toolkit = FakeAgent()._create_toolkit()

    assert ("message", "override") in toolkit.registered
