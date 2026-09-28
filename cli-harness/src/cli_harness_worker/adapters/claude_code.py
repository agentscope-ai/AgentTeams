"""Claude Code adapter (`claude -p`)."""
from __future__ import annotations

from cli_harness_worker.adapters.base import CLIAdapter, env_first, extra_cli_args


class ClaudeCodeAdapter(CLIAdapter):
    runtime = "claude-code"
    binary = "claude"

    def required_env(self, model: str, base_url: str, api_key: str) -> dict[str, str]:
        env: dict[str, str] = {}
        if base_url:
            env["ANTHROPIC_BASE_URL"] = base_url
        if api_key:
            # AUTH_TOKEN is sent verbatim as the auth header; ANTHROPIC_API_KEY
            # would make the CLI attempt its own OAuth login flow.
            env["ANTHROPIC_AUTH_TOKEN"] = api_key
        return env

    def build_command(self, prompt: str) -> list[str]:
        command = [
            self.binary,
            "-p",
            prompt,
            "--dangerously-skip-permissions",
        ]
        model = env_first("AGENTTEAMS_CLI_MODEL", "ANTHROPIC_MODEL")
        if model:
            command += ["--model", model]
        command += extra_cli_args()
        return command
