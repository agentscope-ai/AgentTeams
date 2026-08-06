"""Secret redaction shared by all Codex execution roles."""

from __future__ import annotations

import os
import re
from collections.abc import Iterable

_PATTERNS = (
    re.compile(r"(?i)(authorization\s*:\s*(?:bearer|basic)\s+)\S+"),
    re.compile(r"(?i)((?:access[_-]?token|api[_-]?key|secret|password)\s*[:=]\s*)\S+"),
)


def environment_secret_values(source: dict[str, str] | None = None) -> tuple[str, ...]:
    """Collect AgentTeams service secret values without persisting their names."""

    source = os.environ if source is None else source
    sensitive_markers = ("TOKEN", "SECRET", "PASSWORD", "ACCESS_KEY", "API_KEY")
    return tuple(
        value
        for name, value in source.items()
        if name.startswith(("AGENTTEAMS_", "TEAMHARNESS_"))
        and any(marker in name.upper() for marker in sensitive_markers)
        and len(value) >= 8
    )


class Redactor:
    def __init__(self, secrets: Iterable[str] = ()) -> None:
        self.secrets = sorted(
            {str(secret) for secret in secrets if len(str(secret)) >= 8},
            key=len,
            reverse=True,
        )

    def redact(self, value: object) -> str:
        result = str(value)
        for secret in self.secrets:
            result = result.replace(secret, "[REDACTED]")
        for pattern in _PATTERNS:
            result = pattern.sub(r"\1[REDACTED]", result)
        return result
