"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import logging
from typing import Callable, Optional

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update.constants import (
    TEAMS_CONTEXT_END,
    TEAMS_CONTEXT_START,
    TEAMS_INTERNAL_CONTROL_MARKER,
    TEAMS_PROMPT_FILE,
)
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _section, _stable_json, _string, _string_fields

logger = logging.getLogger(__name__)


class TeamsPromptMixin:
    """TEAMS.md runtime context block writers."""

    config: WorkerConfig
    team_context_renderer: Optional[Callable[[MemberRuntimeConfig], str]]

    def _team_context_content_identity(self, config: MemberRuntimeConfig) -> str:
        facts = dict(config.team_context_facts)
        facts.pop("metadata", None)
        return _stable_json(facts)

    def _load_and_apply_once(self) -> None:
        self.apply_once(runtime_config=self.load(), reapply_adapter=False)

    def _apply_team_context_prompt(self, config: MemberRuntimeConfig) -> None:
        block = self._runtime_team_context_block(config)
        if not block:
            return
        path = self.config.default_workspace_dir / TEAMS_PROMPT_FILE
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            existing = path.read_text(encoding="utf-8")
        else:
            existing = self._render_full_team_context_prompt(config)
            if not existing:
                logger.warning(
                    "full TeamHarness TEAMS renderer unavailable component=update worker=%s action=fallback",
                    self.config.worker_name,
                )
                existing = "# TeamHarness Runtime Context\n"
        existing = self._ensure_teams_internal_marker(existing)
        if TEAMS_CONTEXT_START in existing and TEAMS_CONTEXT_END in existing:
            prefix, rest = existing.split(TEAMS_CONTEXT_START, 1)
            _old, suffix = rest.split(TEAMS_CONTEXT_END, 1)
            text = prefix.rstrip() + "\n\n" + block + suffix
        else:
            text = existing.rstrip() + "\n\n" + block + "\n"
        tmp = path.with_name(f".{path.name}.tmp")
        tmp.write_text(text, encoding="utf-8")
        tmp.replace(path)

    def _render_full_team_context_prompt(self, config: MemberRuntimeConfig) -> str:
        if self.team_context_renderer is None:
            return ""
        try:
            text = self.team_context_renderer(config)
        except Exception as exc:
            logger.warning(
                "full TeamHarness TEAMS renderer failed component=update worker=%s error_type=%s",
                self.config.worker_name,
                type(exc).__name__,
            )
            return ""
        return text if isinstance(text, str) and text.strip() else ""

    def _ensure_teams_internal_marker(self, text: str) -> str:
        if TEAMS_INTERNAL_CONTROL_MARKER in text:
            return text
        body = text.lstrip("\n")
        return f"{TEAMS_INTERNAL_CONTROL_MARKER}\n{body}" if body else f"{TEAMS_INTERNAL_CONTROL_MARKER}\n"

    def _runtime_team_context_block(self, config: MemberRuntimeConfig) -> str:
        facts = config.team_context_facts
        if not facts:
            return ""
        team = _section(facts, "team")
        member = _section(facts, "member")
        lines = [
            TEAMS_CONTEXT_START,
            "## Runtime Team Context",
            "",
        ]
        for key, value in (
            ("team.name", team.get("name")),
            ("team.teamRoomId", team.get("teamRoomId")),
            ("team.leaderName", team.get("leaderName")),
            ("team.leaderRuntimeName", team.get("leaderRuntimeName")),
            ("team.leaderDmRoomId", team.get("leaderDmRoomId")),
            ("team.admin.name", _section(team, "admin").get("name")),
            ("team.admin.matrixUserId", _section(team, "admin").get("matrixUserId")),
            ("member.name", member.get("name")),
            ("member.runtimeName", member.get("runtimeName")),
            ("member.role", member.get("role")),
            ("member.runtime", member.get("runtime")),
            ("member.matrixUserId", member.get("matrixUserId")),
            ("member.personalRoomId", member.get("personalRoomId")),
        ):
            text = _string(value)
            if text:
                lines.append(f"- {key}: {text}")
        members = team.get("members")
        if isinstance(members, list) and members:
            lines.extend(["", "### Team Members"])
            for item in members:
                entry = _string_fields(item, ("name", "runtimeName", "role", "matrixUserId", "personalRoomId"))
                if entry:
                    lines.append("- " + ", ".join(f"{key}: {value}" for key, value in entry.items()))
        lines.extend(["", "Do not write secrets, credentials, or live task status into this file.", TEAMS_CONTEXT_END])
        return "\n".join(lines)
