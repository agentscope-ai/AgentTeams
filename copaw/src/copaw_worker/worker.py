"""
Worker main entry point.

Bootstrap flow:
1. Ensure the MinIO client exists and initialize the worker FileSync workspace.
2. Mirror the worker prefix and shared folders from MinIO into standard space.
3. Load openclaw.json and refresh the Matrix access token/device when possible.
4. Bridge standard space into CoPaw runtime config, prompts, and skill links.
5. Start the local-to-remote preservation loop for runtime edits.
6. Launch CoPaw's FastAPI app; its lifecycle starts the runner, channels, and web console.
"""
from __future__ import annotations

import asyncio
from contextlib import suppress
import json
import logging
import os
import platform
import shutil
import stat
from pathlib import Path
from typing import Any, Optional

from rich.console import Console
from rich.panel import Panel

from copaw_worker.config import WorkerConfig
from copaw_worker.health import (
    ComponentHealth,
    HealthState,
    check_copaw_service,
    check_matrix_service,
    check_model_service,
)
from copaw_worker.sync import FileSync, push_loop, sync_loop
from copaw_worker.worker_api import WorkerAPIServer
from copaw_worker.bridge import (
    bridge_standard_to_runtime,
    sync_mcporter_config_to_runtime,
    sync_skills_to_runtime,
)

console = Console()
logger = logging.getLogger(__name__)


class Worker:
    def __init__(self, config: WorkerConfig) -> None:
        self.config = config
        self.worker_name = config.worker_name
        self.sync: Optional[FileSync] = None
        self._copaw_working_dir: Optional[Path] = None
        self._runner = None
        self._channel_manager = None
        self._push_task: Optional[asyncio.Task[None]] = None
        self._pull_task: Optional[asyncio.Task[None]] = None
        self._worker_api: WorkerAPIServer | None = None
        self._server: Any | None = None
        self._health: HealthState | None = None
        self._openclaw_cfg: dict[str, Any] | None = None
        self._matrix_ready_marker: Path | None = None

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def run(self) -> None:
        if not await self.start():
            return
        try:
            await self._run_copaw()
        except asyncio.CancelledError:
            pass
        finally:
            await self.stop()

    async def stop(self) -> None:
        console.print("[yellow]Stopping worker...[/yellow]")
        logger.info(
            "worker stop requested worker=%s has_server=%s has_push_task=%s",
            self.worker_name,
            self._server is not None,
            self._push_task is not None,
        )

        if self._server is not None:
            self._server.should_exit = True
            logger.info("uvicorn shutdown requested worker=%s", self.worker_name)

        if self._push_task is not None:
            self._push_task.cancel()
            with suppress(asyncio.CancelledError):
                await self._push_task
            self._push_task = None
            logger.info("FileSync push loop stopped worker=%s", self.worker_name)

        if self._pull_task is not None:
            self._pull_task.cancel()
            with suppress(asyncio.CancelledError):
                await self._pull_task
            self._pull_task = None
            logger.info("FileSync pull loop stopped worker=%s", self.worker_name)

        if self._worker_api is not None:
            await self._worker_api.stop()
            self._worker_api = None

        console.print("[green]Worker stopped.[/green]")
        logger.info("worker stopped worker=%s", self.worker_name)

    # ------------------------------------------------------------------
    # Startup
    # ------------------------------------------------------------------

    async def start(self) -> bool:
        logger.info(
            "worker startup begin worker=%s install_dir=%s minio_endpoint=%s bucket=%s console_port=%s",
            self.worker_name,
            self.config.install_dir,
            self.config.minio_endpoint,
            self.config.minio_bucket,
            self.config.console_port,
        )
        console.print(
            Panel.fit(
                f"[bold green]CoPaw Worker[/bold green]\n"
                f"Worker: [cyan]{self.worker_name}[/cyan]",
                title="Starting",
            )
        )

        # 1. Ensure mc (MinIO Client) is available
        logger.info("startup stage=ensure_mc worker=%s", self.worker_name)
        self._ensure_mc()

        # 2. Init file sync
        self._copaw_working_dir = self.config.install_dir / self.worker_name / ".copaw"
        self._health = HealthState(self._copaw_working_dir / "health.json")
        self._health.persist()
        workspace_dir = self._copaw_working_dir / "workspaces" / "default"
        self.sync = FileSync(
            endpoint=self.config.minio_endpoint,
            access_key=self.config.minio_access_key,
            secret_key=self.config.minio_secret_key,
            bucket=self.config.minio_bucket,
            worker_name=self.worker_name,
            worker_cr_name=self.config.worker_cr_name,
            secure=self.config.minio_secure,
            local_dir=self.config.install_dir / self.worker_name,
            shared_dir=workspace_dir / "shared",
            global_shared_dir=workspace_dir / "global-shared",
        )
        logger.info(
            "startup stage=init_sync worker=%s local_dir=%s copaw_working_dir=%s",
            self.worker_name,
            self.sync.local_dir,
            self._copaw_working_dir,
        )

        # 2. Full mirror from MinIO (restore all state: config, sessions, sync token, etc.)
        #    Mirrors the OpenClaw worker's startup approach: pull everything first,
        #    then preserve local changes via push_loop during runtime.
        console.print("[yellow]Pulling all files from MinIO...[/yellow]")
        logger.info("startup stage=mirror_all worker=%s", self.worker_name)
        try:
            self.sync.mirror_all()
        except Exception as exc:
            logger.exception("startup stage=mirror_all failed worker=%s", self.worker_name)
            console.print(f"[red]Failed to mirror from MinIO: {exc}[/red]")
            self._health.update(
                "sync",
                "unhealthy",
                f"startup mirror failed: {exc}",
                {
                    "operation": "mirror_all",
                    "error_type": type(exc).__name__,
                },
            )
            return False
        self._health.update("sync", "healthy", "startup mirror restored")

        # 3. Parse openclaw.json (already on disk after mirror_all)
        logger.info("startup stage=load_config worker=%s", self.worker_name)
        try:
            openclaw_cfg = self.sync.get_config()
        except Exception as exc:
            logger.exception("startup stage=load_config failed worker=%s", self.worker_name)
            console.print(f"[red]Failed to read config: {exc}[/red]")
            return False

        # 3b. Re-login to Matrix to get fresh access token + device ID
        #     Under E2EE, reusing the old access token (same device_id) with a
        #     regenerated identity key causes other clients to reject key
        #     distribution. Re-login creates a new device_id, matching the
        #     Manager's behavior.
        logger.info("startup stage=matrix_relogin worker=%s", self.worker_name)
        openclaw_cfg = self._matrix_relogin(openclaw_cfg)
        self._openclaw_cfg = openclaw_cfg

        logger.info("startup stage=model_preflight worker=%s", self.worker_name)
        model_status = check_model_service(openclaw_cfg)
        self._health.update(
            "model",
            model_status.healthiness,
            model_status.message,
            model_status.details,
        )
        if model_status.healthiness == "healthy":
            logger.info("model preflight OK worker=%s details=%s", self.worker_name, model_status.details)
        else:
            logger.warning(
                "model preflight failed worker=%s message=%s details=%s",
                self.worker_name,
                model_status.message,
                model_status.details,
            )
            console.print(f"[yellow]Model service preflight failed: {model_status.message}[/yellow]")
            details = model_status.details or {}
            notify_msg = (
                f"⚠️ Model service check failed: {model_status.message}\n"
                f"Provider: {details.get('provider', 'unknown')}, "
                f"Model: {details.get('model', 'unknown')}\n"
                f"Please check model configuration."
            )
            self._notify_matrix(notify_msg, openclaw_cfg)

        # 4. Set up CoPaw working directory
        self._copaw_working_dir.mkdir(parents=True, exist_ok=True)
        self._matrix_ready_marker = (
            Path("/tmp") / f"hiclaw-copaw-{self.worker_name}-matrix-ready"
        )
        self._matrix_ready_marker.unlink(missing_ok=True)
        os.environ["HICLAW_MATRIX_CHANNEL_READY_FILE"] = str(
            self._matrix_ready_marker,
        )
        logger.info(
            "startup stage=prepare_runtime_dir worker=%s copaw_working_dir=%s",
            self.worker_name,
            self._copaw_working_dir,
        )

        # 5. Bridge standard space -> CoPaw runtime space.
        #    This writes prompt files into workspaces/default/ and converts
        #    openclaw.json -> config.json / agent.json / providers.json.
        #    Infer gateway port from FS endpoint so bridge's _port_remap uses
        #    the correct host port instead of the hardcoded default.
        if not os.environ.get("HICLAW_PORT_GATEWAY"):
            from urllib.parse import urlparse
            _parsed = urlparse(self.config.minio_endpoint)
            if _parsed.port:
                os.environ["HICLAW_PORT_GATEWAY"] = str(_parsed.port)
                logger.info(
                    "inferred HICLAW_PORT_GATEWAY=%s from MinIO endpoint worker=%s",
                    _parsed.port,
                    self.worker_name,
                )

        console.print("[yellow]Bridging configuration to CoPaw...[/yellow]")
        logger.info("startup stage=bridge worker=%s", self.worker_name)
        try:
            skill_names = self.sync.list_skills()
            bridge_standard_to_runtime(
                self.sync.local_dir,
                self._copaw_working_dir,
                openclaw_cfg,
                skill_names=skill_names,
            )
        except Exception as exc:
            logger.exception("startup stage=bridge failed worker=%s", self.worker_name)
            console.print(f"[red]Config bridge failed: {exc}[/red]")
            self._health.update(
                "bridge",
                "unhealthy",
                f"standard-to-copaw bridge failed: {exc}",
                {
                    "operation": "bridge_standard_to_runtime",
                    "error_type": type(exc).__name__,
                },
            )
            return False
        self._health.update(
            "bridge",
            "healthy",
            "standard-to-copaw bridge completed",
            {"operation": "bridge_standard_to_runtime"},
        )

        if skill_names:
            console.print(f"[green]Skills installed: {len(skill_names)}[/green]")
            logger.info(
                "skills installed worker=%s count=%d",
                self.worker_name,
                len(skill_names),
            )
            logger.debug("skills installed worker=%s names=%s", self.worker_name, skill_names)
        else:
            logger.info("No extra skills in MinIO for worker %s", self.worker_name)

        # 6. Start runtime sync loops. Remote -> Local refreshes controller-
        #    managed config and skills; shared data remains explicit via filesync.
        logger.info(
            "startup stage=start_sync_loop worker=%s interval_seconds=%s",
            self.worker_name,
            self.config.sync_interval,
        )
        self._pull_task = asyncio.create_task(
            sync_loop(
                self.sync,
                interval=self.config.sync_interval,
                on_pull=self._on_files_pulled,
                health=self._health,
            ),
            name=f"copaw-worker-{self.worker_name}-sync-loop",
        )
        logger.info("startup stage=start_push_loop worker=%s interval_seconds=5", self.worker_name)
        self._push_task = asyncio.create_task(
            push_loop(self.sync, check_interval=5, health=self._health),
            name=f"copaw-worker-{self.worker_name}-push-loop",
        )
        await self._start_worker_api()

        console.print("[bold green]Worker initialized.[/bold green]")
        console.print(
            f"[dim]Web console will start on port {self.config.console_port}[/dim]"
        )
        logger.info("worker startup complete worker=%s", self.worker_name)
        return True

    # ------------------------------------------------------------------
    # CoPaw runner
    # ------------------------------------------------------------------

    async def _start_worker_api(self) -> None:
        self._worker_api = WorkerAPIServer(
            host="0.0.0.0",
            port=self.config.worker_port,
            liveness_handler=self.build_worker_liveness,
            readiness_handler=self.build_worker_readiness,
        )
        await self._worker_api.start()

    async def build_worker_liveness(self) -> dict[str, Any]:
        return {
            "liveness": "alive",
            "message": "worker api alive",
            "details": {"worker_port": self.config.worker_port},
        }

    async def build_worker_readiness(self) -> dict[str, Any]:
        if self._health is None:
            raise RuntimeError("health state is not initialized")
        openclaw_cfg = self._openclaw_cfg or {}

        copaw = await asyncio.to_thread(check_copaw_service, self.config.console_port)
        self._health.update("copaw", copaw.healthiness, copaw.message, copaw.details)

        model = await asyncio.to_thread(check_model_service, openclaw_cfg)
        self._health.update("model", model.healthiness, model.message, model.details)

        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        from .bridge import _port_remap, _is_in_container
        homeserver = _port_remap(matrix_cfg.get("homeserver", ""), _is_in_container())
        matrix = await asyncio.to_thread(check_matrix_service, homeserver)
        if matrix.healthiness == "healthy":
            marker_ready = (
                self._matrix_ready_marker is not None
                and self._matrix_ready_marker.exists()
            )
            if not marker_ready:
                matrix = ComponentHealth(
                    "unhealthy",
                    "Matrix channel is not ready",
                    {
                        **(matrix.details or {}),
                        "channelReady": False,
                    },
                )
        self._health.update("matrix", matrix.healthiness, matrix.message, matrix.details)

        snapshot = self._health.to_dict()
        ready = snapshot["healthiness"] == "healthy"
        return {
            "readiness": "ready" if ready else "not_ready",
            "healthiness": snapshot["healthiness"],
            "message": "worker ready" if ready else snapshot["message"],
            "components": snapshot["components"],
            "updated_at": snapshot["updated_at"],
        }

    async def _mark_copaw_startup_health(
        self,
        *,
        timeout: float = 60,
        interval: float = 0.5,
    ) -> None:
        """Mark CoPaw healthy once its own app health endpoint is reachable."""
        if self._health is None:
            return

        deadline = asyncio.get_running_loop().time() + timeout
        while True:
            status = await asyncio.to_thread(
                check_copaw_service,
                self.config.console_port,
            )
            if status.healthiness == "healthy":
                self._health.update("copaw", status.healthiness, status.message, status.details)
                logger.info(
                    "copaw startup health OK worker=%s details=%s",
                    self.worker_name,
                    status.details,
                )
                return

            if asyncio.get_running_loop().time() >= deadline:
                self._health.update("copaw", status.healthiness, status.message, status.details)
                logger.warning(
                    "copaw startup health failed worker=%s message=%s details=%s",
                    self.worker_name,
                    status.message,
                    status.details,
                )
                return
            await asyncio.sleep(interval)

    async def _run_copaw(self) -> None:
        """Start CoPaw via FastAPI app (includes runner + channels + web console)."""
        import uvicorn
        from copaw.app.channels.registry import clear_builtin_channel_cache
        from copaw_worker.hooks import install_tool_hooks

        install_tool_hooks()
        clear_builtin_channel_cache()
        logger.info(
            "starting CoPaw FastAPI app worker=%s host=%s port=%s",
            self.worker_name,
            "0.0.0.0",
            self.config.console_port,
        )

        uv_config = uvicorn.Config(
            "copaw.app._app:app",
            host="0.0.0.0",
            port=self.config.console_port,
            log_level="info",
        )
        server = uvicorn.Server(uv_config)
        self._server = server
        console.print(
            f"[bold green]CoPaw console available at "
            f"http://127.0.0.1:{self.config.console_port}/[/bold green]"
        )
        try:
            startup_health_task = asyncio.create_task(
                self._mark_copaw_startup_health(),
                name=f"copaw-worker-{self.worker_name}-startup-health",
            )
            await server.serve()
            if not server.should_exit and self._health is not None:
                self._health.update(
                    "copaw",
                    "unhealthy",
                    "CoPaw app exited unexpectedly",
                    {"operation": "run_copaw"},
                )
        except asyncio.CancelledError:
            server.should_exit = True
            logger.info("CoPaw FastAPI app cancelled worker=%s", self.worker_name)
        except Exception as exc:
            logger.exception("CoPaw FastAPI app failed worker=%s", self.worker_name)
            if self._health is not None:
                self._health.update(
                    "copaw",
                    "unhealthy",
                    f"CoPaw app failed: {exc}",
                    {
                        "operation": "run_copaw",
                        "error_type": type(exc).__name__,
                    },
                )
            raise
        finally:
            if "startup_health_task" in locals() and not startup_health_task.done():
                startup_health_task.cancel()
                with suppress(asyncio.CancelledError):
                    await startup_health_task
            if self._server is server:
                self._server = None
            logger.info("CoPaw FastAPI app stopped worker=%s", self.worker_name)

    async def _on_files_pulled(self, pulled_files: list[str]) -> None:
        """Refresh runtime projections after controller-managed files change."""
        assert self.sync is not None
        assert self._copaw_working_dir is not None

        needs_rebridge = "openclaw.json" in pulled_files
        skills_changed = any(f.startswith("skills/") for f in pulled_files)
        mcporter_changed = "config/mcporter.json" in pulled_files
        credagent_changed = "config/credagent.json" in pulled_files

        if skills_changed:
            skill_names = self.sync.list_skills()
            sync_skills_to_runtime(
                self.sync.local_dir,
                self._copaw_working_dir,
                skill_names,
            )

        if mcporter_changed and not needs_rebridge:
            sync_mcporter_config_to_runtime(self.sync.local_dir, self._copaw_working_dir)

        if credagent_changed and not needs_rebridge:
            self._hot_update_credential_guard()

        if not needs_rebridge:
            return

        logger.info("openclaw config changed; re-bridging worker=%s", self.worker_name)
        try:
            openclaw_cfg = json.loads(
                (self.sync.local_dir / "openclaw.json").read_text(encoding="utf-8")
            )
            bridge_standard_to_runtime(
                self.sync.local_dir,
                self._copaw_working_dir,
                openclaw_cfg,
                skill_names=self.sync.list_skills() if skills_changed else None,
            )
            self._openclaw_cfg = openclaw_cfg
            self._hot_update_matrix_channel_config()
            if self._health is not None:
                self._health.update(
                    "bridge",
                    "healthy",
                    "runtime config re-bridged",
                    {"operation": "sync_loop"},
                )
        except Exception as exc:
            logger.exception("runtime config re-bridge failed worker=%s", self.worker_name)
            if self._health is not None:
                self._health.update(
                    "bridge",
                    "unhealthy",
                    f"runtime config re-bridge failed: {exc}",
                    {
                        "operation": "sync_loop",
                        "error_type": type(exc).__name__,
                    },
                )

    def _hot_update_matrix_channel_config(self) -> None:
        """Refresh MatrixChannel allowlists if the channel object is reachable."""
        if self._channel_manager is None or self._copaw_working_dir is None:
            return

        agent_path = self._copaw_working_dir / "workspaces" / "default" / "agent.json"
        try:
            import json
            agent_cfg = json.loads(agent_path.read_text(encoding="utf-8"))
            matrix_cfg = (agent_cfg.get("channels") or {}).get("matrix") or {}
        except Exception as exc:
            logger.warning("failed to load re-bridged Matrix config: %s", exc)
            return

        for channel in getattr(self._channel_manager, "_channels", []):
            cfg = getattr(channel, "_cfg", None)
            if cfg is None or not hasattr(cfg, "group_allow_from"):
                continue
            try:
                parsed = type(cfg)(matrix_cfg)
            except Exception as exc:
                logger.warning("failed to parse re-bridged Matrix config: %s", exc)
                return
            for attr in (
                "allow_from",
                "group_allow_from",
                "group_combined_allow",
                "groups",
                "dm_policy",
                "group_policy",
                "vision_enabled",
                "history_limit",
            ):
                if hasattr(parsed, attr):
                    setattr(cfg, attr, getattr(parsed, attr))
            logger.info("MatrixChannel policy hot-updated worker=%s", self.worker_name)
            return

    def _hot_update_credential_guard(self) -> None:
        """Re-apply credagent.json paths and reload CoPaw's file guard."""
        if self.sync is None or self._copaw_working_dir is None:
            return
        from copaw_worker.hooks.credential_guard import apply_credential_guard

        count = apply_credential_guard(self.sync.local_dir, self._copaw_working_dir)
        try:
            from copaw.security.tool_guard.engine import get_guard_engine

            get_guard_engine().reload_rules()
            logger.info(
                "credential guard hot-reloaded paths=%d worker=%s",
                count,
                self.worker_name,
            )
        except Exception as exc:
            logger.warning("credential guard reload failed: %s", exc)

    # ------------------------------------------------------------------
    # Matrix re-login (E2EE device_id refresh)
    # ------------------------------------------------------------------

    def _matrix_relogin(self, openclaw_cfg: dict) -> dict:
        """Re-login to Matrix to get a fresh access token and device ID.

        Under E2EE, crypto state is not persisted across restarts. Reusing
        the old access token keeps the same device_id but with a new identity
        key, which causes other clients (Element Web) to reject key
        distribution. A fresh login creates a new device_id, matching the
        Manager's restart behavior.

        The password is read directly from MinIO (never written to disk).
        """
        import json
        import urllib.request
        import urllib.error

        # Read password directly from MinIO via mc cat (no disk I/O)
        password_key = f"{self.sync._prefix}/credentials/matrix/password"
        matrix_password = self.sync._cat(password_key)

        if not matrix_password:
            logger.warning(
                "Matrix password not found in MinIO; skipping re-login worker=%s",
                self.worker_name,
            )
            console.print(
                "[dim]No Matrix password found in MinIO, skipping re-login "
                "(E2EE may not work after restart)[/dim]"
            )
            self._health.update(
                "matrix",
                "unhealthy",
                "matrix re-login skipped: missing homeserver or password",
                {
                    "operation": "matrix_relogin",
                    "has_homeserver": bool(
                        openclaw_cfg.get("channels", {})
                        .get("matrix", {})
                        .get("homeserver", "")
                    ),
                    "has_password": False,
                },
            )
            return openclaw_cfg

        matrix_password = matrix_password.strip()
        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        from .bridge import _port_remap, _is_in_container
        homeserver = _port_remap(
            matrix_cfg.get("homeserver", ""), _is_in_container()
        )

        if not homeserver or not matrix_password:
            logger.warning(
                "Matrix re-login skipped due to missing homeserver/password worker=%s has_homeserver=%s",
                self.worker_name,
                bool(homeserver),
            )
            self._health.update(
                "matrix",
                "unhealthy",
                "matrix re-login skipped: missing homeserver or password",
                {
                    "operation": "matrix_relogin",
                    "has_homeserver": bool(homeserver),
                    "has_password": bool(matrix_password),
                },
            )
            return openclaw_cfg

        login_url = f"{homeserver}/_matrix/client/v3/login"
        login_body = json.dumps({
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": self.worker_name},
            "password": matrix_password,
        }).encode()

        try:
            req = urllib.request.Request(
                login_url,
                data=login_body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                login_resp = json.loads(resp.read())

            new_token = login_resp.get("access_token", "")
            new_device = login_resp.get("device_id", "")

            if new_token:
                openclaw_cfg["channels"]["matrix"]["accessToken"] = new_token
                # Write updated config back to disk so bridge reads the new token
                config_path = self.sync.local_dir / "openclaw.json"
                with open(config_path, "w") as f:
                    json.dump(openclaw_cfg, f, indent=2, ensure_ascii=False)
                logger.info(
                    "Matrix re-login OK worker=%s device=%s",
                    self.worker_name,
                    new_device,
                )
                console.print(
                    f"[green]Matrix re-login OK[/green] "
                    f"(device: {new_device})"
                )
                self._health.update(
                    "matrix",
                    "healthy",
                    "matrix re-login succeeded",
                    {
                        "operation": "matrix_relogin",
                        "device_id": new_device,
                    },
                )
            else:
                logger.warning(
                    "Matrix re-login returned no token worker=%s device=%s",
                    self.worker_name,
                    new_device,
                )
                console.print(
                    "[yellow]Matrix re-login returned no token, "
                    "using existing access token[/yellow]"
                )
                self._health.update(
                    "matrix",
                    "unhealthy",
                    "matrix re-login returned no access token",
                    {
                        "operation": "matrix_relogin",
                        "device_id": new_device,
                    },
                )
        except Exception as exc:
            logger.exception("Matrix re-login failed worker=%s", self.worker_name)
            console.print(
                f"[yellow]Matrix re-login failed: {exc} — "
                f"using existing access token (E2EE may not work)[/yellow]"
            )
            self._health.update(
                "matrix",
                "unhealthy",
                f"matrix re-login failed: {exc}",
                {
                    "operation": "matrix_relogin",
                    "error_type": type(exc).__name__,
                },
            )

        return openclaw_cfg

    def _notify_matrix(self, message: str, openclaw_cfg: dict) -> None:
        """Best-effort send a m.notice to all joined Matrix rooms.

        Uses the raw Matrix CS API (urllib) since the nio client is not yet
        running at startup time.  Accepts pending room invitations first so
        that brand-new workers that have not yet joined any room still
        receive the notification.
        """
        import json
        import urllib.request
        import uuid

        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        from .bridge import _port_remap, _is_in_container
        homeserver = _port_remap(
            matrix_cfg.get("homeserver", ""), _is_in_container()
        )
        access_token = matrix_cfg.get("accessToken", "")

        if not homeserver or not access_token:
            logger.debug("notify_matrix skipped: missing homeserver or token")
            return

        headers = {"Authorization": f"Bearer {access_token}"}

        rooms = self._wait_for_matrix_rooms(homeserver, headers)
        if not rooms:
            logger.warning(
                "notify_matrix: no rooms available after waiting, "
                "notification skipped worker=%s",
                self.worker_name,
            )
            return

        body = json.dumps({
            "msgtype": "m.notice",
            "body": message,
        }).encode("utf-8")

        for room_id in rooms:
            txn_id = uuid.uuid4().hex
            url = (
                f"{homeserver}/_matrix/client/v3/rooms/"
                f"{urllib.request.quote(room_id, safe='')}"
                f"/send/m.room.message/{txn_id}"
            )
            try:
                req = urllib.request.Request(
                    url,
                    data=body,
                    headers={**headers, "Content-Type": "application/json"},
                    method="PUT",
                )
                urllib.request.urlopen(req, timeout=10)
            except Exception as exc:
                logger.debug(
                    "notify_matrix: failed to send to %s: %s", room_id, exc
                )

    def _wait_for_matrix_rooms(
        self,
        homeserver: str,
        headers: dict[str, str],
        *,
        timeout: float = 120,
        poll_interval: float = 3,
    ) -> list[str]:
        """Wait until the worker has at least one joined Matrix room.

        On each poll cycle: accept pending invites, then check joined_rooms.
        Returns the room list, or [] after *timeout* seconds.
        """
        import json
        import time
        import urllib.request

        deadline = time.monotonic() + timeout

        while True:
            self._accept_matrix_invites(homeserver, headers)

            try:
                req = urllib.request.Request(
                    f"{homeserver}/_matrix/client/v3/joined_rooms",
                    headers=headers,
                    method="GET",
                )
                with urllib.request.urlopen(req, timeout=10) as resp:
                    rooms = json.loads(resp.read()).get("joined_rooms", [])
            except Exception as exc:
                logger.debug("notify_matrix: failed to list joined rooms: %s", exc)
                rooms = []

            if rooms:
                return rooms

            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return []

            logger.info(
                "notify_matrix: no rooms yet, retrying in %.0fs "
                "(%.0fs remaining) worker=%s",
                poll_interval,
                remaining,
                self.worker_name,
            )
            time.sleep(min(poll_interval, remaining))

    def _accept_matrix_invites(
        self,
        homeserver: str,
        headers: dict[str, str],
    ) -> None:
        """Accept all pending Matrix room invitations via initial sync."""
        import json
        import urllib.request

        sync_filter = json.dumps(
            {"room": {"timeline": {"limit": 0}, "state": {"limit": 0}}}
        )
        sync_url = (
            f"{homeserver}/_matrix/client/v3/sync"
            f"?filter={urllib.request.quote(sync_filter)}&timeout=0"
        )
        try:
            req = urllib.request.Request(sync_url, headers=headers, method="GET")
            with urllib.request.urlopen(req, timeout=15) as resp:
                sync_data = json.loads(resp.read())
        except Exception as exc:
            logger.debug("notify_matrix: sync for invites failed: %s", exc)
            return

        invited = sync_data.get("rooms", {}).get("invite", {})
        if not invited:
            return

        logger.info(
            "notify_matrix: accepting %d pending room invite(s) worker=%s",
            len(invited),
            self.worker_name,
        )
        for room_id in invited:
            join_url = (
                f"{homeserver}/_matrix/client/v3/join/"
                f"{urllib.request.quote(room_id, safe='')}"
            )
            try:
                req = urllib.request.Request(
                    join_url,
                    data=b"{}",
                    headers={**headers, "Content-Type": "application/json"},
                    method="POST",
                )
                urllib.request.urlopen(req, timeout=10)
            except Exception as exc:
                logger.debug(
                    "notify_matrix: failed to join %s: %s", room_id, exc
                )

    # ------------------------------------------------------------------
    # mc (MinIO Client) auto-install
    # ------------------------------------------------------------------

    def _ensure_mc(self) -> None:
        """Ensure mc (MinIO Client) binary is available on PATH.

        If not found, downloads the latest release from dl.min.io and installs
        it to ~/.local/bin/mc (created if needed, added to PATH for this process).
        """
        if shutil.which("mc"):
            logger.debug("mc already available")
            return

        system = platform.system().lower()   # linux / darwin
        machine = platform.machine().lower() # x86_64 / aarch64 / arm64

        arch_map = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}
        arch = arch_map.get(machine, machine)

        if system == "windows":
            url = "https://dl.min.io/client/mc/release/windows-amd64/mc.exe"
            install_dir = Path.home() / ".local" / "bin"
            install_dir.mkdir(parents=True, exist_ok=True)
            dest = install_dir / "mc.exe"
        elif system in ("linux", "darwin"):
            url = f"https://dl.min.io/client/mc/release/{system}-{arch}/mc"
            install_dir = Path.home() / ".local" / "bin"
            install_dir.mkdir(parents=True, exist_ok=True)
            dest = install_dir / "mc"
        else:
            logger.warning("mc auto-install not supported system=%s", system)
            console.print(f"[yellow]mc auto-install not supported on {system}, please install mc manually[/yellow]")
            return

        console.print(f"[yellow]mc not found, downloading from {url}...[/yellow]")
        logger.warning("mc not found; downloading worker=%s url=%s dest=%s", self.worker_name, url, dest)
        try:
            import httpx
            with httpx.stream("GET", url, follow_redirects=True, timeout=60) as resp:
                resp.raise_for_status()
                with open(dest, "wb") as f:
                    for chunk in resp.iter_bytes(chunk_size=65536):
                        f.write(chunk)
            if system != "windows":
                dest.chmod(dest.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
            os.environ["PATH"] = str(install_dir) + os.pathsep + os.environ.get("PATH", "")
            console.print(f"[green]mc installed to {dest}[/green]")
            logger.info("mc installed worker=%s dest=%s", self.worker_name, dest)
        except Exception as exc:
            logger.exception("mc auto-install failed worker=%s url=%s dest=%s", self.worker_name, url, dest)
            console.print(f"[yellow]mc auto-install failed: {exc}. Please install mc manually.[/yellow]")
