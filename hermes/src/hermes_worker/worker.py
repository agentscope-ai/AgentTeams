"""Hermes Worker main entry point.

Bootstrap flow (mirrors copaw_worker.worker.Worker):

  1. Ensure ``mc`` (MinIO Client) is on PATH (auto-download on first run).
  2. Mirror the worker's MinIO prefix to local disk so we have openclaw.json,
     SOUL.md, AGENTS.md, skills/, etc. available.
  3. Re-login to Matrix (writes a fresh access_token + device_id back into
     openclaw.json) so E2EE keeps working across restarts.
  4. Bridge openclaw.json → ``${HERMES_HOME}/{config.yaml,.env,SOUL.md,AGENTS.md}``.
  5. Mirror skill directories from MinIO into ``${HERMES_HOME}/skills/``.
  6. Start background pull/push loops against MinIO.
  7. Hand off to ``gateway.run.start_gateway`` which:
       * loads ``HERMES_HOME/config.yaml``
       * spins up the Matrix adapter (our overlay; see ``hermes_matrix.adapter``)
       * starts the agent loop and runs until cancelled.
"""
from __future__ import annotations

import asyncio
import logging
import os
import platform
import shutil
import stat
from pathlib import Path
from typing import Any, Dict, Optional

from rich.console import Console
from rich.panel import Panel

from hermes_worker.bridge import (
    _is_in_container,
    _port_remap,
    bridge_openclaw_to_hermes,
)
from hermes_worker.config import WorkerConfig
from hermes_worker.sync import FileSync, push_loop, sync_loop

console = Console()
logger = logging.getLogger(__name__)


class Worker:
    """Owns the lifecycle of one hermes worker process."""

    def __init__(self, config: WorkerConfig) -> None:
        self.config = config
        self.worker_name = config.worker_name
        self.sync: Optional[FileSync] = None
        self._hermes_home: Path = config.hermes_home
        self._gateway_task: Optional[asyncio.Task] = None
        self._stopping = False

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def run(self) -> None:
        if not await self.start():
            return
        try:
            await self._run_hermes_gateway()
        except asyncio.CancelledError:
            pass
        finally:
            await self.stop()

    async def stop(self) -> None:
        if self._stopping:
            return
        self._stopping = True
        console.print("[yellow]Stopping hermes worker...[/yellow]")
        if self._gateway_task and not self._gateway_task.done():
            self._gateway_task.cancel()
            try:
                await self._gateway_task
            except (asyncio.CancelledError, Exception):
                pass
        console.print("[green]Hermes worker stopped.[/green]")

    # ------------------------------------------------------------------
    # Startup
    # ------------------------------------------------------------------

    async def start(self) -> bool:
        console.print(
            Panel.fit(
                f"[bold green]Hermes Worker[/bold green]\n"
                f"Worker: [cyan]{self.worker_name}[/cyan]\n"
                f"HERMES_HOME: [cyan]{self._hermes_home}[/cyan]",
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
            secure=self.config.minio_secure,
            local_dir=self.config.workspace_dir,
        )

        console.print("[yellow]Pulling all files from MinIO...[/yellow]")
        try:
            self.sync.mirror_all()
        except Exception as exc:
            console.print(f"[red]Failed to mirror from MinIO: {exc}[/red]")
            return False

        try:
            openclaw_cfg = self.sync.get_config()
        except Exception as exc:
            console.print(f"[red]Failed to read openclaw.json: {exc}[/red]")
            return False

        # Refresh Matrix credentials (E2EE relies on a fresh device_id).
        openclaw_cfg = self._matrix_relogin(openclaw_cfg)

        # When we run on the host (dev) and the FS endpoint includes a port,
        # use that port as the gateway port as well so the bridge's _port_remap
        # rewrites container-internal :8080 references correctly.
        if not os.environ.get("AGENTTEAMS_PORT_GATEWAY"):
            from urllib.parse import urlparse
            parsed = urlparse(self.config.minio_endpoint)
            if parsed.port:
                os.environ["AGENTTEAMS_PORT_GATEWAY"] = str(parsed.port)

        self._hermes_home.mkdir(parents=True, exist_ok=True)
        os.environ["HERMES_HOME"] = str(self._hermes_home)

        console.print("[yellow]Bridging openclaw.json → hermes config...[/yellow]")
        try:
            soul = self._read_text_file(self.sync.local_dir / "SOUL.md") or ""
            agents = self._read_text_file(self.sync.local_dir / "AGENTS.md") or ""
            bridge_openclaw_to_hermes(
                openclaw_cfg, self._hermes_home,
                soul=soul or None, agents_md=agents or None,
            )
        except Exception as exc:
            console.print(f"[red]Bridge failed: {exc}[/red]")
            return False

        # Make the bridge-generated .env visible to start_gateway() and the
        # spawned platform adapters in this *same* Python process.
        self._load_env_file(self._hermes_home / ".env")

        self._sync_skills()
        self._copy_mcporter_config()

        asyncio.create_task(
            sync_loop(
                self.sync,
                interval=self.config.sync_interval,
                on_pull=self._on_files_pulled,
            )
        )
        asyncio.create_task(push_loop(self.sync, check_interval=5))

        console.print("[bold green]Hermes worker initialized.[/bold green]")
        return True

    # ------------------------------------------------------------------
    # Hermes gateway runner
    # ------------------------------------------------------------------

    async def _run_hermes_gateway(self) -> None:
        # Imports are deferred so that earlier setup (env, HERMES_HOME) is
        # already in place when hermes-agent's modules read it at import time.
        from gateway.config import load_gateway_config
        from gateway.run import start_gateway

        gw_config = load_gateway_config()
        console.print(
            f"[bold green]Starting hermes gateway "
            f"(home={self._hermes_home})[/bold green]"
        )
        self._gateway_task = asyncio.create_task(
            start_gateway(gw_config, replace=False, verbosity=0)
        )
        try:
            await self._gateway_task
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            console.print(f"[red]hermes gateway crashed: {exc}[/red]")
            raise

    # ------------------------------------------------------------------
    # Matrix re-login (mirrors copaw_worker)
    # ------------------------------------------------------------------

    def _matrix_relogin(self, openclaw_cfg: Dict[str, Any]) -> Dict[str, Any]:
        """Re-login to Matrix with the password kept in MinIO.

        The Manager publishes ``credentials/matrix/password`` for each worker
        and rotates it as needed.  Logging in from scratch issues a *new*
        ``device_id`` and access token, which keeps the per-restart Olm
        identity rotation invisible to other clients (Element Web).
        """
        import json
        import urllib.error
        import urllib.request

        if self.sync is None:
            return openclaw_cfg

        password_key = f"{self.sync._prefix}/credentials/matrix/password"
        matrix_password = self.sync._cat(password_key)
        if not matrix_password:
            console.print(
                "[dim]No Matrix password in MinIO; skipping re-login "
                "(E2EE may not survive restart).[/dim]"
            )
            return openclaw_cfg

        matrix_password = matrix_password.strip()
        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        homeserver = _port_remap(
            matrix_cfg.get("homeserver", ""), _is_in_container()
        )
        if not homeserver or not matrix_password:
            return openclaw_cfg

        login_url = f"{homeserver}/_matrix/client/v3/login"
        body = json.dumps({
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": self.worker_name},
            "password": matrix_password,
        }).encode()

        try:
            req = urllib.request.Request(
                login_url,
                data=body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                payload = json.loads(resp.read())
        except (urllib.error.URLError, ValueError, TimeoutError) as exc:
            console.print(
                f"[yellow]Matrix re-login failed: {exc} — "
                f"using existing token (E2EE may not work).[/yellow]"
            )
            return openclaw_cfg

        new_token = payload.get("access_token", "")
        new_device = payload.get("device_id", "")
        if not new_token:
            console.print(
                "[yellow]Matrix re-login returned no token; keeping current.[/yellow]"
            )
            return openclaw_cfg

        openclaw_cfg.setdefault("channels", {}).setdefault("matrix", {})
        openclaw_cfg["channels"]["matrix"]["accessToken"] = new_token
        if new_device:
            openclaw_cfg["channels"]["matrix"]["deviceId"] = new_device

        # Persist back to disk so subsequent re-bridge picks up the new token.
        config_path = self.sync.local_dir / "openclaw.json"
        try:
            with open(config_path, "w", encoding="utf-8") as fp:
                json.dump(openclaw_cfg, fp, indent=2, ensure_ascii=False)
        except OSError as exc:
            logger.warning("Failed to persist updated openclaw.json: %s", exc)

        console.print(
            f"[green]Matrix re-login OK[/green] "
            f"(device={new_device}, token={new_token[:10]}...)"
        )
        return openclaw_cfg

    # ------------------------------------------------------------------
    # mc (MinIO Client) auto-install
    # ------------------------------------------------------------------

    def _ensure_mc(self) -> None:
        """Install ``mc`` to ~/.local/bin if it isn't already on PATH."""
        if shutil.which("mc"):
            return

        system = platform.system().lower()
        machine = platform.machine().lower()
        arch_map = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}
        arch = arch_map.get(machine, machine)

        if system == "windows":
            url = "https://dl.min.io/client/mc/release/windows-amd64/mc.exe"
            install_dir = Path.home() / ".local" / "bin"
            dest = install_dir / "mc.exe"
        elif system in ("linux", "darwin"):
            url = f"https://dl.min.io/client/mc/release/{system}-{arch}/mc"
            install_dir = Path.home() / ".local" / "bin"
            dest = install_dir / "mc"
        else:
            console.print(
                f"[yellow]mc auto-install not supported on {system}; "
                f"please install mc manually.[/yellow]"
            )
            return

        install_dir.mkdir(parents=True, exist_ok=True)
        console.print(f"[yellow]mc not found, downloading from {url}...[/yellow]")
        try:
            import httpx
            with httpx.stream("GET", url, follow_redirects=True, timeout=60) as resp:
                resp.raise_for_status()
                with open(dest, "wb") as fp:
                    for chunk in resp.iter_bytes(chunk_size=65536):
                        fp.write(chunk)
            if system != "windows":
                dest.chmod(
                    dest.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH
                )
            os.environ["PATH"] = (
                str(install_dir) + os.pathsep + os.environ.get("PATH", "")
            )
            console.print(f"[green]mc installed to {dest}[/green]")
        except Exception as exc:
            console.print(
                f"[yellow]mc auto-install failed: {exc}. "
                f"Please install mc manually.[/yellow]"
            )

    # ------------------------------------------------------------------
    # Skills sync — mirror MinIO into ${HERMES_HOME}/skills/
    # ------------------------------------------------------------------

    def _sync_skills(self) -> None:
        if self.sync is None:
            return
        skills_dir = self._hermes_home / "skills"
        skills_dir.mkdir(parents=True, exist_ok=True)

        skill_names = self.sync.list_skills()
        if not skill_names:
            logger.info("No skills published in MinIO for %s", self.worker_name)

        installed: list[str] = []
        for name in skill_names:
            src_dir = self.sync.local_dir / "skills" / name
            dst_dir = skills_dir / name
            if not src_dir.exists():
                continue
            dst_dir.mkdir(parents=True, exist_ok=True)
            for src_file in src_dir.rglob("*"):
                if not src_file.is_file():
                    continue
                rel = src_file.relative_to(src_dir)
                dst_file = dst_dir / rel
                dst_file.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(src_file, dst_file)
                if dst_file.suffix == ".sh":
                    dst_file.chmod(dst_file.stat().st_mode | 0o111)
            installed.append(name)

        if installed:
            console.print(f"[green]Skills installed: {', '.join(installed)}[/green]")

        # Drop stale skills that are no longer published by the Manager so
        # they don't leak into the agent's tool list.
        keep = set(installed) | {"file-sync"}
        for child in list(skills_dir.iterdir()):
            if child.is_dir() and child.name not in keep:
                try:
                    shutil.rmtree(child)
                    logger.info("Removed stale hermes skill: %s", child.name)
                except OSError as exc:
                    logger.debug("Could not remove %s: %s", child, exc)

    # ------------------------------------------------------------------
    # mcporter config copy
    # ------------------------------------------------------------------

    def _copy_mcporter_config(self) -> None:
        """Mirror the Manager-managed mcporter config into HERMES_HOME.

        Hermes-agent doesn't ship its own mcporter, but the Manager publishes
        ``config/mcporter.json`` for workers that opt-in via skills.  Place a
        copy in the worker's home so any tool that walks ``./config/`` finds it.
        """
        if self.sync is None:
            return
        src = self.sync.local_dir / "config" / "mcporter.json"
        if not src.exists():
            return
        dst = self._hermes_home / "config" / "mcporter.json"
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)
        logger.info("mcporter config copied to %s", dst)

    # ------------------------------------------------------------------
    # File sync callback
    # ------------------------------------------------------------------

    async def _on_files_pulled(self, pulled_files: list[str]) -> None:
        """React to Manager-side file changes by re-bridging only when needed."""
        if self.sync is None:
            return

        if any(f.startswith("skills/") for f in pulled_files):
            self._sync_skills()
        if "config/mcporter.json" in pulled_files:
            self._copy_mcporter_config()

        if "openclaw.json" not in pulled_files:
            return

        console.print("[yellow]openclaw.json changed; re-bridging...[/yellow]")
        try:
            openclaw_cfg = self.sync.get_config()
            soul = self._read_text_file(self.sync.local_dir / "SOUL.md")
            agents = self._read_text_file(self.sync.local_dir / "AGENTS.md")
            bridge_openclaw_to_hermes(
                openclaw_cfg, self._hermes_home, soul=soul, agents_md=agents,
            )
            self._load_env_file(self._hermes_home / ".env")
            console.print(
                "[green]Re-bridge complete; restart the gateway to apply "
                "settings that aren't hot-reloadable.[/green]"
            )
        except Exception as exc:
            console.print(f"[red]Re-bridge failed: {exc}[/red]")

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _read_text_file(path: Path) -> Optional[str]:
        try:
            return path.read_text() if path.exists() else None
        except OSError:
            return None

    @staticmethod
    def _load_env_file(env_path: Path) -> None:
        """Source ``env_path`` into ``os.environ`` for this process."""
        if not env_path.exists():
            return
        try:
            for raw in env_path.read_text(errors="replace").splitlines():
                line = raw.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, _, val = line.partition("=")
                key = key.strip()
                val = val.strip()
                if (val.startswith('"') and val.endswith('"')) or (
                    val.startswith("'") and val.endswith("'")
                ):
                    val = val[1:-1]
                    val = val.replace('\\"', '"').replace("\\\\", "\\")
                os.environ[key] = val
        except OSError as exc:
            logger.warning("Could not source %s: %s", env_path, exc)
