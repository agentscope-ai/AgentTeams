"""MinIO file sync for copaw-worker.

All MinIO operations use the `mc` CLI (MinIO Client).

File Sync Design Principle:

  The party that writes a file is responsible for:
    1. Pushing it to MinIO immediately (Local -> Remote)
    2. Notifying the other side via Matrix @mention so they can pull on demand

  Manager-managed (Worker read-only, pull only):
    openclaw.json, mcporter-servers.json, skills/, shared/

  Worker-managed (Worker read-write, push to MinIO):
    AGENTS.md, SOUL.md, .copaw/sessions/, memory/, etc.

  Local -> Remote (push_loop): change-triggered push of Worker-managed content.
  Remote -> Local (sync_loop pull_all): on-demand via file-sync skill when Manager
    @mentions, plus fallback periodic pull of Manager-managed paths as safety net.
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

# mc alias name used for this worker session
_MC_ALIAS = "hiclaw"


def _mc(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    """Run an mc command and return the result."""
    mc_bin = shutil.which("mc")
    if not mc_bin:
        raise RuntimeError("mc binary not found on PATH. Please install mc first.")
    cmd = [mc_bin, *args]
    logger.info("mc cmd: %s", " ".join(cmd))
    result = subprocess.run(cmd, capture_output=True, text=True, check=check)
    logger.info("mc stdout (%d chars): %r", len(result.stdout), result.stdout[:200])
    if result.stderr:
        logger.info("mc stderr: %r", result.stderr[:200])
    return result


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
        self.local_dir = local_dir or Path.home() / ".copaw-worker" / worker_name
        self.local_dir.mkdir(parents=True, exist_ok=True)
        self._prefix = f"agents/{worker_name}"
        self._alias_set = False
        self._cloud_mode = bool(
            os.environ.get("ALIBABA_CLOUD_OIDC_TOKEN_FILE")
            and Path(os.environ.get("ALIBABA_CLOUD_OIDC_TOKEN_FILE", "")).is_file()
        )

    # ------------------------------------------------------------------
    # mc alias management
    # ------------------------------------------------------------------

    def _refresh_cloud_credentials(self) -> None:
        """Refresh STS credentials by calling the shared shell function.

        The shell function is lazy: it checks /tmp/mc-oss-credentials.env
        and only hits the STS endpoint when the token is within 10 minutes
        of expiring.  Cheap no-op when credentials are still valid.
        """
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
        else:
            logger.warning("ensure_mc_credentials returned empty MC_HOST_%s", _MC_ALIAS)

    def _ensure_alias(self) -> None:
        """Set up mc alias, refreshing STS credentials in cloud mode.

        Cloud mode (RRSA/STS): refresh credentials before every mc batch
        via the shared shell function (lazy, no-op when token is valid).
        Local mode: set mc alias once with static credentials.
        """
        if self._cloud_mode:
            self._refresh_cloud_credentials()
            self._alias_set = True
            return
        if self._alias_set:
            return
        # Local mode: static credentials, set alias once
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
        """Return full mc path: alias/bucket/key"""
        return f"{_MC_ALIAS}/{self.bucket}/{key}"

    def _cat(self, key: str) -> Optional[str]:
        """Download object content as text using mc cat."""
        self._ensure_alias()
        try:
            result = _mc("cat", self._object_path(key), check=True)
            return result.stdout
        except subprocess.CalledProcessError as exc:
            logger.debug("mc cat failed for %s: %s", key, exc.stderr)
            return None
        except Exception as exc:
            logger.debug("mc cat error for %s: %s", key, exc)
            return None

    def _ls(self, prefix: str) -> list[str]:
        """List objects under prefix, return list of relative names."""
        self._ensure_alias()
        try:
            result = _mc("ls", "--recursive", self._object_path(prefix), check=True)
            names = []
            for line in result.stdout.splitlines():
                # mc ls output: "2024-01-01 00:00:00   1234 filename"
                parts = line.strip().split()
                if parts:
                    names.append(parts[-1])
            return names
        except subprocess.CalledProcessError as exc:
            logger.debug("mc ls failed for %s: %s", prefix, exc.stderr)
            return []
        except Exception as exc:
            logger.debug("mc ls error for %s: %s", prefix, exc)
            return []

    def mirror_all(self) -> None:
        """Full mirror of the worker's MinIO prefix to local_dir.

        Called once at startup to restore all state (config, sessions, sync
        token, etc.) — mirrors the OpenClaw worker's ``mc mirror`` approach.
        After this, the running sync uses pull_all (Manager-managed only)
        and push_local (Worker-managed only).
        """
        self._ensure_alias()
        remote = self._object_path(f"{self._prefix}/")
        local = str(self.local_dir) + "/"
        try:
            _mc("mirror", remote, local, "--overwrite",
                 "--exclude", "credentials/**", check=True)
            logger.info("mirror_all: full mirror completed from %s", remote)
        except subprocess.CalledProcessError as exc:
            logger.warning("mirror_all: mc mirror failed: %s", exc.stderr)
            raise

        # Also mirror shared/ from bucket root
        shared_remote = f"{_MC_ALIAS}/{self.bucket}/shared/"
        shared_local = str(self.local_dir / "shared") + "/"
        try:
            _mc("mirror", shared_remote, shared_local, "--overwrite", check=True)
            logger.info("mirror_all: shared/ mirror completed")
        except subprocess.CalledProcessError as exc:
            logger.warning("mirror_all: shared/ mirror failed (non-fatal): %s", exc.stderr)


    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def get_config(self) -> dict[str, Any]:
        """Pull openclaw.json and return parsed dict."""
        text = self._cat(f"{self._prefix}/openclaw.json")
        if not text:
            raise RuntimeError(f"openclaw.json not found in MinIO for worker {self.worker_name}")
        logger.info("openclaw.json raw content (%d chars): %r", len(text), text[:500])
        return json.loads(text)

    def get_soul(self) -> Optional[str]:
        return self._cat(f"{self._prefix}/SOUL.md")

    def get_agents_md(self) -> Optional[str]:
        return self._cat(f"{self._prefix}/AGENTS.md")

    def list_skills(self) -> list[str]:
        """Return list of skill names available in MinIO for this worker."""
        prefix = f"{self._prefix}/skills/"
        entries = self._ls(prefix)
        # entries look like "skill-name/SKILL.md"
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

    def get_skill_md(self, skill_name: str) -> Optional[str]:
        """Pull SKILL.md for a given skill name."""
        return self._cat(f"{self._prefix}/skills/{skill_name}/SKILL.md")

    def pull_all(self) -> list[str]:
        """Pull Manager-managed files only (allowlist). Returns list of filenames that changed.

        Does NOT pull AGENTS.md, SOUL.md (Worker-managed, sync up but never overwrite).
        """
        changed: list[str] = []
        # Manager-managed files (allowlist)
        # Each entry: local_name -> list of remote keys (tried in order, first hit wins).
        # The fallback handles the migration period where MinIO may still have the
        # old path (mcporter-servers.json) before Manager re-runs setup-mcp-server.sh.
        files: dict[str, list[str]] = {
            "openclaw.json": [f"{self._prefix}/openclaw.json"],
            "config/mcporter.json": [
                f"{self._prefix}/config/mcporter.json",
                f"{self._prefix}/mcporter-servers.json",  # backward compat
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
            if content != existing:
                local.parent.mkdir(parents=True, exist_ok=True)
                local.write_text(content)
                changed.append(name)

        # Manager-managed: skills/
        # Use mc mirror to pull entire skill directories (including scripts/ and references/)
        # instead of only pulling SKILL.md, to match OpenClaw worker's mc mirror behavior.
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
                    # Restore +x on scripts (MinIO does not preserve Unix permission bits)
                    for sh in local_skill_dir.rglob("*.sh"):
                        sh.chmod(sh.stat().st_mode | 0o111)
                    changed.append(f"skills/{skill_name}/")
                else:
                    logger.warning("mc mirror failed for skill %s: %s", skill_name, result.stderr)
            except Exception as exc:
                logger.warning("Failed to mirror skill %s: %s", skill_name, exc)

        # Manager-managed: shared/
        # Mirror the shared directory from MinIO bucket root to local_dir/shared/
        # (shared/ lives at bucket root, not under agents/{worker_name}/)
        shared_remote = f"{_MC_ALIAS}/{self.bucket}/shared/"
        shared_local = self.local_dir / "shared"
        shared_local.mkdir(parents=True, exist_ok=True)
        try:
            result = _mc(
                "mirror",
                shared_remote,
                str(shared_local) + "/",
                "--overwrite",
                check=False,
            )
            if result.returncode == 0:
                changed.append("shared/")
            else:
                logger.warning("mc mirror failed for shared/: %s", result.stderr)
        except Exception as exc:
            logger.warning("Failed to mirror shared/: %s", exc)

        # Clean up local skill dirs removed from MinIO
        local_skills_dir = self.local_dir / "skills"
        if local_skills_dir.is_dir():
            minio_skill_set = set(minio_skills)
            for child in list(local_skills_dir.iterdir()):
                if child.is_dir() and child.name not in minio_skill_set:
                    shutil.rmtree(child)
                    changed.append(f"skills/{child.name}/ (removed)")
                    logger.info("Removed local skill no longer in MinIO: %s", child.name)

        return changed


async def sync_loop(
    sync: FileSync,
    interval: int,
    on_pull: Callable[[list[str]], Coroutine],
) -> None:
    """Background task: pull files every `interval` seconds."""
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
    """Push locally-changed files back to MinIO. Returns list of pushed keys.

    Mirrors the openclaw worker entrypoint behavior: only scans files whose
    mtime > `since` (epoch seconds), then content-compares before uploading.
    When since=0 (first run), scans all eligible files.

    Excludes Manager-managed files only. AGENTS.md, SOUL.md, .copaw/sessions/
    are Worker-managed and are pushed (including session backup).
    """
    # Manager-managed files that should never be pushed back
    _EXCLUDE_FILES = {
        "openclaw.json",
        "mcporter-servers.json",
    }
    # Manager-managed files at specific relative paths (not just root)
    _EXCLUDE_PATHS = {
        "config/mcporter.json",
    }
    # Directory name components to skip anywhere in the tree
    _EXCLUDE_DIRS = {
        ".agents",
        ".cache",
        ".npm",
        ".local",
        ".mc",
        # .copaw sub-dirs that are derived / installed at startup
        "custom_channels",
        "active_skills",
        "__pycache__",
        # Manager-managed shared directory (pulled from bucket root)
        "shared",
    }
    # File extensions to skip (transient runtime files)
    _EXCLUDE_EXTENSIONS = {".lock"}
    # Derived files inside .copaw/ that are generated by bridge.py or
    # pulled from MinIO — must not be pushed back.
    _COPAW_DERIVED_FILES = {
        "config.json",
        "providers.json",
        "SOUL.md",
        "AGENTS.md",
        "mcporter.json",
    }

    pushed: list[str] = []
    local_dir = sync.local_dir
    if not local_dir.exists():
        return pushed

    # ── Inner → Outer sync ──────────────────────────────────────────────
    # CoPaw Agent reads/writes .copaw/AGENTS.md and .copaw/SOUL.md at
    # runtime.  These are "inner" copies derived from the "outer" files at
    # the sync root.  If the Agent modifies them, propagate changes back to
    # the outer layer so the normal push cycle uploads them to MinIO.
    _INNER_OUTER_FILES = ("AGENTS.md", "SOUL.md")
    copaw_dir = local_dir / ".copaw"
    for name in _INNER_OUTER_FILES:
        inner = copaw_dir / name
        outer = local_dir / name
        if not inner.exists():
            continue
        try:
            inner_mtime = inner.stat().st_mtime
        except OSError:
            continue
        # Only copy if inner is newer than outer (or outer doesn't exist)
        outer_mtime = outer.stat().st_mtime if outer.exists() else 0
        if inner_mtime > outer_mtime:
            inner_content = inner.read_text(errors="replace")
            outer_content = outer.read_text(errors="replace") if outer.exists() else ""
            if inner_content != outer_content:
                outer.write_text(inner_content)
                logger.debug("Inner→Outer sync: .copaw/%s → %s", name, name)

    sync._ensure_alias()

    for path in local_dir.rglob("*"):
        if not path.is_file():
            continue
        # Quick mtime check — skip files not modified since last push
        try:
            if path.stat().st_mtime <= since:
                continue
        except OSError:
            continue
        rel = path.relative_to(local_dir)
        # Skip Manager-owned config files at workspace root
        if len(rel.parts) == 1 and rel.name in _EXCLUDE_FILES:
            continue
        # Skip Manager-owned config files at specific paths
        if rel.as_posix() in _EXCLUDE_PATHS:
            continue
        # Skip excluded directory trees
        if any(p in _EXCLUDE_DIRS for p in rel.parts):
            continue
        # Skip transient runtime files by extension (e.g. .lock)
        if rel.suffix in _EXCLUDE_EXTENSIONS:
            continue
        # Skip derived files inside .copaw/
        if rel.parts[0] == ".copaw" and rel.name in _COPAW_DERIVED_FILES:
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
            logger.debug("Pushed %s -> %s", rel, dest)
        except Exception as exc:
            logger.debug("push_local: failed for %s: %s", rel, exc)

    return pushed


async def push_loop(sync: FileSync, check_interval: int = 5) -> None:
    """Background task: push local changes to MinIO every `check_interval` seconds.

    Tracks last push timestamp and only triggers push_local when files with
    newer mtime are detected, similar to openclaw's find-newermt approach.
    """
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
