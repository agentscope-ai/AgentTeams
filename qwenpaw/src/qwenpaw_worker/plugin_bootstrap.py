"""QwenPaw builtin plugin staging and zip install helpers."""

from __future__ import annotations

import hashlib
import logging
import os
import shutil
import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path
from typing import Callable, Optional

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.plugin_install import (
    BUILTIN_QWENPAW_PLUGIN_MARKER,
    directory_digest,
    install_qwenpaw_plugin_package,
    safe_extract_zip,
)

logger = logging.getLogger(__name__)

DEFAULT_BUILTIN_QWENPAW_PLUGINS_DIR = Path("/opt/agentteams/qwenpaw-builtin/plugins")


class PluginBootstrap:
    def __init__(
        self,
        config: WorkerConfig,
        *,
        log_step_begin: Callable[..., float],
        log_step_complete: Callable[..., None],
        log_step_failed: Callable[..., None],
    ) -> None:
        self.config = config
        self._log_step_begin = log_step_begin
        self._log_step_complete = log_step_complete
        self._log_step_failed = log_step_failed

    def prepare_default_plugins(self) -> None:
        builtin_root = self._builtin_plugins_dir()
        self.prepare_builtin_plugin("teamharness", builtin_root / "teamharness")
        self.prepare_builtin_plugin("workerflow", builtin_root / "workerflow")

    def install_default_plugins(self) -> None:
        self.install_teamharness_plugin()
        self.install_workerflow_plugin()

    def prepare_builtin_plugin(self, plugin_name: str, source_dir: Path) -> None:
        target_dir = self.config.qwenpaw_working_dir / "plugins" / plugin_name
        step_started = self._log_step_begin(
            plugin_name,
            "prepare_builtin",
            source_dir=source_dir,
            target_dir=target_dir,
        )
        try:
            self._validate_builtin_plugin(plugin_name, source_dir)
            if self._builtin_plugin_current(source_dir, target_dir):
                self._log_step_complete(plugin_name, "prepare_builtin", step_started, action="unchanged")
                return
            if target_dir.is_symlink() or target_dir.is_file():
                target_dir.unlink()
            elif target_dir.exists():
                shutil.rmtree(target_dir)
            target_dir.parent.mkdir(parents=True, exist_ok=True)
            shutil.copytree(source_dir, target_dir)
        except Exception as exc:
            self._log_step_failed(plugin_name, "prepare_builtin", step_started, exc)
            raise
        self._log_step_complete(plugin_name, "prepare_builtin", step_started, action="copied")

    def install_teamharness_plugin(self) -> None:
        plugin_source = Path(
            os.getenv(
                "AGENTTEAMS_TEAMHARNESS_QWENPAW_PLUGIN_PACKAGE",
                "/opt/agentteams/plugins/teamharness-qwenpaw.zip",
            )
        )
        self._install_plugin_package("teamharness", plugin_source, "teamharness-qwenpaw-plugin-")

    def install_workerflow_plugin(self) -> None:
        plugin_source = Path(
            os.getenv(
                "AGENTTEAMS_WORKERFLOW_QWENPAW_PLUGIN_PACKAGE",
                "/opt/agentteams/plugins/workerflow-qwenpaw.zip",
            )
        )
        self._install_plugin_package("workerflow", plugin_source, "workerflow-qwenpaw-plugin-")

    def _builtin_plugins_dir(self) -> Path:
        configured = os.getenv("AGENTTEAMS_BUILTIN_QWENPAW_PLUGINS_DIR", "").strip()
        return Path(configured) if configured else DEFAULT_BUILTIN_QWENPAW_PLUGINS_DIR

    def _validate_builtin_plugin(self, plugin_name: str, plugin_dir: Path) -> None:
        if not plugin_dir.is_dir():
            raise RuntimeError(f"built-in {plugin_name} qwenpaw plugin missing: {plugin_dir}")
        for file_name in ("plugin.json", "plugin.py", BUILTIN_QWENPAW_PLUGIN_MARKER):
            path = plugin_dir / file_name
            if not path.is_file():
                raise RuntimeError(f"built-in {plugin_name} qwenpaw plugin file missing: {path}")

    def _builtin_plugin_current(self, source_dir: Path, target_dir: Path) -> bool:
        source_marker = source_dir / BUILTIN_QWENPAW_PLUGIN_MARKER
        target_marker = target_dir / BUILTIN_QWENPAW_PLUGIN_MARKER
        if not (
            target_marker.is_file()
            and (target_dir / "plugin.json").is_file()
            and (target_dir / "plugin.py").is_file()
        ):
            return False
        expected_digest = source_marker.read_text(encoding="utf-8").strip()
        if not expected_digest:
            return False
        return (
            target_marker.read_text(encoding="utf-8").strip() == expected_digest
            and directory_digest(target_dir) == expected_digest
        )

    def _install_plugin_package(self, plugin_name: str, plugin_source: Path, temp_prefix: str) -> None:
        package_type = self._plugin_package_type(plugin_source)
        step_started = self._log_step_begin(
            plugin_name,
            "install",
            package_type=package_type,
            package_path=plugin_source,
        )
        try:
            qwenpaw_bin = shutil.which("qwenpaw") or str(Path(sys.executable).with_name("qwenpaw"))
            install_qwenpaw_plugin_package(qwenpaw_bin, plugin_source, temp_prefix=temp_prefix)
        except Exception as exc:
            self._log_step_failed(plugin_name, "install", step_started, exc, package_type=package_type)
            raise
        self._log_step_complete(plugin_name, "install", step_started, package_type=package_type)

    def _plugin_package_type(self, plugin_source: Path) -> str:
        if plugin_source.is_dir():
            return "directory"
        if plugin_source.exists() and zipfile.is_zipfile(plugin_source):
            return "zip"
        if not plugin_source.exists():
            return "missing"
        return "unsupported"

    @staticmethod
    def extract_plugin_zip(zip_path: Path, target_dir: Path) -> Path:
        return safe_extract_zip(zip_path, target_dir)
