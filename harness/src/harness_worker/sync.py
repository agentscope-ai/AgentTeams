"""MinIO file sync for harness-worker.

All MinIO operations use the ``mc`` CLI (MinIO Client).

File Sync Design Principle (mirrors hermes_worker):

  The party that writes a file is responsible for:
    1. Pushing it to MinIO immediately (Local -> Remote)
    2. Notifying the other side via Matrix @mention so they can pull on demand

  Manager-managed (Worker read-only, pull only):
    openclaw.json, mcporter-servers.json, skills/, shared/

  Worker-managed (Worker read-write, push to MinIO):
    AGENTS.md, SOUL.md, .harness/sessions/, memory/, etc.

  Local -> Remote (push_loop): change-triggered push of Worker-managed content.
  Remote -> Local (sync_loop pull_all): on-demand via file-sync skill when
    Manager @mentions, plus fallback periodic pull of Manager-managed paths.
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
from typing import Any, Callable, Coroutine, Optional

logger = logging.getLogger(__name__)

_MC_ALIAS = "hiclaw"


def _deep_merge(base: dict, override: dict) -> dict:
    result = dict(base)
    for key, val in override.items():
        if key in result and isinstance(result[key], dict) and isinstance(val, dict):
            result[key] = _deep_merge(result[key], val)
        else:
            result[key] = val
    return result


def _merge_openclaw_config(remote_text: str, local_text: str) -> str:
    remote = json.loads(remote_text)
    local = json.loads(local_text)
    merged: dict[str, Any] = dict(local)

    if remote.get("models") is not None:
        merged["models"] = remote["models"]
    if remote.get("gateway") is not None:
        merged["gateway"] = remote["gateway"]

    r_channels = remote.get("channels") or {}
    l_channels = local.get("channels") or {}
    if r_channels or l_channels:
        merged["channels"] = _deep_merge(dict(l_channels), dict(r_channels))
        l_token = local.get("channels", {}).get("matrix", {}).get("accessToken")
        if l_token:
            merged.setdefault("channels", {}).setdefault("matrix", {})["accessToken"] = l_token

    r_plugins = remote.get("plugins")
    l_plugins = local.get("plugins")
    if r_plugins or l_plugins:
        r_plugins = dict(r_plugins or {})
        l_plugins = dict(l_plugins or {})
        out_plugins: dict[str, Any] = dict(l_plugins)
        r_entries = r_plugins.get("entries") or {}
        l_entries = l_plugins.get("entries") or {}
        if r_entries or l_entries:
            out_plugins["entries"] = _deep_merge(dict(r_entries), dict(l_entries))
        r_paths = r_plugins.get("load", {}).get("paths")
        l_paths = l_plugins.get("load", {}).get("paths")
        if r_paths is not None or l_paths is not None:
            out_load = dict(l_plugins.get("load") or {})
            out_load["paths"] = sorted(set((r_paths or []) + (l_paths or [])))
            out_plugins["load"] = out_load
        merged["plugins"] = out_plugins

    return json.dumps(merged, indent=2)


def _mc(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    mc_bin = shutil.which("mc")
    if not mc_bin:
        raise RuntimeError("mc binary not found on PATH")
    cmd = [mc_bin, *args]
    result = subprocess.run(cmd, capture_output=True, text=True, check=check)
    return result


class FileSync:
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
        self.local_dir = local_dir or Path("/root/hiclaw-fs/agents") / worker_name
        self.local_dir.mkdir(parents=True, exist_ok=True)
        self._prefix = f"agents/{worker_name}"
        self._alias_set = False
        self._cloud_mode = os.environ.get("HICLAW_RUNTIME") == "aliyun"

    def _refresh_cloud_credentials(self) -> None:
        result = subprocess.run(
            ["bash", "-c",
             "source /opt/hiclaw/scripts/lib/oss-credentials.sh && "
             "ensure_mc_credentials && "
             "echo $MC_HOST_hiclaw"],
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
        except subprocess.CalledProcessError:
            return []

    def mirror_all(self) -> None:
        self._ensure_alias()
        remote = self._object_path(f"{self._prefix}/")
        local = str(self.local_dir) + "/"
        _mc("mirror", remote, local, "--overwrite", "--exclude", "credentials/**", check=True)

        shared_remote = self._get_shared_remote()
        shared_local = str(self.local_dir / "shared") + "/"
        try:
            _mc("mirror", shared_remote, shared_local, "--overwrite", check=True)
        except subprocess.CalledProcessError as exc:
            logger.warning("mirror_all shared/ failed (non-fatal): %s", exc.stderr)

    def _get_team_id(self) -> Optional[str]:
        agents_path = self.local_dir / "AGENTS.md"
        if agents_path.exists():
            try:
                content = agents_path.read_text()
                import re
                m = re.search(r'\*\*Team\*\*:\s*(\S+)', content)
                if m:
                    return m.group(1)
            except Exception:
                pass
        config_path = self.local_dir / "openclaw.json"
        if config_path.exists():
            try:
                config = json.loads(config_path.read_text())
                return config.get("team_id") or None
            except Exception:
                pass
        return None

    def _is_team_leader(self) -> bool:
        agents_path = self.local_dir / "AGENTS.md"
        if agents_path.exists():
            try:
                content = agents_path.read_text()
                return "Upstream coordinator" in content
            except Exception:
                pass
        return False

    def _get_shared_remote(self) -> str:
        team_id = self._get_team_id()
        if team_id:
            return f"{_MC_ALIAS}/{self.bucket}/teams/{team_id}/shared/"
        return f"{_MC_ALIAS}/{self.bucket}/shared/"

    def get_config(self) -> dict[str, Any]:
        text = self._cat(f"{self._prefix}/openclaw.json")
        if not text:
            raise RuntimeError(f"openclaw.json not found for worker {self.worker_name}")
        return json.loads(text)

    def get_soul(self) -> Optional[str]:
        return self._cat(f"{self._prefix}/SOUL.md")

    def get_agents_md(self) -> Optional[str]:
        return self._cat(f"{self._prefix}/AGENTS.md")

    def list_skills(self) -> list[str]:
        prefix = f"{self._prefix}/skills/"
        entries = self._ls(prefix)
        skill_names: list[str] = []
        seen: set[str] = set()
        for entry in entries:
            parts = entry.rstrip("/").split("/")
            if parts:
                name = parts[0]
                if name and name not in seen:
                    seen.add(name)
                    skill_names.append(name)
        return skill_names

    def pull_all(self) -> list[str]:
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
                    local.parent.mkdir(parents=True, exist_ok=True)
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
                    "mirror", self._object_path(remote_prefix), str(local_skill_dir) + "/",
                    "--overwrite", check=False,
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
            result = _mc("mirror", shared_remote, str(shared_local) + "/", "--overwrite", check=False)
            if result.returncode == 0:
                changed.append("shared/")
        except Exception as exc:
            logger.warning("Failed to mirror shared/: %s", exc)

        local_skills_dir = self.local_dir / "skills"
        if local_skills_dir.is_dir():
            minio_skill_set = set(minio_skills)
            for child in list(local_skills_dir.iterdir()):
                if child.is_dir() and child.name not in minio_skill_set:
                    shutil.rmtree(child)
                    changed.append(f"skills/{child.name}/ (removed)")

        return changed


async def sync_loop(
    sync: FileSync,
    interval: int,
    on_pull: Callable[[list[str]], Coroutine],
) -> None:
    while True:
        await asyncio.sleep(interval)
        try:
            changed = await asyncio.get_event_loop().run_in_executor(None, sync.pull_all)
            if changed:
                logger.info("FileSync: files changed: %s", changed)
                await on_pull(changed)
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("FileSync: sync error: %s", exc)


def push_local(sync: FileSync, since: float = 0) -> list[str]:
    _EXCLUDE_FILES = {"openclaw.json", "mcporter-servers.json"}
    _EXCLUDE_PATHS = {"config/mcporter.json"}
    _EXCLUDE_DIRS = {
        ".agents", ".cache", ".npm", ".local", ".mc",
        ".harness", "platforms", "matrix-nio-store",
        "image_cache", "audio_cache", "document_cache", "cache", "logs", "__pycache__",
        "shared",
    }
    _EXCLUDE_EXTENSIONS = {".lock", ".db-journal", ".db-wal", ".db-shm"}
    _HARNESS_DERIVED_FILES = {"config.json", ".env"}

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
        if len(rel.parts) == 2 and rel.parts[0] == ".harness" and rel.name in _HARNESS_DERIVED_FILES:
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
    last_push_time: float = time.time()
    while True:
        await asyncio.sleep(check_interval)
        try:
            now = time.time()
            pushed = await asyncio.get_event_loop().run_in_executor(None, push_local, sync, last_push_time)
            last_push_time = now
            if pushed:
                logger.info("FileSync push: uploaded %s", pushed)
        except asyncio.CancelledError:
            break
        except Exception as exc:
            logger.warning("FileSync push error: %s", exc)