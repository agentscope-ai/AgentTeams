"""Push allow/deny policy for background Local -> Remote sync."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class PushPolicy:
    """Rules for which local files may be uploaded by ``push_local``."""

    exclude_files: frozenset[str] = frozenset()
    exclude_basenames: frozenset[str] = frozenset()
    exclude_paths: frozenset[str] = frozenset()
    exclude_dirs: frozenset[str] = frozenset()
    exclude_extensions: frozenset[str] = frozenset()
    exclude_path_prefixes: tuple[str, ...] = ()

    def should_skip(self, rel: Path) -> bool:
        if rel.name in self.exclude_basenames:
            return True
        if len(rel.parts) == 1 and rel.name in self.exclude_files:
            return True
        rel_posix = rel.as_posix()
        if rel_posix in self.exclude_paths:
            return True
        if any(part in self.exclude_dirs for part in rel.parts):
            return True
        if rel.suffix in self.exclude_extensions:
            return True
        return any(
            rel_posix == prefix or rel_posix.startswith(f"{prefix}/")
            for prefix in self.exclude_path_prefixes
        )

    @classmethod
    def copaw(cls) -> PushPolicy:
        """CoPaw Worker background push contract (Phase 6 Y6.2)."""
        return cls(
            exclude_files=frozenset({"openclaw.json", "mcporter-servers.json"}),
            exclude_paths=frozenset(
                {
                    "config/mcporter.json",
                    ".copaw/workspaces/default/config/mcporter.json",
                }
            ),
            exclude_dirs=frozenset(
                {
                    ".agents",
                    ".cache",
                    ".npm",
                    ".local",
                    ".mc",
                    "custom_channels",
                    "active_skills",
                    "__pycache__",
                }
            ),
            exclude_extensions=frozenset({".lock"}),
            exclude_path_prefixes=(
                ".copaw/workspaces/default/skills",
                ".copaw/workspaces/default/shared",
                ".copaw/workspaces/default/global-shared",
                "shared",
                "global-shared",
            ),
        )

    @classmethod
    def hermes(cls) -> PushPolicy:
        """Hermes Worker contract (Phase 6 Y6.3)."""
        return cls(
            exclude_files=frozenset({"openclaw.json", "mcporter-servers.json"}),
            exclude_paths=frozenset({"config/mcporter.json"}),
            exclude_dirs=frozenset(
                {
                    ".agents",
                    ".cache",
                    ".npm",
                    ".local",
                    ".mc",
                    "platforms",
                    "matrix-nio-store",
                    "image_cache",
                    "audio_cache",
                    "document_cache",
                    "cache",
                    "logs",
                    "__pycache__",
                    "shared",
                }
            ),
            exclude_extensions=frozenset({".lock", ".db-journal", ".db-wal", ".db-shm"}),
        )

    @classmethod
    def qwenpaw(cls) -> PushPolicy:
        """QwenPaw Worker background push contract (Phase 6 Y6.6)."""
        return cls(
            exclude_basenames=frozenset(
                {".DS_Store", "qwenpaw.log", "heartbeat.json", "token_usage.json"}
            ),
            exclude_dirs=frozenset({".cache", ".local", ".mc", "__pycache__", "logs"}),
            exclude_extensions=frozenset({".lock", ".pyc"}),
            exclude_path_prefixes=(
                "credentials",
                "runtime",
                "shared",
                "global-shared",
                ".qwenpaw/workspaces/default/shared",
                ".qwenpaw/workspaces/default/global-shared",
                ".qwenpaw/workspaces/default/tool_result",
                ".qwenpaw/workspaces/default/file_store",
                ".qwenpaw/workspaces/default/media",
                ".qwenpaw/workspaces/default/embedding_cache",
            ),
        )

    @classmethod
    def openclaw(cls) -> PushPolicy:
        """OpenClaw Worker background push contract (Phase 6 Y6.4)."""
        return cls(
            exclude_files=frozenset(
                {"openclaw.json", "mcporter-servers.json", "SOUL.md", "AGENTS.md", "HEARTBEAT.md"}
            ),
            exclude_paths=frozenset({"config/mcporter.json"}),
            exclude_dirs=frozenset({".agents", ".cache", ".npm", ".local", ".mc"}),
            exclude_extensions=frozenset({".lock"}),
            exclude_path_prefixes=(".openclaw/matrix", ".openclaw/canvas", "credentials"),
        )

    @classmethod
    def openhuman(cls) -> PushPolicy:
        """OpenHuman Worker contract (Phase 6 Y6.5 — bash loops reference preset)."""
        return cls(
            exclude_files=frozenset({"config.toml", ".last-pull"}),
            exclude_dirs=frozenset({"agent-config", "config"}),
        )
