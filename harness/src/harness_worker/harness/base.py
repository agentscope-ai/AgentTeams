"""Base harness interface."""
from __future__ import annotations

from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any


class BaseHarness(ABC):
    """One subprocess invocation per message. No persistent process."""

    name: str

    @abstractmethod
    def bridge_config(self, openclaw_cfg: dict[str, Any], harness_home: Path) -> None:
        """Write this harness's native config file(s) under harness_home."""

    @abstractmethod
    def build_command(
        self,
        message: str,
        session_id: str | None,
        workspace: Path,
    ) -> list[str]:
        """Return argv for one non-interactive invocation."""

    @abstractmethod
    def parse_output(self, stdout_bytes: bytes) -> tuple[str, str | None]:
        """Parse JSON/JSONL stdout. Return (assistant_text, new_session_id)."""

    @abstractmethod
    def env(self, openclaw_cfg: dict[str, Any]) -> dict[str, str]:
        """Per-harness auth env vars."""

    def process_stream_line(self, line: str, state: dict) -> None:
        """Parse one JSONL line from streaming stdout. Mutates state dict.

        Keys written to state:
          text_chunks: list[str]  — accumulated text fragments
          session_id:  str | None — session id from result event
        """


_HARNESS_REGISTRY: dict[str, type[BaseHarness]] = {}


def register_harness(name: str):
    def decorator(cls: type[BaseHarness]) -> type[BaseHarness]:
        _HARNESS_REGISTRY[name] = cls
        return cls
    return decorator


def build_harness(name: str) -> BaseHarness:
    cls = _HARNESS_REGISTRY.get(name)
    if not cls:
        raise ValueError(f"Unknown harness: {name}. Known: {list(_HARNESS_REGISTRY)}")
    return cls()