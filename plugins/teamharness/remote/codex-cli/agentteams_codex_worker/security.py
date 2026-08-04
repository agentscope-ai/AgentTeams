"""Small redaction helpers used before diagnostics leave the bridge."""

from __future__ import annotations

import re
from typing import Iterable


_PATTERNS = (
    re.compile(r"(?i)(authorization\s*:\s*(?:bearer|basic)\s+)\S+"),
    re.compile(r"(?i)((?:access[_-]?token|api[_-]?key|secret|password)\s*[:=]\s*)\S+"),
)


class Redactor:
    def __init__(self, secret_values: Iterable[str] = ()) -> None:
        self._values = sorted(
            {value for value in secret_values if value and len(value) >= 8},
            key=len,
            reverse=True,
        )

    def redact(self, value: object) -> str:
        text = str(value)
        for secret in self._values:
            text = text.replace(secret, "[REDACTED]")
        for pattern in _PATTERNS:
            text = pattern.sub(r"\1[REDACTED]", text)
        return text
