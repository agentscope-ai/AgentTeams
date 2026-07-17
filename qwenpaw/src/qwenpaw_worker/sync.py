"""Object-storage restore and runtime-state persistence for qwenpaw-worker.

Re-exports ``agentteams_sync`` push policy with QwenPaw-specific mirror/pull hooks.
Import paths remain ``qwenpaw_worker.sync`` for worker startup and tests.
"""

from __future__ import annotations

import logging
import os
from pathlib import Path
import shutil
import subprocess
from typing import List, Optional

from agentteams_sync.contract import QWENPAW
from agentteams_sync.loops import push_loop as _push_loop
from agentteams_sync.mc import (
    bind_active_mc,
    looks_like_missing_object_error,
    preview_text,
    redacted_mc_command,
)
from agentteams_sync.policy import PushPolicy
from agentteams_sync.push import push_local as _push_local_impl

logger = logging.getLogger(__name__)

DEFAULT_MC_ALIAS = "agentteams"
COMPARE_CONTENT_MAX_BYTES = 20 * 1024 * 1024
_QWENPAW_PUSH_POLICY = PushPolicy.qwenpaw()


def _to_text(value: object) -> str:
    if isinstance(value, bytes):
        return value.decode("utf-8", errors="replace")
    return str(value or "")


def _mc_error_message(exc: subprocess.CalledProcessError) -> str:
    text = _to_text(exc.stderr or exc.stdout).strip()
    return text or f"mc exited with status {exc.returncode}"


def _redact_url_userinfo(value: str) -> str:
    if "://" not in value or "@" not in value:
        return value
    scheme, rest = value.split("://", 1)
    return f"{scheme}://<redacted>@{rest.split('@', 1)[1]}"


def _storage_alias() -> str:
    value = os.getenv("AGENTTEAMS_STORAGE_ALIAS", "").strip().strip("/")
    if value:
        return value
    prefix = os.getenv("AGENTTEAMS_STORAGE_PREFIX", "").strip().strip("/")
    if "/" in prefix:
        return prefix.split("/", 1)[0]
    return DEFAULT_MC_ALIAS


def _mc(
    *args: str,
    check: bool = True,
    text: bool = True,
) -> subprocess.CompletedProcess:
    """Run mc using this module's ``shutil`` / ``subprocess`` (test patch target)."""
    mc_bin = shutil.which("mc")
    if not mc_bin:
        raise RuntimeError("mc binary not found")
    cmd = [mc_bin, *args]
    redacted_cmd = redacted_mc_command(cmd)
    logger.info("mc cmd: %s", " ".join(redacted_cmd))
    try:
        result = subprocess.run(cmd, check=check, capture_output=True, text=text)
    except subprocess.CalledProcessError as exc:
        exc.cmd = redacted_cmd
        logger.warning(
            "mc command failed returncode=%s cmd=%s stdout=%r stderr=%r",
            exc.returncode,
            " ".join(redacted_cmd),
            preview_text(exc.stdout if isinstance(exc.stdout, str) else _to_text(exc.stdout)),
            preview_text(exc.stderr if isinstance(exc.stderr, str) else _to_text(exc.stderr)),
        )
        raise
    return result


def _mc_dispatch(
    *args: str,
    check: bool = True,
    warn_on_error: bool = True,
    log_output: bool = True,
) -> subprocess.CompletedProcess:
    import qwenpaw_worker.sync as sync_mod

    return sync_mod._mc(*args, check=check)


bind_active_mc(_mc_dispatch)


class FileSync:
    """QwenPaw MinIO sync — shared push policy with QwenPaw mirror/pull contract."""

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
        self._prefix = self.remote_prefix
        self.mc_alias = _storage_alias()
        self._alias_set = False

    def _mc(self, *args: str, check: bool = True, text: bool = True) -> subprocess.CompletedProcess:
        return _mc(*args, check=check, text=text)

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

    def _ensure_alias(self) -> None:
        self.ensure_alias()

    def _object_path(self, key: str) -> str:
        return f"{self.mc_alias}/{self.bucket}/{key.strip('/')}"

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
        if looks_like_missing_object_error(_to_text(result.stderr)):
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
            if looks_like_missing_object_error(_to_text(exc.stderr)):
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
        if looks_like_missing_object_error(_to_text(result.stderr)):
            logger.info("runtime config not found in storage component=sync key=%s", key)
            return False
        raise RuntimeError(f"pull runtime config failed: {_mc_error_message(result)}")


def push_local(sync: FileSync, since: float = 0) -> List[str]:
    """Push local changes using QwenPaw ``PushPolicy`` and byte-accurate compare."""
    return _push_local_impl(
        sync,
        since,
        policy=_QWENPAW_PUSH_POLICY,
        compare_bytes=True,
        max_compare_bytes=COMPARE_CONTENT_MAX_BYTES,
    )


async def push_loop(
    sync: FileSync,
    check_interval: float = QWENPAW.push_check_interval_seconds,
) -> None:
    """Background push loop using QwenPaw ``push_local``."""
    await _push_loop(sync, check_interval=int(check_interval), push_fn=push_local)


__all__ = [
    "COMPARE_CONTENT_MAX_BYTES",
    "FileSync",
    "PushPolicy",
    "push_local",
    "push_loop",
    "_mc",
]
