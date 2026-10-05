from unittest import mock

from cli_harness_worker import bridge
from cli_harness_worker.adapters import get_adapter


def _cfg():
    return {
        "models": {
            "providers": {
                "higress": {
                    "baseUrl": "http://172.17.0.2:8080/v1",
                    "apiKey": "sk-gw",
                    "models": [{"id": "qwen3.6-plus", "contextWindow": 131072}],
                }
            }
        },
        "agents": {"defaults": {"model": {"primary": "higress/qwen3.6-plus"}}},
        "channels": {
            "matrix": {
                "homeserver": "http://matrix:8008",
                "userId": "@alice:example.org",
                "accessToken": "tok",
            }
        },
    }


def test_resolve_active_model_primary_wins():
    model = bridge.resolve_active_model(_cfg())
    assert model["id"] == "qwen3.6-plus"
    assert model["_provider_id"] == "higress"
    assert model["_provider"]["apiKey"] == "sk-gw"


def test_resolve_active_model_first_provider_fallback():
    cfg = _cfg()
    del cfg["agents"]["defaults"]["model"]["primary"]
    model = bridge.resolve_active_model(cfg)
    assert model["id"] == "qwen3.6-plus"


def test_resolve_active_model_empty():
    assert bridge.resolve_active_model({}) is None


def test_resolve_matrix_config():
    matrix = bridge.resolve_matrix_config(_cfg())
    assert matrix["userId"] == "@alice:example.org"
    assert bridge.resolve_matrix_config({}) == {}


def test_adapter_env_claude():
    adapter = get_adapter("claude-code")
    with mock.patch.object(bridge, "_is_in_container", return_value=True):
        env = bridge.adapter_env(_cfg(), adapter)
    assert env["ANTHROPIC_BASE_URL"] == "http://172.17.0.2:8080/v1"
    assert env["ANTHROPIC_AUTH_TOKEN"] == "sk-gw"
    assert env["AGENTTEAMS_CLI_MODEL"] == "qwen3.6-plus"


def test_port_remap_only_on_host():
    assert bridge._port_remap("http://h:8080/v1", True) == "http://h:8080/v1"
    assert bridge._port_remap("http://h:8080/v1", False) == "http://h:18080/v1"
    assert bridge._port_remap("http://h:9000/v1", False) == "http://h:9000/v1"


def test_build_prompt_includes_soul_and_task():
    prompt = bridge.build_prompt("do X", soul="You are Alice.")
    assert "You are Alice." in prompt
    assert prompt.strip().endswith("do X")


def test_build_prompt_task_only():
    assert bridge.build_prompt("do X") == "do X"
