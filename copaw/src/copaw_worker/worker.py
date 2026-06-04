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
from copaw_worker.sync import FileSync, sync_loop, push_loop
from copaw_worker.bridge import bridge_openclaw_to_copaw

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

        # 1. Ensure mc (MinIO Client) is available
        self._ensure_mc()

        # 2. Init file sync
        self.sync = FileSync(
            endpoint=self.config.minio_endpoint,
            access_key=self.config.minio_access_key,
            secret_key=self.config.minio_secret_key,
            bucket=self.config.minio_bucket,
            worker_name=self.worker_name,
            secure=self.config.minio_secure,
            local_dir=self.config.install_dir / self.worker_name,
        )

        # 2. Full mirror from MinIO (restore all state: config, sessions, sync token, etc.)
        #    Mirrors the OpenClaw worker's startup approach: pull everything first,
        #    then use selective sync during runtime.
        console.print("[yellow]Pulling all files from MinIO...[/yellow]")
        try:
            self.sync.mirror_all()
        except Exception as exc:
            console.print(f"[red]Failed to mirror from MinIO: {exc}[/red]")
            return False

        # 3. Parse openclaw.json (already on disk after mirror_all)
        try:
            openclaw_cfg = self.sync.get_config()
        except Exception as exc:
            console.print(f"[red]Failed to read config: {exc}[/red]")
            return False

        # 3b. Re-login to Matrix to get fresh access token + device ID
        #     Under E2EE, reusing the old access token (same device_id) with a
        #     regenerated identity key causes other clients to reject key
        #     distribution. Re-login creates a new device_id, matching the
        #     Manager's behavior.
        openclaw_cfg = self._matrix_relogin(openclaw_cfg)

        # 4. Set up CoPaw working directory
        self._copaw_working_dir = self.config.install_dir / self.worker_name / ".copaw"
        self._copaw_working_dir.mkdir(parents=True, exist_ok=True)

        # Write SOUL.md / AGENTS.md into CoPaw working dir (read from local copies pulled by mirror_all)
        for name in ("SOUL.md", "AGENTS.md"):
            src = self.sync.local_dir / name
            if src.exists():
                (self._copaw_working_dir / name).write_text(src.read_text())

        # 5. Bridge openclaw.json -> CoPaw config.json + providers.json
        #    Infer gateway port from FS endpoint so bridge's _port_remap uses
        #    the correct host port instead of the hardcoded default.
        if not os.environ.get("HICLAW_PORT_GATEWAY"):
            from urllib.parse import urlparse
            _parsed = urlparse(self.config.minio_endpoint)
            if _parsed.port:
                os.environ["HICLAW_PORT_GATEWAY"] = str(_parsed.port)

        console.print("[yellow]Bridging configuration to CoPaw...[/yellow]")
        try:
            bridge_openclaw_to_copaw(openclaw_cfg, self._copaw_working_dir)
        except Exception as exc:
            console.print(f"[red]Config bridge failed: {exc}[/red]")
            return False

        # 6. Copy mcporter config into CoPaw working dir so mcporter finds
        #    ./config/mcporter.json when running from COPAW_WORKING_DIR
        self._copy_mcporter_config()

        # 7. Install MatrixChannel into CoPaw's custom_channels dir
        self._install_matrix_channel()

        # 8. Sync skills from MinIO into CoPaw's active_skills dir
        self._sync_skills()

        # 9. Start background MinIO sync
        asyncio.create_task(
            sync_loop(
                self.sync,
                interval=self.config.sync_interval,
                on_pull=self._on_files_pulled,
            )
        )
        # Local -> Remote: change-triggered push (every 5s, mirrors openclaw worker behavior)
        asyncio.create_task(push_loop(self.sync, check_interval=5))

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

    # ------------------------------------------------------------------
    # CoPaw runner
    # ------------------------------------------------------------------

    async def _run_copaw(self) -> None:
        """Start CoPaw. If console_port is set, run the full FastAPI app via
        uvicorn (gives access to the web console). Otherwise start the runner
        and channel manager directly (lightweight, no HTTP server)."""
        if self.config.console_port:
            await self._run_copaw_with_console(self.config.console_port)
        else:
            await self._run_copaw_headless()

    async def _run_copaw_with_console(self, port: int) -> None:
        """Run CoPaw's full FastAPI app (runner + channels + web console)."""
        import uvicorn
        from copaw.app.channels.registry import clear_builtin_channel_cache

        clear_builtin_channel_cache()

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

    async def _run_copaw_headless(self) -> None:
        """Start CoPaw's AgentRunner + ChannelManager (no HTTP server)."""
        from copaw.app.runner.runner import AgentRunner
        from copaw.config.utils import load_config
        from copaw.app.channels.manager import ChannelManager
        from copaw.app.channels.utils import make_process_from_runner
        from copaw.app.channels.registry import clear_builtin_channel_cache

        # Force registry reload so newly installed matrix_channel.py is picked up
        clear_builtin_channel_cache()

        self._runner = AgentRunner()
        await self._runner.start()

        # load_config reads COPAW_WORKING_DIR/config.json (set by bridge.py)
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
            # Clear refs so stop() doesn't double-call
            self._channel_manager = None
            self._runner = None

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
            console.print(
                "[dim]No Matrix password found in MinIO, skipping re-login "
                "(E2EE may not work after restart)[/dim]"
            )
            return openclaw_cfg

        matrix_password = matrix_password.strip()
        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        from .bridge import _port_remap, _is_in_container
        homeserver = _port_remap(
            matrix_cfg.get("homeserver", ""), _is_in_container()
        )

        if not homeserver or not matrix_password:
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
                console.print(
                    f"[green]Matrix re-login OK[/green] "
                    f"(device: {new_device}, token: {new_token[:10]}...)"
                )
            else:
                console.print(
                    "[yellow]Matrix re-login returned no token, "
                    "using existing access token[/yellow]"
                )
        except Exception as exc:
            console.print(
                f"[yellow]Matrix re-login failed: {exc} — "
                f"using existing access token (E2EE may not work)[/yellow]"
            )

        return openclaw_cfg

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
            console.print(f"[yellow]mc auto-install not supported on {system}, please install mc manually[/yellow]")
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
            console.print(f"[yellow]mc auto-install failed: {exc}. Please install mc manually.[/yellow]")

    # ------------------------------------------------------------------
    # Skills sync
    # ------------------------------------------------------------------

    def _sync_skills(self) -> None:
        """Pull skills from MinIO and install into CoPaw's active_skills dir.

        First seeds all CoPaw built-in skills (pdf, xlsx, docx, etc.) as a base
        layer, then overlays skills pushed from MinIO by the Manager (which take
        precedence and can override built-ins).
        """
        active_skills_dir = self._copaw_working_dir / "active_skills"
        active_skills_dir.mkdir(parents=True, exist_ok=True)

        # 0. Remove stale customized_skills that duplicate builtins.
        #    After an upgrade the new CoPaw image may ship builtins (pdf, pptx …)
        #    that were previously only available as customized copies.  If the old
        #    customized_skills/ directory persists on disk, CoPaw loads both the
        #    builtin AND the customized copy, causing duplicates in the UI.
        self._dedup_customized_skills()

        # 1. Seed CoPaw built-in skills as base layer.
        # bridge.py has already patched copaw.constant.ACTIVE_SKILLS_DIR to point
        # here, so sync_skills_to_working_dir() writes to the correct directory.
        try:
            from copaw.agents.skills_manager import sync_skills_to_working_dir
            synced, skipped = sync_skills_to_working_dir(skill_names=None, force=False)
            logger.info(
                "Seeded CoPaw built-in skills: %d installed, %d already existed",
                synced, skipped,
            )
        except Exception as exc:
            logger.warning("Failed to seed CoPaw built-in skills: %s", exc)

        # 2. Overlay with Manager-pushed skills from MinIO (higher priority).
        skill_names = self.sync.list_skills()
        if not skill_names:
            logger.info("No extra skills in MinIO for worker %s", self.worker_name)

        for skill_name in skill_names:
            src_skill_dir = self.sync.local_dir / "skills" / skill_name
            dst_skill_dir = active_skills_dir / skill_name
            if not src_skill_dir.exists():
                continue
            dst_skill_dir.mkdir(parents=True, exist_ok=True)
            # Mirror the full skill directory (SKILL.md + scripts/ + references/)
            for src_file in src_skill_dir.rglob("*"):
                if not src_file.is_file():
                    continue
                rel = src_file.relative_to(src_skill_dir)
                dst_file = dst_skill_dir / rel
                dst_file.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(src_file, dst_file)
                # Restore +x on shell scripts
                if dst_file.suffix == ".sh":
                    dst_file.chmod(dst_file.stat().st_mode | 0o111)
            logger.info("Installed MinIO skill: %s", skill_name)

        if skill_names:
            console.print(f"[green]Skills installed: {', '.join(skill_names)}[/green]")

        # 3. Remove stale skills from active_skills/ that are no longer in MinIO
        #    and are not CoPaw builtins.
        try:
            import copaw.agents.skills as _skills_pkg
            builtin_skills_root = Path(_skills_pkg.__file__).resolve().parent
            builtin_names = {
                c.name for c in builtin_skills_root.iterdir()
                if c.is_dir() and not c.name.startswith("_")
            }
        except (ImportError, AttributeError):
            builtin_names = set()

        keep_names = builtin_names | set(skill_names) | {"file-sync"}
        for child in list(active_skills_dir.iterdir()):
            if child.is_dir() and child.name not in keep_names:
                shutil.rmtree(child)
                logger.info("Removed stale active skill: %s", child.name)

    def _dedup_customized_skills(self) -> None:
        """Remove customized skills that shadow CoPaw builtins.

        CoPaw discovers skills from two independent directories:
          - builtin:     <site-packages>/copaw/agents/skills/<name>/
          - customized:  <working_dir>/customized_skills/<name>/

        After an upgrade, new builtins may overlap with stale customized copies
        left over from a previous version.  This method detects the overlap and
        removes the customized copy so only the (newer) builtin is loaded.
        """
        customized_dir = self._copaw_working_dir / "customized_skills"
        if not customized_dir.is_dir():
            return

        # Collect builtin skill names from the installed copaw package
        try:
            import copaw.agents.skills as _skills_pkg
            builtin_skills_root = Path(_skills_pkg.__file__).resolve().parent
        except (ImportError, AttributeError):
            return

        builtin_names: set[str] = set()
        if builtin_skills_root.is_dir():
            for child in builtin_skills_root.iterdir():
                if child.is_dir() and not child.name.startswith("_"):
                    builtin_names.add(child.name)

        if not builtin_names:
            return

        # Remove customized copies that duplicate a builtin
        import shutil
        for child in list(customized_dir.iterdir()):
            if child.is_dir() and child.name in builtin_names:
                shutil.rmtree(child)
                logger.info(
                    "Removed stale customized skill '%s' (now a builtin)",
                    child.name,
                )

    # ------------------------------------------------------------------
    # MatrixChannel installation
    # ------------------------------------------------------------------

    def _install_matrix_channel(self) -> None:
        """Copy matrix_channel.py into COPAW_WORKING_DIR/custom_channels/.

        CoPaw's CUSTOM_CHANNELS_DIR = WORKING_DIR / "custom_channels", and
        WORKING_DIR is read from COPAW_WORKING_DIR env var at import time.
        We set COPAW_WORKING_DIR in bridge.py before this runs, so the
        directory is already correct.
        """
        custom_channels_dir = self._copaw_working_dir / "custom_channels"
        custom_channels_dir.mkdir(parents=True, exist_ok=True)
        src = Path(__file__).parent / "matrix_channel.py"
        dst = custom_channels_dir / "matrix_channel.py"
        shutil.copy2(src, dst)
        logger.debug("MatrixChannel installed to %s", dst)

    # ------------------------------------------------------------------
    # mcporter config
    # ------------------------------------------------------------------

    def _copy_mcporter_config(self) -> None:
        """Copy mcporter config from workspace root into CoPaw working dir.

        pull_all writes to <local_dir>/config/mcporter.json (workspace root),
        but mcporter looks for ./config/mcporter.json relative to cwd, which
        is COPAW_WORKING_DIR (.copaw/). Copy it there so mcporter finds it.
        """
        src = self.sync.local_dir / "config" / "mcporter.json"
        if not src.exists():
            return
        dst = self._copaw_working_dir / "config" / "mcporter.json"
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)
        logger.info("mcporter config copied to %s", dst)

    # ------------------------------------------------------------------
    # File sync callback
    # ------------------------------------------------------------------

    async def _on_files_pulled(self, pulled_files: list[str]) -> None:
        """Re-bridge config when Manager-managed files change (openclaw.json).
        SOUL.md, AGENTS.md are Worker-managed and not pulled; use local copies."""
        # Re-sync skills if any skill file changed
        if any(f.startswith("skills/") for f in pulled_files):
            self._sync_skills()

        # Copy mcporter config into CoPaw working dir when it changes
        if "config/mcporter.json" in pulled_files:
            self._copy_mcporter_config()

        needs_rebridge = "openclaw.json" in pulled_files
        if not needs_rebridge:
            return

        console.print("[yellow]Config changed, re-bridging...[/yellow]")
        try:
            openclaw_cfg = self.sync.get_config()
            # Use local Worker-managed files; fallback to MinIO for initial bootstrap
            soul = (self.sync.local_dir / "SOUL.md").read_text() if (self.sync.local_dir / "SOUL.md").exists() else self.sync.get_soul()
            agents = (self.sync.local_dir / "AGENTS.md").read_text() if (self.sync.local_dir / "AGENTS.md").exists() else self.sync.get_agents_md()

            if soul:
                (self._copaw_working_dir / "SOUL.md").write_text(soul)
            if agents:
                (self._copaw_working_dir / "AGENTS.md").write_text(agents)

            bridge_openclaw_to_copaw(openclaw_cfg, self._copaw_working_dir)
            console.print("[green]Config re-bridged.[/green]")
        except Exception as exc:
            console.print(f"[red]Re-bridge failed: {exc}[/red]")
