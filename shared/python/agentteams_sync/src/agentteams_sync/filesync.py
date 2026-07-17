"""MinIO file sync using the mc CLI."""

from __future__ import annotations

import json
import logging
import os
import re
import shutil
import subprocess
from pathlib import Path
from typing import Any, Literal, Optional

from agentteams_openclaw_merge import merge_openclaw_config

from agentteams_sync.helpers import STARTUP_SYNC_FILES, team_storage_name_from_worker_team
from agentteams_sync import mc as mc_ops
from agentteams_sync.mc import (
    looks_like_missing_object_error,
    looks_like_remote_directory_error,
    storage_alias,
)
from agentteams_sync.types import SharedPath

logger = logging.getLogger(__name__)

_MC_ALIAS = storage_alias()


class FileSync:
    """MinIO file sync using mc CLI."""

    def __init__(
        self,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        worker_name: str,
        worker_cr_name: Optional[str] = None,
        secure: bool = False,
        local_dir: Optional[Path] = None,
        shared_dir: Optional[Path] = None,
        global_shared_dir: Optional[Path] = None,
        *,
        team_resolver: Literal["hiclaw", "agents_md"] = "hiclaw",
        pull_includes_shared: bool = False,
        pull_includes_global_shared: bool = False,
        shared_remote_root: str | None = None,
        global_shared_remote_root: str | None = None,
    ) -> None:
        self.endpoint = endpoint.rstrip("/")
        self.access_key = access_key
        self.secret_key = secret_key
        self.bucket = bucket
        self.worker_name = worker_name
        self.worker_cr_name = worker_cr_name or worker_name
        self._secure = secure
        configured_working_dir = os.environ.get("COPAW_WORKING_DIR")
        if local_dir is not None:
            self.local_dir = local_dir
        elif configured_working_dir:
            self.local_dir = Path(configured_working_dir).parent
        else:
            self.local_dir = Path.home() / ".copaw-worker" / worker_name
        self.local_dir.mkdir(parents=True, exist_ok=True)
        self.shared_dir = shared_dir or self.local_dir / "shared"
        self.global_shared_dir = global_shared_dir or self.local_dir / "global-shared"
        self._team_resolver = team_resolver
        self._pull_includes_shared = pull_includes_shared
        self._pull_includes_global_shared = pull_includes_global_shared
        self._shared_remote_root = (
            shared_remote_root.rstrip("/") + "/"
            if shared_remote_root and shared_remote_root.strip()
            else None
        )
        self._global_shared_remote_root = (
            global_shared_remote_root.rstrip("/") + "/"
            if global_shared_remote_root and global_shared_remote_root.strip()
            else None
        )
        self._prefix = f"agents/{worker_name}"
        self._alias_set = False
        runtime = os.environ.get("AGENTTEAMS_RUNTIME")
        self._cloud_mode = runtime == "aliyun"
        self._k8s_mode = runtime == "k8s"
        self._worker_info: dict[str, Any] | None = None
        self._skipped_local_skills_logged: set[str] = set()
        self._push_content_cache: dict[str, tuple[float, str]] = {}

    def _refresh_cloud_credentials(self) -> None:
        result = subprocess.run(
            [
                "bash",
                "-c",
                "source /opt/hiclaw/scripts/lib/hiclaw-env.sh && "
                "ensure_mc_credentials && "
                f"_mc_host_var=MC_HOST_{_MC_ALIAS} && "
                'printf "%s" "${!_mc_host_var}"',
            ],
            capture_output=True,
            text=True,
            check=True,
        )
        mc_host = result.stdout.strip()
        if mc_host:
            os.environ[f"MC_HOST_{_MC_ALIAS}"] = mc_host
        else:
            logger.warning("ensure_mc_credentials returned empty MC_HOST_%s", _MC_ALIAS)

    def _ensure_alias(self) -> None:
        runtime = os.environ.get("AGENTTEAMS_RUNTIME", "<unset>")
        mc_host_set = bool(os.environ.get(f"MC_HOST_{_MC_ALIAS}"))
        controller_url = os.environ.get("AGENTTEAMS_CONTROLLER_URL", "<unset>")
        logger.info(
            "_ensure_alias: runtime=%s cloud_mode=%s k8s_mode=%s endpoint=%s bucket=%s worker_name=%s access_key=%s alias_set=%s mc_host_set=%s controller_url=%s",
            runtime,
            self._cloud_mode,
            self._k8s_mode,
            self.endpoint,
            self.bucket,
            self.worker_name,
            "<redacted>",
            self._alias_set,
            mc_host_set,
            controller_url,
        )
        if self._k8s_mode:
            logger.info("_ensure_alias: k8s mode, skipping mc alias set (mc-wrapper handles credentials)")
            self._alias_set = True
            return
        if self._cloud_mode:
            logger.info("_ensure_alias: credential path=sts, refreshing MC_HOST_%s", _MC_ALIAS)
            self._refresh_cloud_credentials()
            self._alias_set = True
            return
        if self._alias_set:
            logger.info("_ensure_alias: credential path=static, alias already set")
            return
        if self.endpoint.startswith("http"):
            url = self.endpoint
        else:
            scheme = "https" if self._secure else "http"
            url = f"{scheme}://{self.endpoint}"
        mc_ops.mc("alias", "set", _MC_ALIAS, url, self.access_key, self.secret_key)
        self._alias_set = True

    def _object_path(self, key: str) -> str:
        return f"{_MC_ALIAS}/{self.bucket}/{key}"

    def _cat(self, key: str) -> Optional[str]:
        self._ensure_alias()
        try:
            result = mc_ops.mc(
                "cat",
                self._object_path(key),
                check=False,
                log_output=False,
            )
        except Exception as exc:
            logger.debug("mc cat error for %s: %s", key, exc)
            return None
        if result.returncode == 0:
            return result.stdout
        if looks_like_missing_object_error(result.stderr):
            logger.info("mc cat missing object for %s: %s", key, result.stderr)
            return None
        logger.warning(
            "mc cat failed returncode=%s key=%s stderr=%r",
            result.returncode,
            key,
            result.stderr,
        )
        return None

    def _ls(self, prefix: str) -> list[str]:
        self._ensure_alias()
        try:
            result = mc_ops.mc("ls", "--recursive", self._object_path(prefix), check=True)
            names = []
            for line in result.stdout.splitlines():
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

    def _pull_startup_files(self) -> list[str]:
        changed: list[str] = []
        for rel_path in STARTUP_SYNC_FILES:
            content = self._cat(f"{self._prefix}/{rel_path}")
            if content is None:
                continue
            local_path = self.local_dir / rel_path
            local_path.parent.mkdir(parents=True, exist_ok=True)
            local_path.write_text(content)
            changed.append(rel_path)
        return changed

    def mirror_all(self) -> None:
        runtime = os.environ.get("AGENTTEAMS_RUNTIME", "<unset>")
        controller_url = os.environ.get("AGENTTEAMS_CONTROLLER_URL", "<unset>")
        logger.info(
            "mirror_all: preparing primary mirror runtime=%s cloud_mode=%s k8s_mode=%s endpoint=%s bucket=%s worker_name=%s alias_set=%s mc_host_set=%s controller_url=%s",
            runtime,
            self._cloud_mode,
            self._k8s_mode,
            self.endpoint,
            self.bucket,
            self.worker_name,
            self._alias_set,
            bool(os.environ.get(f"MC_HOST_{_MC_ALIAS}")),
            controller_url,
        )
        self._ensure_alias()
        remote = self._object_path(f"{self._prefix}/")
        local = str(self.local_dir) + "/"
        logger.info("mirror_all: primary mirror remote=%s local=%s", remote, local)
        try:
            mc_ops.mc("mirror", remote, local, "--overwrite", "--exclude", "credentials/**", check=True)
            logger.info("mirror_all: full mirror completed from %s", remote)
        except subprocess.CalledProcessError as exc:
            logger.warning(
                "mirror_all: primary mirror failed runtime=%s remote=%s local=%s stderr=%s",
                runtime,
                remote,
                local,
                exc.stderr,
            )
            error_text = f"{exc.stderr or ''}\n{exc.stdout or ''}"
            if not looks_like_missing_object_error(error_text):
                raise
            logger.info(
                "mirror_all: primary mirror prefix missing; trying direct startup file pulls",
            )
            startup_changed = self._pull_startup_files()
            if startup_changed:
                logger.info(
                    "mirror_all: restored startup files after missing prefix: %s",
                    ", ".join(startup_changed),
                )

        if not (self.local_dir / "openclaw.json").exists():
            raise RuntimeError(
                f"openclaw.json not found in MinIO for worker {self.worker_name}"
            )

        shared_remote = self._get_shared_remote()
        shared_local = str(self.shared_dir) + "/"
        self.shared_dir.mkdir(parents=True, exist_ok=True)
        logger.info("mirror_all: shared mirror remote=%s local=%s", shared_remote, shared_local)
        try:
            mc_ops.mc("mirror", shared_remote, shared_local, "--overwrite", check=True)
            logger.info("mirror_all: shared/ mirror completed from %s", shared_remote)
        except subprocess.CalledProcessError as exc:
            logger.warning(
                "mirror_all: shared/ mirror failed (non-fatal) remote=%s local=%s stderr=%s",
                shared_remote,
                shared_local,
                exc.stderr,
            )

        if self._is_team_leader():
            global_shared_remote = f"{_MC_ALIAS}/{self.bucket}/shared/"
            global_shared_local = str(self.global_shared_dir) + "/"
            self.global_shared_dir.mkdir(parents=True, exist_ok=True)
            logger.info(
                "mirror_all: global-shared mirror remote=%s local=%s",
                global_shared_remote,
                global_shared_local,
            )
            try:
                mc_ops.mc("mirror", global_shared_remote, global_shared_local, "--overwrite", check=True)
                logger.info("mirror_all: global-shared/ mirror completed")
            except subprocess.CalledProcessError as exc:
                logger.warning(
                    "mirror_all: global-shared/ mirror failed (non-fatal) remote=%s local=%s stderr=%s",
                    global_shared_remote,
                    global_shared_local,
                    exc.stderr,
                )

    def _get_worker_info(self) -> dict[str, Any]:
        if self._worker_info is not None:
            return self._worker_info

        hiclaw_bin = shutil.which("hiclaw")
        if not hiclaw_bin:
            raise RuntimeError("hiclaw CLI not found; cannot resolve worker storage scope")

        try:
            result = subprocess.run(
                [hiclaw_bin, "get", "workers", self.worker_cr_name, "-o", "json"],
                capture_output=True,
                text=True,
                check=True,
                timeout=10,
            )
            worker = json.loads(result.stdout)
        except Exception as exc:
            raise RuntimeError(
                f"failed to query worker metadata for {self.worker_cr_name}: {exc}",
            ) from exc

        if not isinstance(worker, dict):
            raise RuntimeError(f"invalid worker metadata for {self.worker_cr_name}")
        self._worker_info = worker
        return worker

    def _get_team_id_from_agents_md(self) -> Optional[str]:
        agents_path = self.local_dir / "AGENTS.md"
        if agents_path.exists():
            try:
                match = re.search(r"\*\*Team\*\*:\s*(\S+)", agents_path.read_text(encoding="utf-8"))
                if match:
                    return match.group(1)
            except OSError:
                pass
        config_path = self.local_dir / "openclaw.json"
        if config_path.exists():
            try:
                config = json.loads(config_path.read_text(encoding="utf-8"))
                team_id = config.get("team_id")
                if isinstance(team_id, str) and team_id.strip():
                    return team_id
            except (OSError, json.JSONDecodeError):
                pass
        return None

    def _is_team_leader_from_agents_md(self) -> bool:
        agents_path = self.local_dir / "AGENTS.md"
        if not agents_path.exists():
            return False
        try:
            return "Upstream coordinator" in agents_path.read_text(encoding="utf-8")
        except OSError:
            return False

    def _get_team_id(self) -> Optional[str]:
        if self._team_resolver == "agents_md":
            return self._get_team_id_from_agents_md()
        worker = self._get_worker_info()
        team_ref = worker.get("team")
        if not isinstance(team_ref, str) or not team_ref.strip():
            return None
        return team_storage_name_from_worker_team(self.bucket, team_ref)

    def _is_team_leader(self) -> bool:
        if self._team_resolver == "agents_md":
            return self._is_team_leader_from_agents_md()
        worker = self._get_worker_info()
        return worker.get("role") == "team_leader"

    def _get_shared_remote(self) -> str:
        if self._shared_remote_root is not None:
            return self._shared_remote_root
        team_id = self._get_team_id()
        if team_id:
            return f"{_MC_ALIAS}/{self.bucket}/teams/{team_id}/shared/"
        return f"{_MC_ALIAS}/{self.bucket}/shared/"

    def _get_global_shared_remote(self) -> str:
        if self._global_shared_remote_root is not None:
            return self._global_shared_remote_root
        return f"{_MC_ALIAS}/{self.bucket}/shared/"

    def resolve_shared_path(self, path: str) -> SharedPath:
        raw = (path or "").strip()
        if not raw:
            raise ValueError("path is required")
        if raw.startswith("/") or "\\" in raw:
            raise ValueError("path must be a relative shared path")

        normalized = raw.strip("/")
        parts = Path(normalized).parts
        if not parts or any(part in ("", ".", "..") for part in parts):
            raise ValueError("path must not contain empty, '.', or '..' segments")

        if parts[0] == "shared":
            subpath = "/".join(parts[1:])
            local = self.shared_dir.joinpath(*parts[1:]) if len(parts) > 1 else self.shared_dir
            remote = self._get_shared_remote()
            if subpath:
                remote = f"{remote}{subpath}"
                if raw.endswith("/"):
                    remote += "/"
            return SharedPath("shared", subpath, local, remote)

        if parts[0] == "global-shared":
            subpath = "/".join(parts[1:])
            local = (
                self.global_shared_dir.joinpath(*parts[1:])
                if len(parts) > 1
                else self.global_shared_dir
            )
            remote = self._get_global_shared_remote()
            if subpath:
                remote = f"{remote}{subpath}"
                if raw.endswith("/"):
                    remote += "/"
            return SharedPath("global-shared", subpath, local, remote)

        raise ValueError("path must start with shared/ or global-shared/")

    def pull_shared_path(self, path: str) -> SharedPath:
        resolved = self.resolve_shared_path(path)
        self._ensure_alias()
        if (path or "").strip().endswith("/"):
            resolved.local.mkdir(parents=True, exist_ok=True)
            mc_ops.mc("mirror", resolved.remote, str(resolved.local) + "/", "--overwrite", check=True)
            return resolved

        resolved.local.parent.mkdir(parents=True, exist_ok=True)
        try:
            mc_ops.mc("cp", resolved.remote, str(resolved.local), check=True)
        except subprocess.CalledProcessError as exc:
            if not looks_like_remote_directory_error(exc):
                raise
            remote = resolved.remote if resolved.remote.endswith("/") else f"{resolved.remote}/"
            resolved.local.mkdir(parents=True, exist_ok=True)
            mc_ops.mc("mirror", remote, str(resolved.local) + "/", "--overwrite", check=True)
        return resolved

    def push_shared_path(
        self,
        path: str,
        *,
        exclude: Optional[list[str]] = None,
    ) -> SharedPath:
        resolved = self.resolve_shared_path(path)
        if resolved.kind == "global-shared":
            raise ValueError("global-shared/ is read-only")
        if not resolved.local.exists():
            raise FileNotFoundError(f"local path does not exist: {resolved.local}")

        self._ensure_alias()
        if resolved.local.is_dir():
            remote = resolved.remote if resolved.remote.endswith("/") else f"{resolved.remote}/"
            args = ["mirror", str(resolved.local) + "/", remote, "--overwrite"]
            for item in exclude or []:
                args.extend(["--exclude", item])
            mc_ops.mc(*args, check=True)
        else:
            mc_ops.mc("cp", str(resolved.local), resolved.remote, check=True)
        return resolved

    def stat_shared_path(self, path: str) -> SharedPath:
        resolved = self.resolve_shared_path(path)
        self._ensure_alias()
        mc_ops.mc("stat", resolved.remote, check=True)
        return resolved

    def list_shared_path(self, path: str) -> tuple[SharedPath, list[str]]:
        resolved = self.resolve_shared_path(path)
        self._ensure_alias()
        result = mc_ops.mc("ls", "--recursive", resolved.remote, check=True)
        entries = [line.strip() for line in result.stdout.splitlines() if line.strip()]
        return resolved, entries

    def get_config(self) -> dict[str, Any]:
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
        """Pull controller-managed worker files, excluding shared data (CoPaw contract)."""
        changed: list[str] = []
        files: dict[str, list[str]] = {
            "openclaw.json": [f"{self._prefix}/openclaw.json"],
            "config/mcporter.json": [
                f"{self._prefix}/config/mcporter.json",
                f"{self._prefix}/mcporter-servers.json",
            ],
            "config/credagent.json": [f"{self._prefix}/config/credagent.json"],
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
            existing = local.read_text(encoding="utf-8") if local.exists() else None
            if name == "openclaw.json" and existing is not None:
                try:
                    content = merge_openclaw_config(content, existing)
                except json.JSONDecodeError as exc:
                    logger.warning("openclaw.json merge failed, replacing from remote: %s", exc)

            if content != existing:
                local.parent.mkdir(parents=True, exist_ok=True)
                local.write_text(content, encoding="utf-8")
                changed.append(name)

        minio_skills = self.list_skills()
        for skill_name in minio_skills:
            remote_prefix = f"{self._prefix}/skills/{skill_name}/"
            local_skill_dir = self.local_dir / "skills" / skill_name
            local_skill_dir.mkdir(parents=True, exist_ok=True)
            try:
                result = mc_ops.mc(
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
                else:
                    logger.warning("mc mirror failed for skill %s: %s", skill_name, result.stderr)
            except Exception as exc:
                logger.warning("Failed to mirror skill %s: %s", skill_name, exc)

        local_skills_dir = self.local_dir / "skills"
        if local_skills_dir.is_dir():
            minio_skill_set = set(minio_skills)
            for child in list(local_skills_dir.iterdir()):
                if child.is_dir() and child.name not in minio_skill_set:
                    if child.name not in self._skipped_local_skills_logged:
                        self._skipped_local_skills_logged.add(child.name)
                        logger.info(
                            "Skipping local skill not in MinIO (not pruned): %s",
                            child.name,
                        )

        if self._pull_includes_shared:
            shared_remote = self._get_shared_remote()
            self.shared_dir.mkdir(parents=True, exist_ok=True)
            try:
                result = mc_ops.mc(
                    "mirror",
                    shared_remote,
                    str(self.shared_dir) + "/",
                    "--overwrite",
                    check=False,
                )
                if result.returncode == 0:
                    changed.append("shared/")
                else:
                    logger.warning("mc mirror failed for shared/: %s", result.stderr)
            except Exception as exc:
                logger.warning("Failed to mirror shared/: %s", exc)

        if self._pull_includes_global_shared and self._is_team_leader():
            self.global_shared_dir.mkdir(parents=True, exist_ok=True)
            try:
                result = mc_ops.mc(
                    "mirror",
                    self._get_global_shared_remote(),
                    str(self.global_shared_dir) + "/",
                    "--overwrite",
                    check=False,
                )
                if result.returncode == 0:
                    changed.append("global-shared/")
            except Exception as exc:
                logger.warning("Failed to mirror global-shared/: %s", exc)

        return changed
