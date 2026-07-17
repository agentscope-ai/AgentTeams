"""QwenPaw Worker main entry point."""

from __future__ import annotations

import asyncio
import importlib.util
import logging
import os
from pathlib import Path
import shutil
import sys
import time
from typing import Optional

from qwenpaw_worker.config import WorkerConfig, _relative_storage_prefix
from qwenpaw_worker.heartbeat import WorkerHeartbeat, run_worker_heartbeat_loop
from qwenpaw_worker.plugin_bootstrap import PluginBootstrap
from qwenpaw_worker.plugin_install import BUILTIN_QWENPAW_PLUGIN_MARKER
from qwenpaw_worker.runtime_configurator import RuntimeConfigurator
from qwenpaw_worker.security_bootstrap import SecurityBootstrap
from qwenpaw_worker.sync import FileSync, push_loop
from qwenpaw_worker.update import MemberRuntimeConfig, RuntimeUpdater

logger = logging.getLogger(__name__)

DEFAULT_AGENT_ID = "default"

__all__ = ["BUILTIN_QWENPAW_PLUGIN_MARKER", "DEFAULT_AGENT_ID", "Worker"]


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


class Worker:
    def __init__(self, config: WorkerConfig) -> None:
        self.config = config
        self.sync: Optional[FileSync] = None
        self.heartbeat = WorkerHeartbeat(config.qwenpaw_working_dir / "heartbeat.json")
        self.updater = RuntimeUpdater(
            config=config,
            adapter_apply=self._apply_runtime_adapter,
            team_context_renderer=self._render_teamharness_context,
        )
        self._process: Optional[asyncio.subprocess.Process] = None
        self._heartbeat_probe_task: Optional[asyncio.Task] = None
        self._push_task: Optional[asyncio.Task] = None
        self._update_task: Optional[asyncio.Task] = None
        self._stopping = False
        self._workspace_shared_dir: Optional[Path] = None
        self._runtime = RuntimeConfigurator(config)
        self._security = SecurityBootstrap(config)
        self._plugins = PluginBootstrap(
            config,
            log_step_begin=self._log_plugin_step_begin,
            log_step_complete=self._log_plugin_step_complete,
            log_step_failed=self._log_plugin_step_failed,
        )

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
        self.config.default_workspace_dir.mkdir(parents=True, exist_ok=True)
        self.heartbeat.persist()

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
        self.updater.runtime_config_pull = lambda: self.sync.pull_runtime_config(self.config.runtime_config_path)

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

        try:
            stage_started = self._log_worker_stage_begin("prepare_qwenpaw_runtime")
            self._link_workspace_shared()
            self._configure_qwenpaw_runtime()
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

        try:
            stage_started = self._log_worker_stage_begin("apply_desired_state")
            self.updater.apply_once(runtime_config=runtime_config, force=True, reapply_adapter=False)
            self._ensure_session_file_prompt_policy()
        except Exception as exc:
            self._log_worker_stage_failed("apply_desired_state", stage_started, exc)
            self.heartbeat.update("not_ready", str(exc))
            return False
        self._log_worker_stage_complete("apply_desired_state", stage_started)

        try:
            stage_started = self._log_worker_stage_begin("sync_teamharness_assets")
            self._apply_teamharness_assets()
        except Exception as exc:
            self._log_worker_stage_failed("sync_teamharness_assets", stage_started, exc)
            self.heartbeat.update("not_ready", str(exc))
            return False
        self._log_worker_stage_complete("sync_teamharness_assets", stage_started)

        try:
            stage_started = self._log_worker_stage_begin("sync_workerflow_assets")
            self._apply_workerflow_assets()
        except Exception as exc:
            self._log_worker_stage_failed("sync_workerflow_assets", stage_started, exc)
            self.heartbeat.update("not_ready", str(exc))
            return False
        self._log_worker_stage_complete("sync_workerflow_assets", stage_started)

        stage_started = self._log_worker_stage_begin("start_push_loop", interval_seconds=5)
        self._push_task = asyncio.create_task(
            push_loop(self.sync, check_interval=5),
            name=f"qwenpaw-worker-{self.config.worker_name}-push-loop",
        )
        self._log_worker_stage_complete("start_push_loop", stage_started, interval_seconds=5)
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
        logger.info("qwenpaw worker startup complete component=worker worker=%s", self.config.worker_name)
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
            "startup component=worker stage=%s event=failed worker=%s duration_ms=%s error_type=%s %s",
            stage,
            self.config.worker_name,
            _duration_ms(started_at),
            type(exc).__name__,
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

    def _prepare_env(self) -> None:
        self._runtime.prepare_env()

    def _link_workspace_shared(self) -> None:
        shared_dir = self._workspace_shared_dir or self.config.shared_dir
        self._runtime.link_workspace_shared(shared_dir)

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

    def _configure_qwenpaw_runtime(self) -> None:
        self._runtime.configure_qwenpaw_runtime()

    def _ensure_session_file_prompt_policy(self) -> None:
        self._security.ensure_session_file_prompt_policy()

    def _apply_runtime_adapter(self) -> None:
        self._prepare_default_plugins()
        self._apply_teamharness_assets()
        self._apply_workerflow_assets()
        self._ensure_session_file_prompt_policy()

    def _prepare_default_plugins(self) -> None:
        self._plugins.prepare_default_plugins()

    def _prepare_builtin_plugin(self, plugin_name: str, source_dir: Path) -> None:
        self._plugins.prepare_builtin_plugin(plugin_name, source_dir)

    def _builtin_plugin_current(self, source_dir: Path, target_dir: Path) -> bool:
        return self._plugins._builtin_plugin_current(source_dir, target_dir)

    def _install_default_plugins(self) -> None:
        self._plugins.install_default_plugins()

    def _extract_qwenpaw_plugin_zip(self, zip_path: Path, target_dir: Path) -> Path:
        return self._plugins.extract_plugin_zip(zip_path, target_dir)

    def _apply_teamharness_assets(self) -> dict:
        return self._apply_plugin_assets(
            plugin_name="teamharness",
            module_name="agentteams_teamharness_qwenpaw_plugin",
            entrypoint_name="apply_teamharness",
        )

    def _render_teamharness_context(self, runtime_config: MemberRuntimeConfig) -> str:
        plugin_file = self.config.qwenpaw_working_dir / "plugins" / "teamharness" / "plugin.py"
        if not plugin_file.is_file():
            return ""

        spec = importlib.util.spec_from_file_location("hiclaw_teamharness_qwenpaw_plugin_renderer", plugin_file)
        if spec is None or spec.loader is None:
            return ""

        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        render = getattr(module, "render_team_context", None)
        if not callable(render):
            return ""
        text = render(runtime_config.raw)
        return text if isinstance(text, str) else ""

    def _apply_workerflow_assets(self) -> dict:
        return self._apply_plugin_assets(
            plugin_name="workerflow",
            module_name="agentteams_workerflow_qwenpaw_plugin",
            entrypoint_name="apply_workerflow",
        )

    def _apply_plugin_assets(self, *, plugin_name: str, module_name: str, entrypoint_name: str) -> dict:
        plugin_file = self.config.qwenpaw_working_dir / "plugins" / plugin_name / "plugin.py"
        step_started = self._log_plugin_step_begin(
            plugin_name,
            "load",
            plugin_file=plugin_file,
            plugin_file_exists=plugin_file.is_file(),
            entrypoint=entrypoint_name,
        )
        try:
            if not plugin_file.is_file():
                raise RuntimeError(f"installed {plugin_name} qwenpaw plugin missing: {plugin_file}")

            spec = importlib.util.spec_from_file_location(module_name, plugin_file)
            if spec is None or spec.loader is None:
                raise RuntimeError(f"failed to load {plugin_name} qwenpaw plugin: {plugin_file}")

            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            apply_plugin = getattr(module, entrypoint_name, None)
            if not callable(apply_plugin):
                raise RuntimeError(f"installed {plugin_name} qwenpaw plugin has no {entrypoint_name}: {plugin_file}")
        except Exception as exc:
            self._log_plugin_step_failed(plugin_name, "load", step_started, exc, entrypoint=entrypoint_name)
            raise
        self._log_plugin_step_complete(plugin_name, "load", step_started, entrypoint=entrypoint_name)

        step_started = self._log_plugin_step_begin(plugin_name, "apply", entrypoint=entrypoint_name)
        try:
            result = apply_plugin()
            if not isinstance(result, dict) or result.get("ok") is not True:
                raise RuntimeError(f"{plugin_name} asset sync failed: {result!r}")
        except Exception as exc:
            self._log_plugin_step_failed(plugin_name, "apply", step_started, exc, entrypoint=entrypoint_name)
            raise
        self._log_plugin_step_complete(
            plugin_name,
            "apply",
            step_started,
            entrypoint=entrypoint_name,
            ok=result.get("ok"),
            result_key_count=len(result),
        )
        return result

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

    async def _heartbeat_probe_loop(self) -> None:
        await run_worker_heartbeat_loop(
            self.heartbeat,
            worker_name=self.config.worker_cr_name,
            port=self.config.console_port,
        )
