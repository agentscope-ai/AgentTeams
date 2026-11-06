# -*- coding: utf-8 -*-
"""Service layer for coding CLI management (probe/settings/install).

Ported from QwenPaw upstream PR 8156
(``src/qwenpaw/coding_cli/service.py``). Deltas vs upstream:

- Install task state is file-backed under the plugin state directory
  (``<workspace>/.qwenpaw/coding-cli/install/<task_id>.json``, design doc
  §3) instead of an in-memory dict, so state survives hot reloads and
  restarts; the service can be constructed per request without any
  module-level mutable state.
- "Another install of the same CLI is running" is detected by scanning
  task files (``running_task_for``) instead of an in-memory busy set.
  A stale ``running`` task file (e.g. after a hard crash mid-install)
  blocks new installs of that CLI until the file is removed manually.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import time
import uuid
from pathlib import Path
from typing import Any

from .registry import PRESERVE, CliSpec
from .state import install_dir as _default_install_dir

#: Maximum characters of npm output kept in install task logs.
_LOG_TAIL_LIMIT = 4096

_VERSION_RE = re.compile(r"(\d+\.\d+\.\d+[\w.-]*)")

#: npm dist-tags and version strings look like this (no shell/space chars).
_VALID_TAG_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")

#: Install task ids are ``uuid4().hex``; the check also keeps task file
#: names safe against traversal via ``install_status`` input.
_TASK_ID_RE = re.compile(r"^[0-9a-f]{32}$")


class InstallBusyError(RuntimeError):
    """Raised when an install task for the CLI is already running."""


def _expand(path: str) -> str:
    return os.path.expanduser(path)


def _set_dot_path(data: dict[str, Any], dot_path: str, value: Any) -> None:
    """Set ``data[a][b] = value`` for dot-path ``a.b``."""
    keys = dot_path.split(".")
    cur = data
    for key in keys[:-1]:
        cur = cur.setdefault(key, {})
    cur[keys[-1]] = value


def _get_dot_path(data: dict[str, Any], dot_path: str) -> Any:
    cur: Any = data
    for key in dot_path.split("."):
        if not isinstance(cur, dict) or key not in cur:
            return None
        cur = cur[key]
    return cur


def _deep_merge(base: dict[str, Any], patch: dict[str, Any]) -> dict[str, Any]:
    for key, value in patch.items():
        if value == PRESERVE and isinstance(
            base.get(key),
            (str, int, float, bool),
        ):
            # Credential placeholder: keep the existing value.
            continue
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            base[key] = _deep_merge(base[key], value)
        else:
            base[key] = value
    return base


def redact_settings(spec: CliSpec, data: dict[str, Any]) -> dict[str, Any]:
    """Return a copy with credential dot-paths redacted.

    Each redacted leaf becomes ``{"redacted": true, "has_value": bool}``.
    """
    out = json.loads(json.dumps(data))
    for dot_path in spec.auth_paths:
        parent = _get_dot_path(out, ".".join(dot_path.split(".")[:-1]))
        if isinstance(parent, dict):
            leaf = dot_path.split(".")[-1]
            if leaf in parent:
                has_value = bool(parent[leaf])
                parent[leaf] = {
                    "redacted": True,
                    "has_value": has_value,
                }
    return out


def _auth_configured(spec: CliSpec, settings: dict[str, Any]) -> bool:
    # Env-var auth (qoder PAT): a non-empty value counts as configured.
    # Checked first — Qoder's own rule is PAT-over-login.
    if spec.auth_env_vars:
        return any(os.environ.get(v) for v in spec.auth_env_vars)
    if spec.auth_paths:
        return any(bool(_get_dot_path(settings, p)) for p in spec.auth_paths)
    # CLI whose credentials live in separate files.
    return any(
        os.path.isfile(_expand(p)) and os.path.getsize(_expand(p)) > 0
        for p in spec.extra_auth_files
    )


class CodingCliService:
    """Probe, settings and install management for registered CLIs.

    Install task state is persisted as JSON files under ``install_dir``
    (design doc §3), so instances are cheap and interchangeable: the
    HTTP layer constructs one per request.
    """

    def __init__(self, install_dir: str | Path | None = None) -> None:
        self._install_dir = (
            Path(install_dir).expanduser()
            if install_dir is not None
            else _default_install_dir()
        )
        self._install_dir.mkdir(parents=True, exist_ok=True)

    # -- install task file helpers ---------------------------------------

    def _task_path(self, task_id: str) -> Path:
        return self._install_dir / f"{task_id}.json"

    def _write_task(self, task_id: str, task: dict[str, Any]) -> None:
        path = self._task_path(task_id)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(
            json.dumps(task, ensure_ascii=False),
            encoding="utf-8",
        )
        os.replace(tmp, path)

    def _read_task(self, task_id: str) -> dict[str, Any] | None:
        if not _TASK_ID_RE.fullmatch(task_id):
            return None
        try:
            data = json.loads(self._task_path(task_id).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return None
        return data if isinstance(data, dict) else None

    # -- probing ---------------------------------------------------------

    async def probe(self, spec: CliSpec) -> dict[str, Any]:
        """Collect installed/version/auth facts for one CLI."""
        info: dict[str, Any] = {
            "id": spec.id,
            "name": spec.name,
            "installed": False,
            "version": None,
            "auth": {"configured": False, "type": None},
            "installable": await asyncio.to_thread(
                self._installable,
                spec,
            ),
        }
        # Probes must stay snappy for the UI: a version check that takes
        # longer than 10s is treated as "not installed", not waited on.
        proc = await asyncio.to_thread(
            self._run,
            (spec.bin_name, "--version"),
            10.0,
        )
        if proc is not None and proc.returncode == 0:
            info["installed"] = True
            match = _VERSION_RE.search(proc.stdout or "")
            if match:
                info["version"] = match.group(1)
        settings = await asyncio.to_thread(self._load_settings, spec)
        # Always evaluate auth: for CLIs whose credentials live in separate
        # files (e.g. opencode) the settings file may be absent while the
        # credentials are present.
        info["auth"]["configured"] = _auth_configured(spec, settings or {})
        if settings is not None:
            auth = _get_dot_path(settings, "security.auth")
            if isinstance(auth, dict):
                info["auth"]["type"] = auth.get("selectedType")
        return info

    @staticmethod
    def _installable(spec: CliSpec) -> bool:
        from shutil import which

        return all(which(binary) for binary in spec.requires)

    @staticmethod
    def _run(cmd: tuple[str, ...], timeout: float = 30.0) -> Any:
        import subprocess

        try:
            return subprocess.run(
                list(cmd),
                capture_output=True,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.SubprocessError):
            return None

    @staticmethod
    def _cli_prefix() -> str:
        """Persisted npm prefix (container rebuild safe), or "" for default."""
        import os as _os

        raw = _os.environ.get("QWEN_CODE_BRIDGE_CLI_PREFIX", "").strip()
        if raw:
            return raw
        # QwenPaw containers persist /app/working (qwenpaw-data); keep the
        # CLI next to the bridge data instead of the ephemeral image layer.
        if _os.path.isdir("/app/working"):
            return "/app/working/.qwen-cli"
        return ""

    @staticmethod
    def _load_settings(spec: CliSpec) -> dict[str, Any] | None:
        path = _expand(spec.settings_path)
        if not os.path.isfile(path):
            return None
        try:
            with open(path, encoding="utf-8") as fh:
                data = json.load(fh)
            return data if isinstance(data, dict) else None
        except (OSError, json.JSONDecodeError):
            return None

    # -- settings --------------------------------------------------------

    async def read_settings(self, spec: CliSpec) -> dict[str, Any]:
        """Read settings with credentials redacted."""
        raw = await asyncio.to_thread(self._load_settings, spec)
        return redact_settings(spec, raw or {})

    async def write_settings(
        self,
        spec: CliSpec,
        patch: dict[str, Any],
    ) -> dict[str, Any]:
        """Merge ``patch`` into settings; ``***`` keeps existing values."""
        path = _expand(spec.settings_path)

        def _apply() -> dict[str, Any]:
            current: dict[str, Any] = {}
            if os.path.isfile(path):
                try:
                    with open(path, encoding="utf-8") as fh:
                        loaded = json.load(fh)
                    if isinstance(loaded, dict):
                        current = loaded
                except (OSError, json.JSONDecodeError):
                    current = {}
            _deep_merge(current, patch)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            tmp = path + ".tmp"
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump(current, fh, indent=2, ensure_ascii=False)
                fh.write("\n")
            os.replace(tmp, path)
            return redact_settings(spec, current)

        return await asyncio.to_thread(_apply)

    # -- install ---------------------------------------------------------

    async def start_install(self, spec: CliSpec, tag: str) -> str:
        """Start an async npm install/upgrade; return task id.

        ``tag`` may be an npm dist-tag (``latest``, ``nightly``) or a
        concrete version string (``0.25.0``); anything else is rejected.
        """
        if not _VALID_TAG_RE.fullmatch(tag):
            raise ValueError(f"Invalid install tag: {tag!r}")
        if self.running_task_for(spec.id) is not None:
            raise InstallBusyError(
                f"An install task for {spec.id} is already running",
            )
        task_id = uuid.uuid4().hex
        self._write_task(task_id, {
            "task_id": task_id,
            "cli": spec.id,
            "tag": tag,
            "state": "running",
            "version": None,
            "log_tail": "",
            "started_at": time.time(),
            "finished_at": None,
        })
        asyncio.get_running_loop().create_task(
            self._run_install(spec, task_id),
        )
        return task_id

    async def _run_install(self, spec: CliSpec, task_id: str) -> None:
        task = self._read_task(task_id)
        if task is None:
            return
        try:
            spec_check = await asyncio.to_thread(
                self._installable,
                spec,
            )
            if not spec_check:
                raise RuntimeError(
                    "npm is not available in this worker",
                )
            # ``npm install -g pkg@<tag>`` accepts dist-tags (latest,
            # nightly) and concrete versions (0.25.0) uniformly, so the
            # tag is always appended explicitly.
            version_ref = f"{spec.npm_package}@{task['tag']}"
            prefix = self._cli_prefix()
            cmd = (
                ("npm", "install", "-g", "--prefix", prefix, version_ref)
                if prefix
                else ("npm", "install", "-g", version_ref)
            )
            proc = await asyncio.to_thread(self._run, cmd)
            if proc is None:
                raise RuntimeError("npm failed to start")
            task["log_tail"] = (proc.stdout or "")[-_LOG_TAIL_LIMIT:]
            if proc.returncode != 0:
                task["log_tail"] = ((proc.stderr or proc.stdout or ""))[
                    -_LOG_TAIL_LIMIT:
                ]
                raise RuntimeError(f"npm exit {proc.returncode}")
            bin_path = (
                f"{prefix}/bin/{spec.bin_name}"
                if prefix and os.path.exists(f"{prefix}/bin/{spec.bin_name}")
                else spec.bin_name
            )
            check = await asyncio.to_thread(
                self._run,
                (bin_path, "--version"),
            )
            if check is not None and check.returncode == 0:
                match = _VERSION_RE.search(check.stdout or "")
                if match:
                    task["version"] = match.group(1)
            task["state"] = "success"
            self._write_task(task_id, task)
        except Exception as exc:  # noqa: BLE001 - report all failures
            task["state"] = "failed"
            task["log_tail"] = str(exc)[-_LOG_TAIL_LIMIT:]
        finally:
            task["finished_at"] = time.time()
            self._write_task(task_id, task)

    def install_status(self, task_id: str) -> dict[str, Any] | None:
        return self._read_task(task_id)

    def running_task_for(self, cli_id: str) -> dict[str, Any] | None:
        """Find the running task for a CLI, if any."""
        if not self._install_dir.is_dir():
            return None
        for path in sorted(self._install_dir.glob("*.json")):
            task = self._read_task(path.stem)
            if (
                task is not None
                and task.get("cli") == cli_id
                and task.get("state") == "running"
            ):
                return task
        return None


__all__ = [
    "CodingCliService",
    "InstallBusyError",
    "redact_settings",
]
