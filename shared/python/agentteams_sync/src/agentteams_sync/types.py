"""Shared sync types."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol


class HealthStateProtocol(Protocol):
    def update(
        self,
        component: str,
        healthiness: str,
        message: str = "",
        details: dict[str, Any] | None = None,
    ) -> Any:
        ...


@dataclass(frozen=True)
class SharedPath:
    """Resolved local and remote paths for a shared file operation."""

    kind: str
    subpath: str
    local: Path
    remote: str
