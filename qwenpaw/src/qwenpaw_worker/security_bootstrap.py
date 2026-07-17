"""Session and credential guard bootstrap for QwenPaw workers."""

from __future__ import annotations

from qwenpaw_worker.config import WorkerConfig

SENSITIVE_FILE_AUTO_DENY_RULE = "SENSITIVE_FILE_BLOCK"
SESSION_FILE_PROMPT_POLICY = """Do not read, list, grep, glob, summarize, copy, or expose files under sessions/.
Session files are runtime-private state and may contain private conversation history.
This rule applies to all channels, users, and sessions, not only DingTalk."""
SESSION_FILE_PROMPT_POLICY_MARKER = "Session files are runtime-private state"


class SecurityBootstrap:
    def __init__(self, config: WorkerConfig) -> None:
        self.config = config

    def ensure_session_file_guard(self, root) -> None:
        security = root.security
        file_guard = security.file_guard
        tool_guard = security.tool_guard

        file_guard.enabled = True
        tool_guard.enabled = True
        tool_guard.guarded_tools = []

        session_path = f"{self.config.default_workspace_dir / 'sessions'}/"
        sensitive_files = [str(path) for path in (file_guard.sensitive_files or []) if str(path)]
        if session_path not in sensitive_files:
            sensitive_files.append(session_path)
        file_guard.sensitive_files = sensitive_files

        auto_denied_rules = [
            str(rule) for rule in (getattr(tool_guard, "auto_denied_rules", None) or []) if str(rule)
        ]
        if SENSITIVE_FILE_AUTO_DENY_RULE not in auto_denied_rules:
            auto_denied_rules.append(SENSITIVE_FILE_AUTO_DENY_RULE)
        tool_guard.auto_denied_rules = auto_denied_rules

    def ensure_session_file_prompt_policy(self) -> None:
        self.config.default_workspace_dir.mkdir(parents=True, exist_ok=True)
        for file_name in ("AGENTS.md", "SOUL.md"):
            prompt_file = self.config.default_workspace_dir / file_name
            existing = prompt_file.read_text(encoding="utf-8") if prompt_file.exists() else ""
            if SESSION_FILE_PROMPT_POLICY_MARKER in existing:
                continue
            separator = "\n" if existing and not existing.endswith("\n") else ""
            prefix = "\n" if existing.strip() else ""
            prompt_file.write_text(
                f"{existing}{separator}{prefix}{SESSION_FILE_PROMPT_POLICY}\n",
                encoding="utf-8",
            )
