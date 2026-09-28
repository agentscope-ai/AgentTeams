"""Kimi Code adapter (`kimi -p`)."""
from __future__ import annotations

from cli_harness_worker.adapters.base import CLIAdapter, env_first, extra_cli_args


class KimiCodeAdapter(CLIAdapter):
    runtime = "kimi-code"
    binary = "kimi"

    def required_env(self, model: str, base_url: str, api_key: str) -> dict[str, str]:
        env: dict[str, str] = {}
        if base_url:
            env["OPENAI_BASE_URL"] = base_url
            env["ANTHROPIC_BASE_URL"] = base_url
        if api_key:
            env["MOONSHOT_API_KEY"] = api_key
            env["OPENAI_API_KEY"] = api_key
            env["ANTHROPIC_AUTH_TOKEN"] = api_key
        return env

    def build_command(self, prompt: str) -> list[str]:
        command = [self.binary, "-p", prompt, "--yolo"]
        model = env_first("AGENTTEAMS_CLI_MODEL")
        if model:
            command += ["--model", model]
        command += extra_cli_args()
        return command
