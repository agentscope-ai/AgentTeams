"""Object-storage restore and runtime-state persistence for qwenpaw-worker."""

from __future__ import annotations

import asyncio
import logging
import os
from pathlib import Path
import shutil
import subprocess
import time
from typing import List, Optional

logger = logging.getLogger(__name__)

DEFAULT_MC_ALIAS = "agentteams"
COMPARE_CONTENT_MAX_BYTES = 20 * 1024 * 1024


def _to_text(value: object) -> str:
    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")
    return str(value or "")


def _looks_like_missing_object_error(stderr: Optional[str] | bytes) -> bool:
    text = _to_text(stderr)
    return "Object does not exist" in text or "The specified key does not exist" in text


def _mc_error_message(exc: subprocess.CalledProcessError) -> str:
    text = _to_text(exc.stderr or exc.stdout).strip()
    return text or f"mc exited with status {exc.returncode}"


def _redact_url_userinfo(value: str) -> str:
    if "://" not in value or "@" not in value:
        return value
    scheme, rest = value.split("://", 1)
    return f"{scheme}://<redacted>@{rest.split('@', 1)[1]}"


def _preview_list(values: List[str], limit: int = 20) -> List[str]:
    if len(values) <= limit:
        return values
    return [*values[:limit], f"...({len(values) - limit} more)"]


def _storage_alias() -> str:
    value = os.getenv("AGENTTEAMS_STORAGE_ALIAS", "").strip().strip("/")
    if value:
        return value
    prefix = os.getenv("AGENTTEAMS_STORAGE_PREFIX", "").strip().strip("/")
    if "/" in prefix:
        return prefix.split("/", 1)[0]
    return DEFAULT_MC_ALIAS


class FileSync:
    def __init__(
        self,
        *,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        worker_name: str,
        local_dir: Path,
        shared_dir: Path,
        remote_prefix: Optional[str] = None,
        shared_prefix: Optional[str] = None,
    ) -> None:
        self.endpoint = endpoint
        self.access_key = access_key
        self.secret_key = secret_key
        self.bucket = bucket
        self.worker_name = worker_name
        self.local_dir = local_dir
        self.shared_dir = shared_dir
        self.remote_prefix = (remote_prefix or f"agents/{worker_name}").strip("/")
        self.shared_prefix = (shared_prefix or "shared").strip("/")
        self.mc_alias = _storage_alias()
        self._alias_set = False

    def _mc(self, *args: str, check: bool = True, text: bool = True) -> subprocess.CompletedProcess:
        mc_bin = shutil.which("mc")
        if not mc_bin:
            raise RuntimeError("mc binary not found")
        return subprocess.run(
            [mc_bin, *args],
            check=check,
            capture_output=True,
            text=text,
        )

    def ensure_alias(self) -> None:
        if self._alias_set:
            return
        if os.getenv(f"MC_HOST_{self.mc_alias}"):
            self._alias_set = True
            logger.info("storage alias ready component=sync worker=%s mode=env", self.worker_name)
            return
        if os.getenv("AGENTTEAMS_RUNTIME") == "k8s":
            self._alias_set = True
            logger.info("storage alias ready component=sync worker=%s mode=k8s-wrapper", self.worker_name)
            return
        missing = [
            name
            for name, value in (
                ("endpoint", self.endpoint),
                ("access_key", self.access_key),
                ("secret_key", self.secret_key),
            )
            if not value
        ]
        if missing:
            raise RuntimeError(f"missing storage config: {', '.join(missing)}")

        endpoint = self.endpoint.rstrip("/")
        if not endpoint.startswith(("http://", "https://")):
            endpoint = f"http://{endpoint}"
        logger.info(
            "configuring storage alias component=sync worker=%s endpoint=%s bucket=%s",
            self.worker_name,
            _redact_url_userinfo(endpoint),
            self.bucket,
        )
        try:
            self._mc("alias", "set", self.mc_alias, endpoint, self.access_key, self.secret_key)
        except subprocess.CalledProcessError as exc:
            raise RuntimeError(f"configure storage alias failed: {_mc_error_message(exc)}") from None
        self._alias_set = True
        logger.info("storage alias ready component=sync worker=%s mode=static", self.worker_name)

    def _object_path(self, key: str) -> str:
        return f"{self.mc_alias}/{self.bucket}/{key.strip('/')}"

    def _cat(self, key: str) -> Optional[str]:
        self.ensure_alias()
        result = self._mc("cat", self._object_path(key), check=False)
        if result.returncode == 0:
            return result.stdout
        if _looks_like_missing_object_error(result.stderr):
            return None
        logger.debug("mc cat failed component=sync key=%s returncode=%s", key, result.returncode)
        return None

    def _cat_bytes(self, key: str) -> Optional[bytes]:
        self.ensure_alias()
        try:
            result = self._mc("cat", self._object_path(key), check=False, text=False)
        except Exception as exc:
            logger.debug("mc cat failed component=sync key=%s error_type=%s", key, type(exc).__name__)
            return None
        if result.returncode == 0:
            stdout = result.stdout
            if isinstance(stdout, bytes):
                return stdout
            return _to_text(stdout).encode("utf-8")
        if _looks_like_missing_object_error(result.stderr):
            return None
        logger.debug("mc cat failed component=sync key=%s returncode=%s", key, result.returncode)
        return None

    def _mirror_prefix(self, remote_prefix: str, local_dir: Path) -> None:
        local_dir.mkdir(parents=True, exist_ok=True)
        remote = f"{self.mc_alias}/{self.bucket}/{remote_prefix.strip('/')}/"
        logger.info(
            "mirroring storage prefix component=sync worker=%s remote=%s local=%s",
            self.worker_name,
            remote,
            local_dir,
        )
        try:
            self._mc(
                "mirror",
                remote,
                str(local_dir) + "/",
                "--overwrite",
                "--exclude",
                "credentials/**",
            )
        except subprocess.CalledProcessError as exc:
            if _looks_like_missing_object_error(exc.stderr):
                logger.info("storage prefix is empty component=sync remote=%s", remote)
                return
            raise RuntimeError(f"mirror storage failed: {_mc_error_message(exc)}") from None
        logger.info(
            "mirrored storage prefix component=sync worker=%s remote=%s local=%s",
            self.worker_name,
            remote,
            local_dir,
        )

    def mirror_all(self) -> None:
        self.ensure_alias()
        self._mirror_prefix(self.remote_prefix, self.local_dir)
        self._mirror_prefix(self.shared_prefix, self.shared_dir)

    def mirror_prefix(self, remote_prefix: str, local_dir: Path) -> None:
        self.ensure_alias()
        self._mirror_prefix(remote_prefix, local_dir)

    def pull_runtime_config(self, local_path: Path, remote_key: Optional[str] = None) -> bool:
        self.ensure_alias()
        key = remote_key or f"{self.remote_prefix}/runtime/runtime.yaml"
        local_path.parent.mkdir(parents=True, exist_ok=True)
        result = self._mc("cp", self._object_path(key), str(local_path), check=False)
        if result.returncode == 0:
            return True
        if _looks_like_missing_object_error(result.stderr):
            logger.info("runtime config not found in storage component=sync key=%s", key)
            return False
        raise RuntimeError(f"pull runtime config failed: {_mc_error_message(result)}")


def _skip_background_push(rel: Path) -> bool:
    rel_path = rel.as_posix()
    excluded_prefixes = (
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
    )
    if any(rel_path == prefix or rel_path.startswith(f"{prefix}/") for prefix in excluded_prefixes):
        return True
    excluded_dirs = {".cache", ".local", ".mc", "__pycache__", "logs"}
    if any(part in excluded_dirs for part in rel.parts):
        return True
    excluded_files = {".DS_Store", "qwenpaw.log", "heartbeat.json", "token_usage.json"}
    if rel.name in excluded_files:
        return True
    return rel.suffix in {".lock", ".pyc"}


def push_local(sync: FileSync, since: float = 0) -> List[str]:
    pushed = []
    local_dir = sync.local_dir
    if not local_dir.exists():
        return pushed

    sync.ensure_alias()
    for path in local_dir.rglob("*"):
        if not path.is_file():
            continue
        try:
            stat = path.stat()
            if stat.st_mtime <= since:
                continue
        except OSError:
            continue

        rel = path.relative_to(local_dir)
        if _skip_background_push(rel):
            continue

        key = f"{sync.remote_prefix}/{rel.as_posix()}"
        try:
            if stat.st_size <= COMPARE_CONTENT_MAX_BYTES:
                remote = sync._cat_bytes(key)
                if remote == path.read_bytes():
                    continue
            sync._mc("cp", str(path), sync._object_path(key), check=True)
            pushed.append(rel.as_posix())
        except Exception as exc:
            logger.debug("push_local failed component=sync file=%s error_type=%s", rel, type(exc).__name__)

    return pushed


async def push_loop(sync: FileSync, check_interval: float = 5) -> None:
    last_push_time = 0.0
    logger.info(
        "qwenpaw FileSync push loop started component=sync worker=%s interval_seconds=%s",
        sync.worker_name,
        check_interval,
    )
    while True:
        try:
            await asyncio.sleep(check_interval)
            now = time.time()
            pushed = await asyncio.to_thread(push_local, sync, last_push_time)
            last_push_time = now
            if pushed:
                logger.info(
                    "qwenpaw FileSync push uploaded component=sync worker=%s count=%d files=%s",
                    sync.worker_name,
                    len(pushed),
                    _preview_list(pushed),
                )
        except asyncio.CancelledError:
            logger.info("qwenpaw FileSync push loop stopped component=sync worker=%s", sync.worker_name)
            break
        except Exception as exc:
            logger.warning(
                "qwenpaw FileSync push failed component=sync worker=%s error_type=%s",
                sync.worker_name,
                type(exc).__name__,
            )
