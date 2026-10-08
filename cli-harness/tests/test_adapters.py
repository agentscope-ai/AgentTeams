import os
from unittest import mock

import pytest

from cli_harness_worker.adapters import (
    DEFAULT_RUNTIME,
    get_adapter,
    normalize_runtime,
)
from cli_harness_worker.adapters.base import AdapterError


ALL_RUNTIMES = [
    "atomcode",
    "codex",
    "claude-code",
    "kimi-code",
    "pi",
    "dsh",
]


def test_normalize_runtime_accepts_all_names():
    for name in ALL_RUNTIMES:
        assert normalize_runtime(name) == name


def test_normalize_runtime_aliases():
    assert normalize_runtime("claude") == "claude-code"
    assert normalize_runtime("claudecode") == "claude-code"
    assert normalize_runtime("kimi") == "kimi-code"


def test_normalize_runtime_deepseek_harness_not_mapped():
    # "deepseek-harness" is a dedicated upstream runtime on its own image;
    # it must keep routing there instead of being treated as the dsh adapter.
    assert normalize_runtime("deepseek-harness") == DEFAULT_RUNTIME


def test_normalize_runtime_unknown_falls_back():
    assert normalize_runtime("bogus") == DEFAULT_RUNTIME
    assert normalize_runtime("") == DEFAULT_RUNTIME


def test_get_adapter_returns_matching_binary():
    assert get_adapter("claude-code").binary == "claude"
    assert get_adapter("codex").binary == "codex"
    assert get_adapter("atomcode").binary == "atomcode"
    assert get_adapter("kimi-code").binary == "kimi"
    assert get_adapter("pi").binary == "pi"
    assert get_adapter("dsh").binary == "dsh"


def test_claude_command_includes_prompt_and_flags():
    adapter = get_adapter("claude-code")
    with mock.patch.dict(os.environ, {"AGENTTEAMS_CLI_MODEL": "qwen3.6-plus"}, clear=False):
        command = adapter.build_command("do the thing")
    assert command[0] == "claude"
    assert "-p" in command
    assert "do the thing" in command
    assert "--model" in command
    assert "qwen3.6-plus" in command


def test_codex_command_uses_exec_subcommand():
    adapter = get_adapter("codex")
    command = adapter.build_command("fix the bug")
    assert command[:2] == ["codex", "exec"]
    assert command[-1] == "fix the bug"


def test_dsh_command_uses_headless_run():
    adapter = get_adapter("dsh")
    command = adapter.build_command("write tests")
    assert command[:3] == ["dsh", "run", "--headless"]
    assert "write tests" in command


def test_extra_args_appended():
    adapter = get_adapter("pi")
    with mock.patch.dict(
        os.environ, {"AGENTTEAMS_CLI_EXTRA_ARGS": "--verbose --trace"}, clear=False
    ):
        command = adapter.build_command("hello")
    assert command[-2:] == ["--verbose", "--trace"]


def test_required_env_claude():
    adapter = get_adapter("claude-code")
    env = adapter.required_env("m1", "http://gw:8080", "sk-test")
    assert env["ANTHROPIC_BASE_URL"] == "http://gw:8080"
    assert env["ANTHROPIC_AUTH_TOKEN"] == "sk-test"


def test_required_env_codex():
    adapter = get_adapter("codex")
    env = adapter.required_env("m1", "http://gw:8080", "sk-test")
    assert env["OPENAI_BASE_URL"] == "http://gw:8080"
    assert env["OPENAI_API_KEY"] == "sk-test"


def test_run_raises_when_binary_missing():
    adapter = get_adapter("claude-code")
    adapter.binary = "definitely-not-a-real-binary-xyz"
    with pytest.raises(AdapterError):
        import asyncio
        asyncio.run(adapter.run("prompt", timeout=5))
