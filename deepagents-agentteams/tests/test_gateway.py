import unittest

from deepagents_agentteams.config import ConfigError, ModelConfig
from deepagents_agentteams.gateway import build_higress_model, build_mcp_connections


class RecordingChatModel:
    def __init__(self, **kwargs: object) -> None:
        self.kwargs = kwargs


class HigressModelTests(unittest.TestCase):
    def test_normalizes_legacy_gateway_root_to_openai_v1_base(self) -> None:
        config = ModelConfig(
            name="qwen-max",
            gateway_url="https://higress.example.org",
            gateway_key="gateway-key",
        )

        model = build_higress_model(config, model_factory=RecordingChatModel)

        self.assertEqual(model.kwargs["base_url"], "https://higress.example.org/v1")

    def test_uses_openai_compatible_chat_completions_without_responses_api(self) -> None:
        config = ModelConfig(
            name="qwen-max",
            gateway_url="https://higress.example.org/v1",
            gateway_key="gateway-secret",
        )

        model = build_higress_model(config, model_factory=RecordingChatModel)

        self.assertEqual(model.kwargs["model"], "qwen-max")
        self.assertEqual(model.kwargs["base_url"], "https://higress.example.org/v1")
        self.assertEqual(model.kwargs["api_key"], "gateway-secret")
        self.assertIs(model.kwargs["use_responses_api"], False)

    def test_preserves_explicit_non_root_provider_path(self) -> None:
        config = ModelConfig(
            name="qwen-max",
            gateway_url="https://provider.example.org/openai/v1",
            gateway_key="gateway-key",
        )

        model = build_higress_model(config, model_factory=RecordingChatModel)

        self.assertEqual(model.kwargs["base_url"], "https://provider.example.org/openai/v1")


class MCPConnectionTests(unittest.TestCase):
    def test_maps_agentteams_http_and_sse_to_langchain_connections(self) -> None:
        servers = [
            {
                "name": "github",
                "url": "https://higress.example.org/mcp/github",
                "transport": "http",
            },
            {
                "name": "legacy",
                "url": "https://higress.example.org/mcp/legacy",
                "transport": "sse",
            },
        ]

        connections = build_mcp_connections(servers, gateway_key="gateway-secret")

        self.assertEqual(
            connections,
            {
                "github": {
                    "url": "https://higress.example.org/mcp/github",
                    "transport": "streamable_http",
                    "headers": {"Authorization": "Bearer gateway-secret"},
                },
                "legacy": {
                    "url": "https://higress.example.org/mcp/legacy",
                    "transport": "sse",
                    "headers": {"Authorization": "Bearer gateway-secret"},
                },
            },
        )

    def test_rejects_stdio_mcp_servers(self) -> None:
        servers = [
            {
                "name": "local-command",
                "url": "file:///bin/tool",
                "transport": "stdio",
            }
        ]

        with self.assertRaises(ConfigError):
            build_mcp_connections(servers, gateway_key="gateway-secret")


if __name__ == "__main__":
    unittest.main()
