"""Codex adapter (`codex exec`)."""
from __future__ import annotations

from cli_harness_worker.adapters.base import CLIAdapter, env_first, extra_cli_args


class CodexAdapter(CLIAdapter):
    runtime = "codex"
    binary = "codex"

    def required_env(self, model: str, base_url: str, api_key: str) -> dict[str, str]:
        env: dict[str, str] = {}
        if base_url:
            env["OPENAI_BASE_URL"] = base_url
        if api_key:
            env["OPENAI_API_KEY"] = api_key
        return env

    def build_command(self, prompt: str) -> list[str]:
        command = [
            self.binary,
            "exec",
            "--skip-git-repo-check",
            "--dangerously-bypass-approvals-and-sandbox",
        ]
        model = env_first("AGENTTEAMS_CLI_MODEL", "OPENAI_MODEL")
        if model:
            command += ["-m", model]
        command += extra_cli_args()
        command.append(prompt)
        return command
