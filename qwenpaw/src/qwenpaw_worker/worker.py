"""QwenPaw Worker main entry point."""

from __future__ import annotations

import asyncio
import filecmp
import hashlib
import json
import logging
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import tempfile
import zipfile
from typing import Optional

from qwenpaw_worker.api import QwenPawApiClient
from qwenpaw_worker.config import WorkerConfig, _relative_storage_prefix
from qwenpaw_worker.heartbeat import WorkerHeartbeat, run_worker_heartbeat_loop
from qwenpaw_worker.log import configure_worker_logging
from qwenpaw_worker.sync import FileSync, push_loop
from qwenpaw_worker.update import MemberRuntimeConfig, RuntimeUpdater

logger = logging.getLogger(__name__)

DEFAULT_AGENT_ID = "default"
DEFAULT_BUILTIN_QWENPAW_PLUGINS_DIR = Path("/opt/agentteams/qwenpaw-builtin/plugins")
BUILTIN_QWENPAW_PLUGIN_MARKER = ".agentteams-builtin-plugin.sha256"
COPAW_MIGRATION_MARKER = ".copaw-migrated"
SESSION_FILE_PROMPT_POLICY = """Do not read, list, grep, glob, summarize, copy, or expose files under sessions/.
Session files are runtime-private state and may contain private conversation history.
This rule applies to all channels, users, and sessions, not only DingTalk."""
SESSION_FILE_PROMPT_POLICY_MARKER = "Session files are runtime-private state"


def _duration_ms(started_at: float) -> int:
    return max(0, int((time.monotonic() - started_at) * 1000))


def _log_fields(**fields: object) -> str:
    parts = []
    for key, value in fields.items():
        if value is None:
            continue
        parts.append(f"{key}={value}")
    return " ".join(parts)


def _redact_url_userinfo(value: str) -> str:
    if "://" not in value or "@" not in value:
        return value
    scheme, rest = value.split("://", 1)
    return f"{scheme}://<redacted>@{rest.split('@', 1)[1]}"


def _safe_error_code(exc: Exception) -> str:
    message = str(exc).lower()
    if "access denied" in message:
        return "storage_access_denied"
    if "agent package" in message:
        return "agent_package_failed"
    return type(exc).__name__


class Worker:
    def __init__(self, config: WorkerConfig) -> None:
        self.config = config
        self.sync: Optional[FileSync] = None
        self.heartbeat = WorkerHeartbeat(config.qwenpaw_working_dir / "heartbeat.json")
        self.api_client = QwenPawApiClient(
            f"http://127.0.0.1:{config.console_port}",
        )
        self.updater = RuntimeUpdater(
            config=config,
            adapter_apply=self._apply_runtime_adapter,
            adapter_force_apply=self._force_apply_runtime_adapter,
            api_client=self.api_client,
            runtime_reconcile=self._reconcile_runtime_storage,
        )
        self._process: Optional[asyncio.subprocess.Process] = None
        self._heartbeat_probe_task: Optional[asyncio.Task] = None
        self._push_task: Optional[asyncio.Task] = None
        self._update_task: Optional[asyncio.Task] = None
        self._stopping = False
        self._workspace_shared_dir: Optional[Path] = None
        self._initial_runtime_config: Optional[MemberRuntimeConfig] = None
        self._builtin_mcp_payload_identities: dict[str, str] = {}

    async def run(self) -> None:
        if not await self.start():
            return
        try:
            await self._run_qwenpaw()
        finally:
            await self.stop()

    async def start(self) -> bool:
        self._stopping = False
        logger.info(
            "qwenpaw worker startup begin component=worker worker=%s cr_name=%s install_dir=%s storage_endpoint=%s bucket=%s "
            "storage_prefix=%s shared_prefix=%s console_port=%s",
            self.config.worker_name,
            self.config.worker_cr_name,
            self.config.install_dir,
            _redact_url_userinfo(self.config.fs_endpoint),
            self.config.fs_bucket,
            self.config.storage_prefix,
            self.config.shared_prefix,
            self.config.console_port,
        )
        self._prepare_env()

        self.sync = FileSync(
            endpoint=self.config.fs_endpoint,
            access_key=self.config.fs_access_key,
            secret_key=self.config.fs_secret_key,
            bucket=self.config.fs_bucket,
            worker_name=self.config.worker_name,
            local_dir=self.config.worker_home,
            shared_dir=self.config.shared_dir,
            remote_prefix=self.config.storage_prefix,
            shared_prefix=self.config.shared_prefix,
        )

        try:
            stage_started = self._log_worker_stage_begin("mirror_all")
            self.sync.mirror_all()
        except Exception as exc:
            self._log_worker_stage_failed("mirror_all", stage_started, exc)
            self.heartbeat.update(
                "not_ready",
                f"startup mirror failed: {exc}",
                {"operation": "mirror_all", "error_type": type(exc).__name__},
            )
            return False
        self._log_worker_stage_complete("mirror_all", stage_started)

        try:
            stage_started = self._log_worker_stage_begin("migrate_copaw_state")
            migrated = self._migrate_legacy_state()
        except Exception as exc:
            self._log_worker_stage_failed("migrate_copaw_state", stage_started, exc)
            return False
        self._log_worker_stage_complete("migrate_copaw_state", stage_started, migrated=migrated)

        # These operations create .qwenpaw and therefore must happen only
        # after storage restore and CoPaw state migration.
        configure_worker_logging(self.config.qwenpaw_working_dir)
        self.config.default_workspace_dir.mkdir(parents=True, exist_ok=True)
        self.heartbeat.persist()
        self.updater.runtime_config_pull = lambda: self.sync.pull_runtime_config(self.config.runtime_config_path)
        self.updater.skill_sync = self._sync_managed_skills

        try:
            stage_started = self._log_worker_stage_begin("load_runtime_config", path=self.config.runtime_config_path)
            runtime_config = self.updater.load()
        except Exception as exc:
            self._log_worker_stage_failed("load_runtime_config", stage_started, exc)
            self.heartbeat.update("not_ready", str(exc))
            return False
        self._log_worker_stage_complete(
            "load_runtime_config",
            stage_started,
            generation=runtime_config.generation,
            team=runtime_config.team_name,
            member=runtime_config.member_name,
            role=runtime_config.member_role,
        )

        self._apply_runtime_identity(runtime_config)
        self._apply_runtime_storage(runtime_config)
        self._initial_runtime_config = runtime_config

        try:
            stage_started = self._log_worker_stage_begin("prepare_qwenpaw_runtime")
            self._link_workspace_shared()
        except Exception as exc:
            self._log_worker_stage_failed("prepare_qwenpaw_runtime", stage_started, exc)
            self.heartbeat.update("not_ready", str(exc))
            return False
        self._log_worker_stage_complete("prepare_qwenpaw_runtime", stage_started)

        try:
            stage_started = self._log_worker_stage_begin("prepare_default_plugins")
            self._prepare_default_plugins()
        except Exception as exc:
            self._log_worker_stage_failed("prepare_default_plugins", stage_started, exc)
            self.heartbeat.update("not_ready", str(exc))
            return False
        self._log_worker_stage_complete("prepare_default_plugins", stage_started)

        stage_started = self._log_worker_stage_begin("start_push_loop", interval_seconds=5)
        self._push_task = asyncio.create_task(
            push_loop(self.sync, check_interval=5),
            name=f"qwenpaw-worker-{self.config.worker_name}-push-loop",
        )
        self._log_worker_stage_complete("start_push_loop", stage_started, interval_seconds=5)
        logger.info(
            "qwenpaw worker preparation complete component=worker worker=%s",
            self.config.worker_name,
        )
        return True

    async def stop(self) -> None:
        self._stopping = True
        logger.info(
            "qwenpaw worker stop requested component=worker worker=%s has_process=%s has_push_task=%s has_update_task=%s "
            "has_heartbeat_task=%s",
            self.config.worker_name,
            self._process is not None,
            self._push_task is not None,
            self._update_task is not None,
            self._heartbeat_probe_task is not None,
        )
        for attr in ("_update_task", "_push_task", "_heartbeat_probe_task"):
            task = getattr(self, attr)
            if task is not None:
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass
                except Exception as exc:
                    logger.warning(
                        "background task %s failed during stop component=worker worker=%s error_type=%s",
                        attr,
                        self.config.worker_name,
                        type(exc).__name__,
                    )
                setattr(self, attr, None)
                logger.info("background task stopped component=worker worker=%s task=%s", self.config.worker_name, attr)

        if self._process is not None and self._process.returncode is None:
            self._process.terminate()
            try:
                await asyncio.wait_for(self._process.wait(), timeout=10)
                logger.info("qwenpaw app terminated component=worker worker=%s", self.config.worker_name)
            except asyncio.TimeoutError:
                self._process.kill()
                await self._process.wait()
                logger.warning("qwenpaw app killed after stop timeout component=worker worker=%s", self.config.worker_name)
        self._process = None
        logger.info("qwenpaw worker stopped component=worker worker=%s", self.config.worker_name)

    def _log_worker_stage_begin(self, stage: str, **fields: object) -> float:
        started_at = time.monotonic()
        logger.info(
            "startup component=worker stage=%s event=begin worker=%s %s",
            stage,
            self.config.worker_name,
            _log_fields(**fields),
        )
        return started_at

    def _log_worker_stage_complete(self, stage: str, started_at: float, **fields: object) -> None:
        logger.info(
            "startup component=worker stage=%s event=complete worker=%s duration_ms=%s %s",
            stage,
            self.config.worker_name,
            _duration_ms(started_at),
            _log_fields(**fields),
        )

    def _log_worker_stage_failed(self, stage: str, started_at: float, exc: Exception, **fields: object) -> None:
        logger.warning(
            "startup component=worker stage=%s event=failed worker=%s duration_ms=%s error_type=%s error_code=%s %s",
            stage,
            self.config.worker_name,
            _duration_ms(started_at),
            type(exc).__name__,
            _safe_error_code(exc),
            _log_fields(**fields),
        )

    def _log_plugin_step_begin(self, plugin_name: str, step: str, **fields: object) -> float:
        started_at = time.monotonic()
        logger.info(
            "component=plugin plugin=%s step=%s event=begin worker=%s %s",
            plugin_name,
            step,
            self.config.worker_name,
            _log_fields(**fields),
        )
        return started_at

    def _log_plugin_step_complete(self, plugin_name: str, step: str, started_at: float, **fields: object) -> None:
        logger.info(
            "component=plugin plugin=%s step=%s event=complete worker=%s duration_ms=%s %s",
            plugin_name,
            step,
            self.config.worker_name,
            _duration_ms(started_at),
            _log_fields(**fields),
        )

    def _log_plugin_step_failed(self, plugin_name: str, step: str, started_at: float, exc: Exception, **fields: object) -> None:
        logger.warning(
            "component=plugin plugin=%s step=%s event=failed worker=%s duration_ms=%s error_type=%s %s",
            plugin_name,
            step,
            self.config.worker_name,
            _duration_ms(started_at),
            type(exc).__name__,
            _log_fields(**fields),
        )

    def _migrate_legacy_state(self) -> bool:
        """Copy, verify, and persist legacy CoPaw state after storage restore."""

        if self.sync is None:
            raise RuntimeError("storage sync is not initialized")

        worker_home = self.config.worker_home
        legacy_dir = worker_home / ".copaw"
        legacy_secret_dir = worker_home / ".copaw.secret"
        target_dir = self.config.qwenpaw_working_dir
        marker = target_dir / COPAW_MIGRATION_MARKER

        if marker.is_file():
            self._remove_legacy_state(legacy_dir, legacy_secret_dir)
            logger.info(
                "legacy CoPaw state migration already complete component=worker worker=%s marker=%s",
                self.config.worker_name,
                marker,
            )
            return False
        if not legacy_dir.exists() and not legacy_secret_dir.exists():
            return False

        target_secret_dir = self._qwenpaw_secret_dir()
        if legacy_secret_dir.exists():
            try:
                target_secret_dir.relative_to(worker_home.expanduser().resolve())
            except ValueError:
                raise ValueError(
                    f"QWENPAW_SECRET_DIR is outside worker storage root: {target_secret_dir}",
                ) from None

        copied_paths = []
        migrated_directories = []
        if legacy_dir.exists():
            copied_paths.extend(self._copy_legacy_tree(legacy_dir, target_dir))
            migrated_directories.append(target_dir)
        if legacy_secret_dir.exists():
            copied_paths.extend(self._copy_legacy_tree(legacy_secret_dir, target_secret_dir))
            migrated_directories.append(target_secret_dir)

        if legacy_dir.exists():
            self._rebase_migrated_workspace_paths(target_dir)

        # Persist migrated files before the completion marker. If any upload
        # fails, the marker is not written and the next cold start retries
        # from the authoritative legacy copy restored from object storage.
        self.sync.push_directories(migrated_directories)
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text("copaw-to-qwenpaw\n", encoding="utf-8")
        try:
            self.sync.push_paths([marker])
        except Exception:
            # A local-only marker would make a same-container retry skip an
            # incomplete migration. Only retain it after remote persistence.
            marker.unlink(missing_ok=True)
            raise
        self._remove_legacy_state(legacy_dir, legacy_secret_dir)
        logger.info(
            "migrated legacy CoPaw state to QwenPaw component=worker worker=%s files=%s marker=%s",
            self.config.worker_name,
            len(copied_paths),
            marker,
        )
        return True

    def _qwenpaw_secret_dir(self) -> Path:
        """Resolve the secret directory as the QwenPaw child process does."""

        configured = os.environ.get(
            "QWENPAW_SECRET_DIR",
            f"{self.config.qwenpaw_working_dir}.secret",
        )
        secret_dir = Path(configured).expanduser()
        if not secret_dir.is_absolute():
            secret_dir = self.config.default_workspace_dir / secret_dir
        return secret_dir.resolve()

    def _rebase_migrated_workspace_paths(self, target_dir: Path) -> None:
        """Point migrated CoPaw workspace metadata at the active QwenPaw tree."""

        def rebase(value: object) -> object:
            if not isinstance(value, str):
                return value
            marker = "/.copaw/workspaces/"
            if marker in value:
                relative = value.split(marker, 1)[1]
                return str(target_dir / "workspaces" / relative)
            if value.endswith("/.copaw/workspaces"):
                return str(target_dir / "workspaces")
            return value

        def update_json(path: Path, update) -> None:
            if not path.is_file():
                return
            try:
                data = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError) as exc:
                raise RuntimeError(f"cannot rebase migrated workspace metadata: {path}") from exc
            if update(data):
                path.write_text(
                    json.dumps(data, ensure_ascii=False, indent=2) + "\n",
                    encoding="utf-8",
                )

        def update_config(data: object) -> bool:
            if not isinstance(data, dict):
                return False
            profiles = data.get("agents", {}).get("profiles", {})
            if not isinstance(profiles, dict):
                return False
            changed = False
            for profile in profiles.values():
                if not isinstance(profile, dict) or "workspace_dir" not in profile:
                    continue
                rebased = rebase(profile["workspace_dir"])
                if rebased != profile["workspace_dir"]:
                    profile["workspace_dir"] = rebased
                    changed = True
            return changed

        def update_agent(data: object) -> bool:
            if not isinstance(data, dict) or "workspace_dir" not in data:
                return False
            rebased = rebase(data["workspace_dir"])
            if rebased == data["workspace_dir"]:
                return False
            data["workspace_dir"] = rebased
            return True

        update_json(target_dir / "config.json", update_config)
        workspaces_dir = target_dir / "workspaces"
        if workspaces_dir.is_dir():
            for agent_config in workspaces_dir.glob("*/agent.json"):
                update_json(agent_config, update_agent)

    def _copy_legacy_tree(self, source: Path, target: Path) -> list[Path]:
        """Merge one legacy tree with legacy files authoritative on conflict."""

        copied_paths = []

        def copy_path(source_path: Path, target_path: Path) -> None:
            if source_path.is_symlink():
                self._remove_path(target_path)
                target_path.parent.mkdir(parents=True, exist_ok=True)
                target_path.symlink_to(os.readlink(source_path), target_is_directory=source_path.is_dir())
                if os.readlink(target_path) != os.readlink(source_path):
                    raise RuntimeError(f"legacy symlink verification failed: {source_path}")
                return
            if source_path.is_dir():
                if (target_path.exists() or target_path.is_symlink()) and (
                    not target_path.is_dir() or target_path.is_symlink()
                ):
                    self._remove_path(target_path)
                target_path.mkdir(parents=True, exist_ok=True)
                for child in source_path.iterdir():
                    copy_path(child, target_path / child.name)
                return
            if source_path.is_file():
                if (target_path.exists() or target_path.is_symlink()) and (
                    target_path.is_dir() or target_path.is_symlink()
                ):
                    self._remove_path(target_path)
                target_path.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source_path, target_path)
                if not filecmp.cmp(source_path, target_path, shallow=False):
                    raise RuntimeError(f"legacy file verification failed: {source_path}")
                copied_paths.append(target_path)
                return
            raise RuntimeError(f"unsupported legacy state entry: {source_path}")

        copy_path(source, target)
        return copied_paths

    @staticmethod
    def _remove_path(path: Path) -> None:
        if path.is_symlink() or path.is_file():
            path.unlink()
        elif path.is_dir():
            shutil.rmtree(path)

    def _remove_legacy_state(self, legacy_dir: Path, legacy_secret_dir: Path) -> None:
        for path in (legacy_dir, legacy_secret_dir):
            if path.exists() or path.is_symlink():
                self._remove_path(path)

    def _prepare_env(self) -> None:
        os.environ["AGENTTEAMS_AGENT_NAME"] = self.config.agent_name
        os.environ["AGENTTEAMS_AGENT_ROLE"] = self.config.agent_role
        os.environ["AGENTTEAMS_AGENT_HOME"] = str(self.config.worker_home)
        os.environ["AGENTTEAMS_WORKER_HOME"] = str(self.config.worker_home)
        os.environ.setdefault("AGENTTEAMS_WORKER_NAME", self.config.worker_name)
        os.environ["AGENTTEAMS_FS_ENDPOINT"] = self.config.fs_endpoint
        os.environ["AGENTTEAMS_FS_ACCESS_KEY"] = self.config.fs_access_key
        os.environ["AGENTTEAMS_FS_SECRET_KEY"] = self.config.fs_secret_key
        os.environ["AGENTTEAMS_FS_BUCKET"] = self.config.fs_bucket
        os.environ.setdefault(
            "AGENTTEAMS_STORAGE_PREFIX",
            f"agentteams/{self.config.fs_bucket}",
        )
        os.environ["QWENPAW_WORKING_DIR"] = str(self.config.qwenpaw_working_dir)
        os.environ["AGENT_WORKSPACE"] = str(self.config.default_workspace_dir)
        os.environ["AGENTTEAMS_SHARED_DIR"] = str(self.config.shared_dir)
        os.environ["TEAMHARNESS_SHARED_DIR"] = str(self.config.shared_dir)
        os.environ["TEAMHARNESS_RUNTIME_CONFIG"] = str(self.config.runtime_config_path)
        os.environ.setdefault("QWENPAW_SECRET_DIR", f"{self.config.qwenpaw_working_dir}.secret")
        os.environ.setdefault("QWENPAW_RUNNING_IN_CONTAINER", "true")

    def _link_workspace_shared(self) -> None:
        shared_dir = self._workspace_shared_dir or self.config.shared_dir
        workspace_shared = self.config.default_workspace_dir / "shared"
        shared_dir.mkdir(parents=True, exist_ok=True)
        workspace_shared.parent.mkdir(parents=True, exist_ok=True)

        if workspace_shared.is_symlink():
            if workspace_shared.resolve() == shared_dir.resolve():
                return
            workspace_shared.unlink()
        elif workspace_shared.exists():
            if workspace_shared.is_dir():
                shutil.rmtree(workspace_shared)
            else:
                workspace_shared.unlink()

        target = os.path.relpath(shared_dir, workspace_shared.parent)
        workspace_shared.symlink_to(target, target_is_directory=True)
        logger.info(
            "linked qwenpaw workspace shared dir component=worker step=link_workspace_shared worker=%s path=%s target=%s",
            self.config.worker_name,
            workspace_shared,
            target,
        )

    def _apply_runtime_storage(self, runtime_config) -> None:
        shared_prefix = self._runtime_shared_prefix(runtime_config)
        shared_dir = self._local_shared_dir_for_prefix(shared_prefix)
        self._workspace_shared_dir = shared_dir
        os.environ["AGENTTEAMS_SHARED_DIR"] = str(shared_dir)
        os.environ["TEAMHARNESS_SHARED_DIR"] = str(shared_dir)
        if shared_prefix and shared_prefix != "shared":
            os.environ["AGENTTEAMS_SHARED_STORAGE_PREFIX"] = shared_prefix
            if self.sync is not None:
                logger.info(
                    "startup component=worker stage=mirror_team_shared event=begin worker=%s shared_prefix=%s local_dir=%s",
                    self.config.worker_name,
                    shared_prefix,
                    shared_dir,
                )
                self.sync.mirror_prefix(shared_prefix, shared_dir)
        else:
            os.environ.pop("AGENTTEAMS_SHARED_STORAGE_PREFIX", None)

    def _reconcile_runtime_storage(self, runtime_config: MemberRuntimeConfig) -> None:
        shared_prefix = self._runtime_shared_prefix(runtime_config)
        shared_dir = self._local_shared_dir_for_prefix(shared_prefix)
        if self._workspace_shared_dir is None:
            self._apply_runtime_storage(runtime_config)
            self._link_workspace_shared()
            return
        current_prefix = os.getenv("AGENTTEAMS_SHARED_STORAGE_PREFIX", "").strip() or self.config.shared_prefix
        if self._workspace_shared_dir == shared_dir and current_prefix == shared_prefix:
            return
        self._apply_runtime_storage(runtime_config)
        self._link_workspace_shared()
        self._configure_builtin_plugin_mcp_clients()
        self._configure_builtin_plugin_mcp_policies()

    def _sync_managed_skills(self, skill_names: list[str]) -> None:
        if self.sync is None:
            raise RuntimeError("file sync is not initialized")
        for name in skill_names:
            if not name or name in {".", ".."} or "/" in name or "\\" in name:
                raise ValueError(f"invalid assigned skill name: {name!r}")
            self.sync.mirror_prefix(
                f"{self.sync.remote_prefix}/skills/{name}",
                self.config.default_workspace_dir / "skills" / name,
            )

    def _runtime_shared_prefix(self, runtime_config) -> str:
        storage = getattr(runtime_config, "storage", {}) or {}
        prefix = str(storage.get("sharedPrefix") or "").strip() if isinstance(storage, dict) else ""
        if not prefix:
            return self.config.shared_prefix
        return _relative_storage_prefix(prefix, self.config.fs_bucket)

    def _local_shared_dir_for_prefix(self, shared_prefix: str) -> Path:
        if self.config.shared_dir_override is not None:
            return self.config.shared_dir
        prefix = shared_prefix.strip().strip("/")
        if not prefix or prefix == "shared":
            return self.config.shared_dir
        path = Path(prefix)
        if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
            logger.warning(
                "invalid shared storage prefix component=worker step=runtime_storage action=use_default "
                "worker=%s shared_prefix=%s",
                self.config.worker_name,
                shared_prefix,
            )
            return self.config.shared_dir
        return self.config.install_dir.parent.joinpath(*path.parts)

    def _apply_runtime_identity(self, runtime_config) -> None:
        role = runtime_config.member_role
        if not role:
            return
        self.config.agent_role = role
        os.environ["AGENTTEAMS_AGENT_ROLE"] = role
        os.environ["AGENTTEAMS_WORKER_ROLE"] = role

    def _ensure_session_file_prompt_policy(self) -> None:
        self.config.default_workspace_dir.mkdir(parents=True, exist_ok=True)
        for file_name in ("AGENTS.md", "SOUL.md"):
            prompt_file = self.config.default_workspace_dir / file_name
            existing = prompt_file.read_text(encoding="utf-8") if prompt_file.exists() else ""
            if SESSION_FILE_PROMPT_POLICY_MARKER in existing:
                continue
            separator = "\n" if existing and not existing.endswith("\n") else ""
            prefix = "\n" if existing.strip() else ""
            prompt_file.write_text(
                f"{existing}{separator}{prefix}{SESSION_FILE_PROMPT_POLICY}\n",
                encoding="utf-8",
            )

    def _apply_runtime_adapter(self, force: bool = False) -> None:
        self._prepare_default_plugins()
        if force:
            self._configure_builtin_plugin_mcp_clients(force=True)
        else:
            self._configure_builtin_plugin_mcp_clients()
        self._configure_builtin_plugin_mcp_policies()
        self._ensure_session_file_prompt_policy()

    def _force_apply_runtime_adapter(self) -> None:
        self._apply_runtime_adapter(force=True)

    def _prepare_default_plugins(self) -> None:
        builtin_root = self._builtin_qwenpaw_plugins_dir()
        self._prepare_builtin_plugin(
            "agentteams-matrix-channel",
            builtin_root / "agentteams-matrix-channel",
        )
        self._prepare_builtin_plugin("teamharness", builtin_root / "teamharness")
        self._prepare_builtin_plugin("workerflow", builtin_root / "workerflow")

    def _builtin_qwenpaw_plugins_dir(self) -> Path:
        configured = os.getenv("AGENTTEAMS_BUILTIN_QWENPAW_PLUGINS_DIR", "").strip()
        return Path(configured) if configured else DEFAULT_BUILTIN_QWENPAW_PLUGINS_DIR

    def _prepare_builtin_plugin(self, plugin_name: str, source_dir: Path) -> None:
        target_dir = self.config.qwenpaw_working_dir / "plugins" / plugin_name
        step_started = self._log_plugin_step_begin(
            plugin_name,
            "prepare_builtin",
            source_dir=source_dir,
            target_dir=target_dir,
        )
        try:
            self._validate_builtin_plugin(plugin_name, source_dir)
            if self._builtin_plugin_current(source_dir, target_dir):
                self._log_plugin_step_complete(plugin_name, "prepare_builtin", step_started, action="unchanged")
                return
            if target_dir.is_symlink() or target_dir.is_file():
                target_dir.unlink()
            elif target_dir.exists():
                shutil.rmtree(target_dir)
            target_dir.parent.mkdir(parents=True, exist_ok=True)
            shutil.copytree(source_dir, target_dir)
        except Exception as exc:
            self._log_plugin_step_failed(plugin_name, "prepare_builtin", step_started, exc)
            raise
        self._log_plugin_step_complete(plugin_name, "prepare_builtin", step_started, action="copied")

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
            and self._plugin_directory_digest(target_dir) == expected_digest
        )

    def _plugin_directory_digest(self, plugin_dir: Path) -> str:
        digest = hashlib.sha256()
        for path in sorted(plugin_dir.rglob("*")):
            if not path.is_file() or path.name == BUILTIN_QWENPAW_PLUGIN_MARKER:
                continue
            rel = path.relative_to(plugin_dir).as_posix()
            digest.update(rel.encode("utf-8"))
            digest.update(b"\0")
            digest.update(path.read_bytes())
            digest.update(b"\0")
        return digest.hexdigest()

    def _install_teamharness_plugin(self) -> None:
        plugin_source = Path(
            os.getenv(
                "AGENTTEAMS_TEAMHARNESS_QWENPAW_PLUGIN_PACKAGE",
                "/opt/agentteams/plugins/teamharness-qwenpaw.zip",
            )
        )
        self._install_qwenpaw_plugin_package("teamharness", plugin_source, "teamharness-qwenpaw-plugin-")

    def _install_workerflow_plugin(self) -> None:
        plugin_source = Path(
            os.getenv(
                "AGENTTEAMS_WORKERFLOW_QWENPAW_PLUGIN_PACKAGE",
                "/opt/agentteams/plugins/workerflow-qwenpaw.zip",
            )
        )
        self._install_qwenpaw_plugin_package("workerflow", plugin_source, "workerflow-qwenpaw-plugin-")

    def _install_default_plugins(self) -> None:
        self._install_teamharness_plugin()
        self._install_workerflow_plugin()

    def _install_qwenpaw_plugin_package(self, plugin_name: str, plugin_source: Path, temp_prefix: str) -> None:
        package_type = self._qwenpaw_plugin_package_type(plugin_source)
        step_started = self._log_plugin_step_begin(
            plugin_name,
            "install",
            package_type=package_type,
            package_path=plugin_source,
        )
        try:
            if not plugin_source.exists():
                raise RuntimeError(f"{plugin_name} qwenpaw plugin package missing: {plugin_source}")
            qwenpaw_bin = shutil.which("qwenpaw") or str(Path(sys.executable).with_name("qwenpaw"))
            if plugin_source.is_dir():
                self._run_qwenpaw_plugin_install(qwenpaw_bin, plugin_source)
            elif zipfile.is_zipfile(plugin_source):
                with tempfile.TemporaryDirectory(prefix=temp_prefix) as tmp:
                    package_dir = self._extract_qwenpaw_plugin_zip(plugin_source, Path(tmp))
                    self._run_qwenpaw_plugin_install(qwenpaw_bin, package_dir)
            else:
                raise RuntimeError(f"{plugin_name} qwenpaw plugin package must be a directory or zip: {plugin_source}")
        except Exception as exc:
            self._log_plugin_step_failed(plugin_name, "install", step_started, exc, package_type=package_type)
            raise
        self._log_plugin_step_complete(plugin_name, "install", step_started, package_type=package_type)

    def _qwenpaw_plugin_package_type(self, plugin_source: Path) -> str:
        if plugin_source.is_dir():
            return "directory"
        if plugin_source.exists() and zipfile.is_zipfile(plugin_source):
            return "zip"
        if not plugin_source.exists():
            return "missing"
        return "unsupported"

    def _run_qwenpaw_plugin_install(self, qwenpaw_bin: str, package_dir: Path) -> None:
        command = [qwenpaw_bin, "plugin", "install", str(package_dir), "--force"]
        logger.info("installing qwenpaw plugin package=%s", package_dir)
        subprocess.run(command, check=True)

    def _extract_qwenpaw_plugin_zip(self, zip_path: Path, target_dir: Path) -> Path:
        with zipfile.ZipFile(zip_path) as archive:
            target_root = target_dir.resolve()
            for name in archive.namelist():
                resolved = (target_dir / name).resolve()
                try:
                    resolved.relative_to(target_root)
                except ValueError:
                    raise RuntimeError(f"unsafe qwenpaw plugin package path: {name}")
            archive.extractall(target_dir)

        packages = [
            path
            for path in target_dir.iterdir()
            if path.is_dir() and (path / "plugin.json").is_file()
        ]
        if len(packages) != 1:
            raise RuntimeError(f"expected one qwenpaw plugin package in {zip_path}")
        return packages[0]

    async def _run_qwenpaw(self) -> None:
        qwenpaw_bin = shutil.which("qwenpaw") or str(Path(sys.executable).with_name("qwenpaw"))
        host = "0.0.0.0"
        log_level = os.getenv("QWENPAW_LOG_LEVEL", "info")
        command = [
            qwenpaw_bin,
            "app",
            "--host",
            host,
            "--port",
            str(self.config.console_port),
            "--log-level",
            log_level,
        ]
        stage_started = self._log_worker_stage_begin(
            "start_qwenpaw_app",
            binary=qwenpaw_bin,
            host=host,
            port=self.config.console_port,
            cwd=self.config.default_workspace_dir,
            log_level=log_level,
        )
        try:
            self._process = await asyncio.create_subprocess_exec(
                *command,
                cwd=str(self.config.default_workspace_dir),
            )
        except Exception as exc:
            self._log_worker_stage_failed(
                "start_qwenpaw_app",
                stage_started,
                exc,
                binary=qwenpaw_bin,
                port=self.config.console_port,
                cwd=self.config.default_workspace_dir,
            )
            self.heartbeat.update(
                "not_ready",
                "qwenpaw app failed to start",
                {"operation": "run_qwenpaw", "error_type": type(exc).__name__},
            )
            raise
        self._log_worker_stage_complete(
            "start_qwenpaw_app",
            stage_started,
            pid=getattr(self._process, "pid", "-"),
            port=self.config.console_port,
        )
        try:
            await self._wait_for_qwenpaw_api()
            await asyncio.to_thread(
                self.api_client.configure_agent,
                DEFAULT_AGENT_ID,
                {
                    "name": self.config.agent_name,
                    "workspace_dir": str(self.config.default_workspace_dir),
                    "approval_level": "AUTO",
                    "system_prompt_files": ["AGENTS.md", "SOUL.md", "TEAMS.md"],
                },
            )
            await asyncio.to_thread(
                self.api_client.disable_agent_if_present,
                "QwenPaw_QA_Agent_0.2",
            )
            await asyncio.to_thread(self._configure_builtin_plugin_mcp_clients)
            await asyncio.to_thread(self._configure_builtin_plugin_mcp_policies)
            runtime_config = self._initial_runtime_config or self.updater.load()
            stage_started = self._log_worker_stage_begin("apply_desired_state")
            await asyncio.to_thread(
                self.updater.apply_once,
                runtime_config,
                True,
                False,
            )
            self._ensure_session_file_prompt_policy()
            self._log_worker_stage_complete("apply_desired_state", stage_started)
        except Exception as exc:
            if self._process.returncode is not None:
                returncode = self._process.returncode
                self.heartbeat.update(
                    "not_ready",
                    "qwenpaw app exited unexpectedly",
                    {"operation": "run_qwenpaw", "returncode": returncode},
                )
                logger.warning(
                    "qwenpaw app exited component=worker stage=start_qwenpaw_app "
                    "event=exited worker=%s returncode=%s stopping=False duration_ms=0",
                    self.config.worker_name,
                    returncode,
                )
                return
            self.heartbeat.update("not_ready", str(exc))
            if self._process.returncode is None:
                self._process.terminate()
                await self._process.wait()
            raise

        stage_started = self._log_worker_stage_begin(
            "start_update_loop",
            interval_seconds=self.config.runtime_config_poll_interval,
        )
        self._update_task = asyncio.create_task(
            self.updater.loop(),
            name=f"qwenpaw-worker-{self.config.worker_name}-update-loop",
        )
        self._log_worker_stage_complete(
            "start_update_loop",
            stage_started,
            interval_seconds=self.config.runtime_config_poll_interval,
        )
        logger.info(
            "qwenpaw worker startup complete component=worker worker=%s",
            self.config.worker_name,
        )
        process_started_at = time.monotonic()
        self._heartbeat_probe_task = asyncio.create_task(self._heartbeat_probe_loop())
        returncode = await self._process.wait()
        if not self._stopping:
            self.heartbeat.update(
                "not_ready",
                "qwenpaw app exited unexpectedly",
                {"operation": "run_qwenpaw", "returncode": returncode},
            )
            logger.warning(
                "qwenpaw app exited component=worker stage=start_qwenpaw_app event=exited worker=%s "
                "returncode=%s stopping=False duration_ms=%s",
                self.config.worker_name,
                returncode,
                _duration_ms(process_started_at),
            )
        else:
            logger.info(
                "qwenpaw app exited component=worker stage=start_qwenpaw_app event=exited worker=%s "
                "returncode=%s stopping=True duration_ms=%s",
                self.config.worker_name,
                returncode,
                _duration_ms(process_started_at),
            )

    async def _wait_for_qwenpaw_api(self) -> None:
        deadline = time.monotonic() + 60
        last_error: Optional[Exception] = None
        while time.monotonic() < deadline:
            if self._process is not None and self._process.returncode is not None:
                raise RuntimeError(
                    f"qwenpaw app exited before API readiness: {self._process.returncode}",
                )
            try:
                await asyncio.to_thread(self.api_client.require_version, "2.2.0")
                return
            except Exception as exc:
                last_error = exc
                await asyncio.sleep(0.5)
        raise RuntimeError(f"qwenpaw API did not become ready: {last_error}")

    def _configure_builtin_plugin_mcp_clients(self, force: bool = False) -> None:
        existing = {str(item.get("key")) for item in self.api_client.list_mcp()}
        for plugin_id in ("teamharness", "workerflow"):
            plugin_dir = self.config.qwenpaw_working_dir / "plugins" / plugin_id
            asset_dir = plugin_dir / plugin_id
            payload = {
                "name": plugin_id,
                "description": f"AgentTeams {plugin_id} MCP server",
                "enabled": True,
                "transport": "stdio",
                "command": sys.executable,
                "args": [str(asset_dir / "mcp" / "server.py")],
                "env": {
                    name: value
                    for name in (
                        "TEAMHARNESS_RUNTIME_CONFIG",
                        "TEAMHARNESS_SHARED_DIR",
                        "AGENTTEAMS_MATRIX_URL",
                        "AGENTTEAMS_WORKER_MATRIX_TOKEN",
                        "AGENTTEAMS_MATRIX_USER_ID",
                        "AGENTTEAMS_WORKER_ROLE",
                        "AGENTTEAMS_AGENT_ROLE",
                        "AGENTTEAMS_WORKER_NAME",
                        "AGENTTEAMS_STORAGE_PREFIX",
                        "AGENTTEAMS_SHARED_STORAGE_PREFIX",
                        "AGENTTEAMS_FS_BUCKET",
                        "AGENTTEAMS_FS_ENDPOINT",
                        "AGENTTEAMS_FS_ACCESS_KEY",
                        "AGENTTEAMS_FS_SECRET_KEY",
                        "QWENPAW_WORKING_DIR",
                    )
                    if (value := os.getenv(name, "").strip())
                },
                "cwd": str(asset_dir),
            }
            identity = self._builtin_mcp_payload_identity(plugin_dir, payload)
            if plugin_id not in existing:
                self.api_client.create_mcp(plugin_id, payload)
            elif force or self._builtin_mcp_payload_identities.get(plugin_id) != identity:
                self.api_client.update_mcp(plugin_id, payload)
            self._builtin_mcp_payload_identities[plugin_id] = identity

    def _builtin_mcp_payload_identity(
        self,
        plugin_dir: Path,
        payload: dict[str, object],
    ) -> str:
        marker = plugin_dir / BUILTIN_QWENPAW_PLUGIN_MARKER
        plugin_digest = marker.read_text(encoding="utf-8").strip() if marker.is_file() else ""
        serialized = json.dumps(
            {"payload": payload, "pluginDigest": plugin_digest},
            sort_keys=True,
            ensure_ascii=False,
            separators=(",", ":"),
        )
        return hashlib.sha256(serialized.encode("utf-8")).hexdigest()

    def _configure_builtin_plugin_mcp_policies(self) -> None:
        allow_policy = {
            "default_effect": "allow",
            "client_overrides": [],
            "tool_defaults": [],
            "tool_overrides": [],
        }
        for plugin_id in ("teamharness", "workerflow"):
            self.api_client.wait_for_mcp_tools(plugin_id)
            self.api_client.put_mcp_policy(plugin_id, allow_policy)

    async def _heartbeat_probe_loop(self) -> None:
        await run_worker_heartbeat_loop(
            self.heartbeat,
            worker_name=self.config.worker_cr_name,
            port=self.config.console_port,
        )
