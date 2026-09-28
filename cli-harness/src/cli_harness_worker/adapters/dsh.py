"""DeepSeek Harness adapter (`dsh run --headless`)."""
from __future__ import annotations

from cli_harness_worker.adapters.base import CLIAdapter, env_first, extra_cli_args


class DSHAdapter(CLIAdapter):
    runtime = "dsh"
    binary = "dsh"

    def required_env(self, model: str, base_url: str, api_key: str) -> dict[str, str]:
        env: dict[str, str] = {}
        if base_url:
            env["OPENAI_BASE_URL"] = base_url
            env["DEEPSEEK_BASE_URL"] = base_url
        if api_key:
            env["OPENAI_API_KEY"] = api_key
            env["DEEPSEEK_API_KEY"] = api_key
        return env

    def build_command(self, prompt: str) -> list[str]:
        command = [self.binary, "run", "--headless", "--task", prompt]
        model = env_first("AGENTTEAMS_CLI_MODEL", "DEEPSEEK_MODEL")
        if model:
            command += ["--model", model]
        command += extra_cli_args()
        return command
