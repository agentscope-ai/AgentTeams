"""MinIO file sync for the CLI-harness worker.

Same ownership contract as hermes/copaw workers:

  Manager-managed (Worker pull-only):
    openclaw.json, mcporter-servers.json, config/mcporter.json, skills/, shared/

  Worker-managed (pushed back to MinIO):
    AGENTS.md, SOUL.md, memory/, progress/, task outputs, CLI session state

All object operations shell out to the ``mc`` CLI.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import shutil
import subprocess
import time
from pathlib import Path
from typing import Callable, Coroutine, Optional

logger = logging.getLogger(__name__)


def _storage_alias() -> str:
    explicit = os.environ.get("AGENTTEAMS_STORAGE_ALIAS")
    if explicit:
        return explicit
    prefix = os.environ.get("AGENTTEAMS_STORAGE_PREFIX") or ""
    if "/" in prefix:
        return prefix.split("/", 1)[0]
    return "agentteams"


_MC_ALIAS = _storage_alias()

_STARTUP_SYNC_FILES = (
    "openclaw.json",
    "AGENTS.md",
    "SOUL.md",
    "config/mcporter.json",
    "mcporter-servers.json",
)


def _mc(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    mc_bin = shutil.which("mc")
    if not mc_bin:
        raise RuntimeError("mc binary not found on PATH")
    cmd = [mc_bin, *args]
    logger.debug("mc cmd: %s", " ".join(cmd))
    result = subprocess.run(cmd, capture_output=True, text=True, check=check)
    return result


def _looks_like_missing_object_error(stderr: str | None) -> bool:
    text = stderr or ""
    return "Object does not exist" in text or "The specified key does not exist" in text


def _deep_merge(base: dict, override: dict) -> dict:
    result = dict(base)
    for key, val in override.items():
        if key in result and isinstance(result[key], dict) and isinstance(val, dict):
            result[key] = _deep_merge(result[key], val)
        else:
            result[key] = val
    return result


def _merge_openclaw_config(remote_text: str, local_text: str) -> str:
    """Merge remote openclaw.json into the local copy (local-first).

    models / gateway are replaced from remote when present; channels deep-merge
    with the local accessToken kept (worker re-login owns it locally).
    """
    remote = json.loads(remote_text)
    local = json.loads(local_text)
    merged: dict = dict(local)

    if remote.get("models") is not None:
        merged["models"] = remote["models"]
    if remote.get("gateway") is not None:
        merged["gateway"] = remote["gateway"]

    r_channels = remote.get("channels") or {}
    l_channels = local.get("channels") or {}
    if r_channels or l_channels:
        merged["channels"] = _deep_merge(dict(l_channels), dict(r_channels))
        l_token = l_channels.get("matrix", {}).get("accessToken")
        if l_token:
            merged.setdefault("channels", {}).setdefault("matrix", {})[
                "accessToken"
            ] = l_token

    return json.dumps(merged, indent=2)


class FileSync:
    """MinIO file sync using mc CLI."""

    def __init__(
        self,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        worker_name: str,
        secure: bool = False,
        local_dir: Optional[Path] = None,
    ) -> None:
        self.endpoint = endpoint.rstrip("/")
        self.access_key = access_key
        self.secret_key = secret_key
        self.bucket = bucket
        self.worker_name = worker_name
        self._secure = secure
        self.local_dir = (
            local_dir or Path("/root/agentteams-fs/agents") / worker_name
        )
        self.local_dir.mkdir(parents=True, exist_ok=True)
        self._prefix = f"agents/{worker_name}"
        self._alias_set = False
        self._cloud_mode = os.environ.get("AGENTTEAMS_RUNTIME") == "aliyun"

    # ------------------------------------------------------------------
    # mc alias management
    # ------------------------------------------------------------------

    def _refresh_cloud_credentials(self) -> None:
        result = subprocess.run(
            ["bash", "-c",
             "source /opt/agentteams/scripts/lib/oss-credentials.sh && "
             "ensure_mc_credentials && "
             f"echo $MC_HOST_{_MC_ALIAS}"],
            capture_output=True, text=True, check=True,
        )
        mc_host = result.stdout.strip()
        if mc_host:
            os.environ[f"MC_HOST_{_MC_ALIAS}"] = mc_host

    def _ensure_alias(self) -> None:
        if self._cloud_mode:
            self._refresh_cloud_credentials()
            self._alias_set = True
            return
        if self._alias_set:
            return
        if self.endpoint.startswith("http"):
            url = self.endpoint
        else:
            scheme = "https" if self._secure else "http"
            url = f"{scheme}://{self.endpoint}"
        _mc("alias", "set", _MC_ALIAS, url, self.access_key, self.secret_key)
        self._alias_set = True

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _object_path(self, key: str) -> str:
        return f"{_MC_ALIAS}/{self.bucket}/{key}"

    def _cat(self, key: str) -> Optional[str]:
        self._ensure_alias()
        try:
            result = _mc("cat", self._object_path(key), check=True)
            return result.stdout
        except subprocess.CalledProcessError:
            return None

    def _ls(self, prefix: str) -> list[str]:
        self._ensure_alias()
        try:
            result = _mc("ls", "--recursive", self._object_path(prefix), check=True)
            names = []
            for line in result.stdout.splitlines():
                parts = line.strip().split()
                if parts:
                    names.append(parts[-1])
            return names
        except Exception:
            return []

    def _pull_startup_files(self) -> list[str]:
        changed: list[str] = []
        for rel_path in _STARTUP_SYNC_FILES:
            content = self._cat(f"{self._prefix}/{rel_path}")
            if content is None:
                continue
            local_path = self.local_dir / rel_path
            local_path.parent.mkdir(parents=True, exist_ok=True)
            local_path.write_text(content)
            changed.append(rel_path)
        return changed

    def _get_team_id(self) -> Optional[str]:
        agents_path = self.local_dir / "AGENTS.md"
        if agents_path.exists():
            try:
                import re
                m = re.search(r"\*\*Team\*\*:\s*(\S+)", agents_path.read_text())
                if m:
                    return m.group(1)
            except Exception:
                pass
        return None

    def _get_shared_remote(self) -> str:
        team_id = self._get_team_id()
        if team_id:
            return f"{_MC_ALIAS}/{self.bucket}/teams/{team_id}/shared/"
        return f"{_MC_ALIAS}/{self.bucket}/shared/"

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def mirror_all(self) -> None:
        """Full mirror of agents/<name>/ at startup."""
        self._ensure_alias()
        remote = self._object_path(f"{self._prefix}/")
        local = str(self.local_dir) + "/"
        try:
            _mc("mirror", remote, local, "--overwrite",
                "--exclude", "credentials/**", check=True)
        except subprocess.CalledProcessError as exc:
            error_text = f"{exc.stderr or ''}\n{exc.stdout or ''}"
            if not _looks_like_missing_object_error(error_text):
                raise
            logger.info("mirror_all: prefix missing; pulling startup files")
            self._pull_startup_files()

        if not (self.local_dir / "openclaw.json").exists():
            raise RuntimeError(
                f"openclaw.json not found in MinIO for worker {self.worker_name}"
            )

        shared_remote = self._get_shared_remote()
        shared_local = str(self.local_dir / "shared") + "/"
        try:
            _mc("mirror", shared_remote, shared_local, "--overwrite", check=True)
        except subprocess.CalledProcessError as exc:
            logger.warning("mirror_all: shared/ mirror failed: %s", exc.stderr)

    def get_config(self) -> dict:
        text = self._cat(f"{self._prefix}/openclaw.json")
        if not text:
            raise RuntimeError(
                f"openclaw.json not found in MinIO for worker {self.worker_name}"
            )
        return json.loads(text)

    def get_matrix_password(self) -> Optional[str]:
        return self._cat(f"{self._prefix}/credentials/matrix/password")

    def list_skills(self) -> list[str]:
        entries = self._ls(f"{self._prefix}/skills/")
        names: list[str] = []
        for entry in entries:
            parts = entry.rstrip("/").split("/")
            if parts and parts[0] not in names:
                names.append(parts[0])
        return names

    def pull_all(self) -> list[str]:
        """Pull Manager-managed files. Returns changed relative paths."""
        changed: list[str] = []
        files: dict[str, list[str]] = {
            "openclaw.json": [f"{self._prefix}/openclaw.json"],
            "config/mcporter.json": [
                f"{self._prefix}/config/mcporter.json",
                f"{self._prefix}/mcporter-servers.json",
            ],
        }
        for name, keys in files.items():
            content = None
            for key in keys:
                content = self._cat(key)
                if content is not None:
                    break
            if content is None:
                continue
            local = self.local_dir / name
            existing = local.read_text() if local.exists() else None

            if name == "openclaw.json" and existing is not None:
                merged = _merge_openclaw_config(content, existing)
                if merged != existing:
                    local.write_text(merged)
                    changed.append(name)
            elif content != existing:
                local.parent.mkdir(parents=True, exist_ok=True)
                local.write_text(content)
                changed.append(name)

        minio_skills = self.list_skills()
        for skill_name in minio_skills:
            remote_prefix = f"{self._prefix}/skills/{skill_name}/"
            local_skill_dir = self.local_dir / "skills" / skill_name
            local_skill_dir.mkdir(parents=True, exist_ok=True)
            try:
                result = _mc(
                    "mirror",
                    self._object_path(remote_prefix),
                    str(local_skill_dir) + "/",
                    "--overwrite",
                    check=False,
                )
                if result.returncode == 0:
                    for sh in local_skill_dir.rglob("*.sh"):
                        sh.chmod(sh.stat().st_mode | 0o111)
                    changed.append(f"skills/{skill_name}/")
            except Exception as exc:
                logger.warning("Failed to mirror skill %s: %s", skill_name, exc)

        shared_remote = self._get_shared_remote()
        shared_local = self.local_dir / "shared"
        shared_local.mkdir(parents=True, exist_ok=True)
        try:
            result = _mc(
                "mirror", shared_remote, str(shared_local) + "/",
                "--overwrite", check=False,
            )
            if result.returncode == 0:
                changed.append("shared/")
        except Exception as exc:
            logger.warning("Failed to mirror shared/: %s", exc)

        return changed


async def sync_loop(
    sync: FileSync,
    interval: int,
    on_pull: Callable[[list[str]], Coroutine],
) -> None:
    """Background task: pull Manager-managed files every ``interval`` seconds."""
    while True:
        await asyncio.sleep(interval)
        try:
            changed = await asyncio.get_event_loop().run_in_executor(
                None, sync.pull_all
            )
            if changed:
                logger.info("FileSync: files changed: %s", changed)
                await on_pull(changed)
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("FileSync: sync error: %s", exc)


def push_local(sync: FileSync, since: float = 0) -> list[str]:
    """Push worker-managed local changes back to MinIO.

    Excludes Manager-owned files and derived/ephemeral dirs (.cli-harness
    runtime state, caches, matrix crypto store).
    """
    _EXCLUDE_FILES = {
        "openclaw.json",
        "mcporter-servers.json",
    }
    _EXCLUDE_PATHS = {
        "config/mcporter.json",
    }
    _EXCLUDE_DIRS = {
        ".agents",
        ".cache",
        ".npm",
        ".local",
        ".mc",
        ".cli-harness",
        "matrix-nio-store",
        "cache",
        "logs",
        "__pycache__",
        "shared",
    }
    _EXCLUDE_EXTENSIONS = {".lock", ".db-journal", ".db-wal", ".db-shm"}

    pushed: list[str] = []
    local_dir = sync.local_dir
    if not local_dir.exists():
        return pushed

    sync._ensure_alias()

    for path in local_dir.rglob("*"):
        if not path.is_file():
            continue
        try:
            if path.stat().st_mtime <= since:
                continue
        except OSError:
            continue
        rel = path.relative_to(local_dir)
        if len(rel.parts) == 1 and rel.name in _EXCLUDE_FILES:
            continue
        if rel.as_posix() in _EXCLUDE_PATHS:
            continue
        if any(p in _EXCLUDE_DIRS for p in rel.parts):
            continue
        if rel.suffix in _EXCLUDE_EXTENSIONS:
            continue

        key = f"{sync._prefix}/{rel.as_posix()}"
        try:
            remote = sync._cat(key)
            local_content = path.read_text(errors="replace")
            if remote == local_content:
                continue
            dest = sync._object_path(key)
            _mc("cp", str(path), dest, check=True)
            pushed.append(str(rel))
        except Exception as exc:
            logger.debug("push_local: failed for %s: %s", rel, exc)

    return pushed


async def push_loop(sync: FileSync, check_interval: int = 5) -> None:
    """Background task: push local changes to MinIO every ``check_interval`` seconds."""
    last_push_time: float = time.time()

    while True:
        await asyncio.sleep(check_interval)
        try:
            now = time.time()
            pushed = await asyncio.get_event_loop().run_in_executor(
                None, push_local, sync, last_push_time
            )
            last_push_time = now
            if pushed:
                logger.info("FileSync push: uploaded %s", pushed)
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("FileSync push error: %s", exc)
