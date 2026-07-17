"""
Worker main entry point.

Bootstrap flow:
1. Pull openclaw.json + SOUL.md + AGENTS.md from MinIO
2. Bridge openclaw.json -> CoPaw config.json + providers.json
3. Install MatrixChannel into CoPaw's custom_channels dir
4. Start CoPaw AgentRunner + ChannelManager (Matrix channel)
"""
from __future__ import annotations

import asyncio
import logging
import os
import platform
import shutil
import stat
from pathlib import Path
from typing import Optional

from rich.console import Console
from rich.panel import Panel

from copaw_worker.config import WorkerConfig
from copaw_worker.controller_report import ControllerReadyReporter, run_controller_ready_loop
from copaw_worker.llm_usage import configure_llm_usage_from_openclaw, install_llm_usage_hooks
from copaw_worker.sync import FileSync, sync_loop, push_loop
from copaw_worker.matrix_bootstrap import MatrixBootstrapClient
from copaw_worker.workspace_layout import WorkspaceLayout
from copaw_worker.worker_api import WorkerAPIServer
from copaw_worker.health import HealthState, check_matrix_service

console = Console()
logger = logging.getLogger(__name__)


class Worker:
    def __init__(self, config: WorkerConfig) -> None:
        self.config = config
        self.worker_name = config.worker_name
        self.sync: Optional[FileSync] = None
        self._layout: Optional[WorkspaceLayout] = None
        self._runner = None
        self._channel_manager = None
        self._bg_tasks: list[asyncio.Task] = []

    @property
    def _copaw_working_dir(self) -> Optional[Path]:
        return self._layout.copaw_working_dir if self._layout else None

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def run(self) -> bool:
        if not await self.start():
            return False
        try:
            await self._run_copaw()
        except asyncio.CancelledError:
            pass
        finally:
            await self.stop()
        return True

    async def stop(self) -> None:
        console.print("[yellow]Stopping worker...[/yellow]")
        await self._stop_bg_tasks()
        if self._channel_manager is not None:
            try:
                await self._channel_manager.stop_all()
            except Exception:
                pass
        if self._runner is not None:
            try:
                await self._runner.stop()
            except Exception:
                pass
        console.print("[green]Worker stopped.[/green]")

    # ------------------------------------------------------------------
    # Background task supervision
    # ------------------------------------------------------------------

    def _spawn_bg_task(self, coro, *, name: str) -> asyncio.Task:
        """Create a supervised background task: tracked on self._bg_tasks and
        logged (not silently swallowed) if it raises."""
        task = asyncio.create_task(coro, name=name)
        self._bg_tasks.append(task)

        def _on_done(t: asyncio.Task) -> None:
            if t.cancelled():
                return
            exc = t.exception()
            if exc is not None:
                logger.error("background task %r failed", name, exc_info=exc)

        task.add_done_callback(_on_done)
        return task

    async def _stop_bg_tasks(self) -> None:
        tasks = [t for t in self._bg_tasks if not t.done()]
        for t in tasks:
            t.cancel()
        for t in tasks:
            try:
                await t
            except asyncio.CancelledError:
                pass
            except Exception:
                logger.exception("background task %r raised during shutdown", t.get_name())
        self._bg_tasks.clear()

    # ------------------------------------------------------------------
    # Startup
    # ------------------------------------------------------------------

    async def start(self) -> bool:
        console.print(
            Panel.fit(
                f"[bold green]CoPaw Worker[/bold green]\n"
                f"Worker: [cyan]{self.worker_name}[/cyan]",
                title="Starting",
            )
        )

        self._ensure_mc()

        self.sync = FileSync(
            endpoint=self.config.minio_endpoint,
            access_key=self.config.minio_access_key,
            secret_key=self.config.minio_secret_key,
            bucket=self.config.minio_bucket,
            worker_name=self.worker_name,
            worker_cr_name=self.config.worker_cr_name,
            secure=self.config.minio_secure,
            local_dir=self.config.install_dir / self.worker_name,
        )

        openclaw_cfg = await self._mirror_until_config_ready()
        if openclaw_cfg is None:
            return False

        matrix_bootstrap = MatrixBootstrapClient(
            self.sync, worker_name=self.worker_name
        )
        openclaw_cfg = matrix_bootstrap.relogin(openclaw_cfg)
        matrix_bootstrap.join_pending_invites(openclaw_cfg)

        self._layout = WorkspaceLayout.for_sync(self.sync, profile="worker")

        console.print("[yellow]Bridging configuration to CoPaw...[/yellow]")
        try:
            self._layout.materialize(openclaw_cfg, bootstrap=True)
            self._layout.sync_skills(
                list_skills=self.sync.list_skills,
                worker_name=self.worker_name,
            )
        except Exception as exc:
            console.print(f"[red]Config bridge failed: {exc}[/red]")
            return False

        install_llm_usage_hooks()
        configure_llm_usage_from_openclaw(openclaw_cfg)

        self._spawn_bg_task(
            sync_loop(
                self.sync,
                interval=self.config.sync_interval,
                on_pull=self._on_files_pulled,
            ),
            name="sync_loop",
        )
        self._spawn_bg_task(push_loop(self.sync, check_interval=5), name="push_loop")

        console.print("[bold green]Worker initialized.[/bold green]")
        if self.config.console_port:
            console.print(
                f"[dim]Note: web console enabled on port {self.config.console_port} "
                f"(~500MB extra RAM). Remove --console-port to save memory.[/dim]"
            )
        else:
            console.print(
                "[dim]Tip: add --console-port 8088 to enable the web console "
                "(costs ~500MB extra RAM).[/dim]"
            )
        return True

    async def _mirror_until_config_ready(self) -> Optional[dict]:
        max_attempts = 12
        for attempt in range(1, max_attempts + 1):
            console.print("[yellow]Pulling all files from MinIO...[/yellow]")
            try:
                self.sync.mirror_all()
                return self.sync.get_config()
            except Exception as exc:
                if attempt >= max_attempts:
                    console.print(
                        f"[red]Failed to read worker config from MinIO: {exc}[/red]"
                    )
                    return None
                logger.warning(
                    "Worker config not ready yet (attempt %s/%s): %s",
                    attempt,
                    max_attempts,
                    exc,
                )
                await asyncio.sleep(5)
        return None

    # ------------------------------------------------------------------
    # CoPaw runner
    # ------------------------------------------------------------------

    async def _run_copaw(self) -> None:
        """Start CoPaw. If console_port is set, run the full FastAPI app via
        uvicorn (gives access to the web console). Otherwise start the runner
        and channel manager directly (lightweight, no HTTP server)."""
        from copaw_worker.hooks import install_tool_hooks

        install_tool_hooks()
        self._spawn_controller_ready_loop()
        if self.config.console_port:
            await self._run_copaw_with_console(self.config.console_port)
        else:
            await self._run_copaw_headless()

    async def _run_copaw_with_console(self, port: int) -> None:
        """Run CoPaw's full FastAPI app (runner + channels + web console)."""
        import uvicorn
        from copaw.app.channels.registry import clear_builtin_channel_cache

        clear_builtin_channel_cache()

        worker_port = self.config.worker_port or (port + 1)
        health_state = HealthState(
            self._copaw_working_dir / "health.json"
        )

        async def _liveness():
            snap = health_state.snapshot()
            return {"liveness": "alive", "healthiness": snap.healthiness}

        async def _readiness():
            for comp in ("sync", "bridge", "model"):
                health_state.update(comp, "healthy", "validated at startup")

            import socket as _socket

            def _probe_console() -> None:
                with _socket.create_connection(("127.0.0.1", port), timeout=3):
                    pass

            try:
                await asyncio.to_thread(_probe_console)
                health_state.update("copaw", "healthy", f"console reachable on port {port}")
            except Exception as e:
                health_state.update("copaw", "unhealthy", f"console unreachable: {e}")

            matrix_cfg = {}
            try:
                cfg_path = self.sync.local_dir / "openclaw.json"
                if cfg_path.exists():
                    import json as _json
                    matrix_cfg = _json.loads(cfg_path.read_text()).get("channels", {}).get("matrix", {})
            except Exception:
                pass
            homeserver = matrix_cfg.get("homeserver", "")
            if homeserver:
                mx_health = await asyncio.to_thread(check_matrix_service, homeserver, timeout=5)
                health_state.update("matrix", mx_health.healthiness, mx_health.message)

            snap = health_state.snapshot()
            return {
                "readiness": "ready" if snap.healthiness == "healthy" else "not_ready",
                "healthiness": snap.healthiness,
                "message": snap.message,
                "components": {
                    k: {"healthiness": v.healthiness, "message": v.message}
                    for k, v in snap.components.items()
                },
            }

        api_server = WorkerAPIServer(
            host="0.0.0.0",
            port=worker_port,
            liveness_handler=_liveness,
            readiness_handler=_readiness,
        )
        await api_server.start()

        uv_config = uvicorn.Config(
            "copaw.app._app:app",
            host="0.0.0.0",
            port=port,
            log_level="info",
        )
        server = uvicorn.Server(uv_config)
        console.print(
            f"[bold green]CoPaw console available at "
            f"http://127.0.0.1:{port}/[/bold green]"
        )
        try:
            await server.serve()
        except asyncio.CancelledError:
            server.should_exit = True
        finally:
            await api_server.stop()

    async def _run_copaw_headless(self) -> None:
        """Start CoPaw's AgentRunner + ChannelManager (no HTTP server)."""
        from copaw.app.runner.runner import AgentRunner
        from copaw.config.utils import load_config
        from copaw.app.channels.manager import ChannelManager
        from copaw.app.channels.utils import make_process_from_runner
        from copaw.app.channels.registry import clear_builtin_channel_cache

        clear_builtin_channel_cache()

        self._runner = AgentRunner()
        await self._runner.start()

        config = load_config()
        self._channel_manager = ChannelManager.from_config(
            process=make_process_from_runner(self._runner),
            config=config,
            on_last_dispatch=None,
        )
        await self._channel_manager.start_all()

        console.print("[bold green]CoPaw channels started. Worker is running.[/bold green]")

        try:
            while True:
                await asyncio.sleep(60)
        except asyncio.CancelledError:
            pass
        finally:
            await self._channel_manager.stop_all()
            await self._runner.stop()
            self._channel_manager = None
            self._runner = None

    # ------------------------------------------------------------------
    # mc (MinIO Client) auto-install
    # ------------------------------------------------------------------

    def _ensure_mc(self) -> None:
        """Ensure mc (MinIO Client) binary is available on PATH."""
        if shutil.which("mc"):
            logger.debug("mc already available")
            return

        system = platform.system().lower()
        machine = platform.machine().lower()

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
            console.print(
                f"[yellow]mc auto-install not supported on {system}, "
                f"please install mc manually[/yellow]"
            )
            return

        console.print(f"[yellow]mc not found, downloading from {url}...[/yellow]")
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
        except Exception as exc:
            console.print(
                f"[yellow]mc auto-install failed: {exc}. "
                f"Please install mc manually.[/yellow]"
            )

    # ------------------------------------------------------------------
    # File sync callback
    # ------------------------------------------------------------------

    def _spawn_controller_ready_loop(self) -> None:
        reporter = ControllerReadyReporter.from_env(self.config.worker_cr_name)
        if not reporter.enabled():
            return
        self._spawn_bg_task(
            run_controller_ready_loop(
                worker_name=self.config.worker_cr_name,
                reporter=reporter,
            ),
            name="controller_ready_loop",
        )

    async def _on_files_pulled(self, pulled_files: list[str]) -> None:
        """Re-bridge config when Manager-managed files change (openclaw.json)."""
        if self._layout is None:
            return

        if any(f.startswith("skills/") for f in pulled_files):
            self._layout.sync_skills(
                list_skills=self.sync.list_skills,
                worker_name=self.worker_name,
            )

        if "config/mcporter.json" in pulled_files:
            self._layout.copy_mcporter_config()

        if "openclaw.json" not in pulled_files:
            return

        console.print("[yellow]Config changed, re-bridging...[/yellow]")
        try:
            openclaw_cfg = self.sync.get_config()
            self._layout.rebridge(openclaw_cfg)
            configure_llm_usage_from_openclaw(openclaw_cfg)
            console.print("[green]Config re-bridged.[/green]")
        except Exception as exc:
            console.print(f"[red]Re-bridge failed: {exc}[/red]")
