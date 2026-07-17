"""CoPaw workspace layout: materialize MinIO L2 state into runtime space."""

from __future__ import annotations

import logging
import os
import shutil
import sys
import types
from pathlib import Path
from typing import Any, Callable, Optional

from copaw_worker.bridge import (
    bridge_config,
    bridge_runtime_to_standard,
    bootstrap_copaw_runtime,
    propagate_prompts,
)

logger = logging.getLogger(__name__)


class WorkspaceLayout:
    """Owns the four workspace verbs: pull (FileSync), bridge, propagate, save."""

    def __init__(
        self,
        local_dir: Path,
        copaw_working_dir: Path,
        *,
        profile: str = "worker",
    ) -> None:
        self.local_dir = Path(local_dir)
        self.copaw_working_dir = Path(copaw_working_dir)
        self.profile = profile
        self._runtime_bootstrapped = False

    @classmethod
    def for_sync(cls, sync: Any, *, profile: str = "worker") -> WorkspaceLayout:
        """Build layout paths from a FileSync instance."""
        local_dir = Path(sync.local_dir)
        return cls(
            local_dir=local_dir,
            copaw_working_dir=local_dir / ".copaw",
            profile=profile,
        )

    def materialize(
        self,
        openclaw_cfg: dict[str, Any],
        *,
        bootstrap: bool = True,
    ) -> None:
        """Startup: propagate prompts, write CoPaw JSON, bootstrap runtime once."""
        self.copaw_working_dir.mkdir(parents=True, exist_ok=True)
        propagate_prompts(self.local_dir, self.copaw_working_dir)
        bridge_config(openclaw_cfg, self.copaw_working_dir, profile=self.profile)
        if bootstrap and not self._runtime_bootstrapped:
            bootstrap_copaw_runtime(self.copaw_working_dir)
            self._runtime_bootstrapped = True
        self.ensure_skills_symlink()
        self.copy_mcporter_config()
        self.install_matrix_channel_shim()

    def rebridge(self, openclaw_cfg: dict[str, Any]) -> None:
        """Refresh bridged JSON after openclaw.json changes without re-patching."""
        propagate_prompts(self.local_dir, self.copaw_working_dir)
        bridge_config(openclaw_cfg, self.copaw_working_dir, profile=self.profile)

    def persist_edits(self) -> None:
        """Save agent-edited runtime prompts back to standard (L2) space."""
        bridge_runtime_to_standard(self.local_dir)

    def ensure_skills_symlink(self) -> Path:
        """Symlink workspaces/default/skills -> standard-space skills/."""
        workspace_dir = self.copaw_working_dir / "workspaces" / "default"
        workspace_dir.mkdir(parents=True, exist_ok=True)
        standard_skills = self.local_dir / "skills"
        standard_skills.mkdir(parents=True, exist_ok=True)
        link_path = workspace_dir / "skills"

        if link_path.is_symlink():
            try:
                if link_path.resolve() == standard_skills.resolve():
                    return link_path
            except OSError:
                pass
            link_path.unlink()

        if link_path.exists() and not link_path.is_symlink():
            logger.warning(
                "skills path %s exists and is not a symlink; leaving in place",
                link_path,
            )
            return link_path

        rel_target = os.path.relpath(standard_skills, link_path.parent)
        try:
            link_path.symlink_to(rel_target, target_is_directory=True)
        except OSError:
            try:
                link_path.symlink_to(standard_skills.resolve(), target_is_directory=True)
            except OSError as exc:
                logger.warning(
                    "Could not create skills symlink at %s (%s); "
                    "skills remain at %s",
                    link_path,
                    exc,
                    standard_skills,
                )
                return standard_skills
        logger.info(
            "skills symlink: %s -> %s (target %s)",
            link_path,
            rel_target,
            standard_skills,
        )
        return link_path

    def copy_mcporter_config(self) -> None:
        """Copy mcporter.json into CoPaw working dir for cwd-relative lookup."""
        src = self.local_dir / "config" / "mcporter.json"
        if not src.exists():
            return
        dst = self.copaw_working_dir / "config" / "mcporter.json"
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)
        logger.info("mcporter config copied to %s", dst)

    def install_matrix_channel_shim(self) -> None:
        """Install unified Matrix overlay entry point into custom_channels/."""
        custom_channels_dir = self.copaw_working_dir / "custom_channels"
        custom_channels_dir.mkdir(parents=True, exist_ok=True)
        dst = custom_channels_dir / "matrix_channel.py"
        shim = (
            '"""AgentTeams Matrix custom channel (unified overlay)."""\n'
            "from copaw.app.channels.matrix.channel import MatrixChannel\n\n"
            '__all__ = ["MatrixChannel"]\n'
        )
        dst.write_text(shim, encoding="utf-8")
        logger.debug("MatrixChannel shim installed to %s", dst)

    def sync_skills(
        self,
        *,
        list_skills: Callable[[], list[str]],
        worker_name: str,
    ) -> None:
        """Seed CoPaw builtins and reconcile legacy active_skills/ copies."""
        active_skills_dir = self.copaw_working_dir / "active_skills"
        active_skills_dir.mkdir(parents=True, exist_ok=True)

        self._dedup_customized_skills()

        try:
            from copaw.agents.skills_manager import sync_skills_to_working_dir

            synced, skipped = sync_skills_to_working_dir(
                skill_names=None, force=False
            )
            logger.info(
                "Seeded CoPaw built-in skills: %d installed, %d already existed",
                synced,
                skipped,
            )
        except Exception as exc:
            logger.warning("Failed to seed CoPaw built-in skills: %s", exc)

        skill_names = list_skills()
        if skill_names:
            logger.info(
                "Skills available for worker %s: %s",
                worker_name,
                ", ".join(skill_names),
            )
        else:
            logger.info("No extra skills in MinIO for worker %s", worker_name)

        builtin_names = self._builtin_skill_names()
        keep_names = builtin_names | {"file-sync"}
        for child in list(active_skills_dir.iterdir()):
            if child.is_dir() and child.name not in keep_names:
                shutil.rmtree(child)
                logger.info("Removed stale active skill: %s", child.name)

    def _dedup_customized_skills(self) -> None:
        customized_dir = self.copaw_working_dir / "customized_skills"
        if not customized_dir.is_dir():
            return

        builtin_names = self._builtin_skill_names()
        if not builtin_names:
            return

        for child in list(customized_dir.iterdir()):
            if child.is_dir() and child.name in builtin_names:
                shutil.rmtree(child)
                logger.info(
                    "Removed stale customized skill '%s' (now a builtin)",
                    child.name,
                )

    @staticmethod
    def _builtin_skill_names() -> set[str]:
        try:
            import copaw.agents.skills as skills_pkg

            builtin_skills_root = Path(skills_pkg.__file__).resolve().parent
        except (ImportError, AttributeError):
            return set()

        if not builtin_skills_root.is_dir():
            return set()

        return {
            child.name
            for child in builtin_skills_root.iterdir()
            if child.is_dir() and not child.name.startswith("_")
        }


def install_fake_copaw_skills_modules(monkeypatch, tmp_path: Path) -> None:
    """Test helper: minimal copaw.agents.skills* stand-ins."""
    builtin_root = tmp_path / "builtin_skills_pkg"
    (builtin_root / "pdf").mkdir(parents=True)

    skills_pkg = types.ModuleType("copaw.agents.skills")
    skills_pkg.__file__ = str(builtin_root / "__init__.py")

    skills_manager = types.ModuleType("copaw.agents.skills_manager")
    skills_manager.sync_skills_to_working_dir = (
        lambda skill_names=None, force=False: (0, 0)
    )

    monkeypatch.setitem(sys.modules, "copaw", types.ModuleType("copaw"))
    monkeypatch.setitem(sys.modules, "copaw.agents", types.ModuleType("copaw.agents"))
    monkeypatch.setitem(sys.modules, "copaw.agents.skills", skills_pkg)
    monkeypatch.setitem(
        sys.modules, "copaw.agents.skills_manager", skills_manager
    )
