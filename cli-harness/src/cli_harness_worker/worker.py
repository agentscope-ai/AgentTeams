"""CLI-harness worker lifecycle.

Bootstrap flow (mirrors hermes-worker):

  1. Mirror the worker's MinIO prefix to local disk (openclaw.json, SOUL.md,
     AGENTS.md, skills/, shared/).
  2. Re-login to Matrix with the password kept in MinIO (fresh access token).
  3. Select the CLI adapter for this runtime and export provider env.
  4. Start background pull/push loops against MinIO.
  5. Run the Matrix loop: each inbound task message is dispatched to the CLI
     in headless mode and the output is sent back to the room.
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import urllib.request
from pathlib import Path
from typing import Dict, Optional

from rich.console import Console

from cli_harness_worker.adapters import get_adapter
from cli_harness_worker import bridge
from cli_harness_worker.config import WorkerConfig
from cli_harness_worker.matrix import MatrixLoop, attach_callbacks
from cli_harness_worker.sync import FileSync, push_loop, sync_loop

console = Console()
logger = logging.getLogger(__name__)


class Worker:
    """Owns the lifecycle of one CLI-harness worker process."""

    def __init__(self, config: WorkerConfig) -> None:
        self.config = config
        self.worker_name = config.worker_name
        self.sync: Optional[FileSync] = None
        self._matrix_loop: Optional[MatrixLoop] = None
        self._bg_tasks: list[asyncio.Task] = []
        self._stopping = False

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    async def run(self) -> None:
        if not await self.start():
            return
        try:
            assert self._matrix_loop is not None
            await self._matrix_loop.run()
        except asyncio.CancelledError:
            pass
        finally:
            await self.stop()

    async def stop(self) -> None:
        if self._stopping:
            return
        self._stopping = True
        console.print("[yellow]Stopping CLI-harness worker...[/yellow]")
        if self._matrix_loop:
            try:
                await self._matrix_loop.close()
            except Exception:
                logger.exception("Matrix loop close failed")
        for task in self._bg_tasks:
            task.cancel()
        console.print("[green]CLI-harness worker stopped.[/green]")

    # ------------------------------------------------------------------
    # Startup
    # ------------------------------------------------------------------

    async def start(self) -> bool:
        console.print(
            f"[bold green]CLI-harness Worker[/bold green]\n"
            f"Worker: [cyan]{self.worker_name}[/cyan]\n"
            f"Runtime: [cyan]{self.config.runtime}[/cyan]\n"
            f"Workspace: [cyan]{self.config.workspace_dir}[/cyan]"
        )

        self.sync = FileSync(
            endpoint=self.config.minio_endpoint,
            access_key=self.config.minio_access_key,
            secret_key=self.config.minio_secret_key,
            bucket=self.config.minio_bucket,
            worker_name=self.worker_name,
            secure=self.config.minio_secure,
            local_dir=self.config.workspace_dir,
        )

        openclaw_cfg: Optional[Dict] = None
        max_attempts = 12
        for attempt in range(1, max_attempts + 1):
            try:
                self.sync.mirror_all()
                openclaw_cfg = self.sync.get_config()
                break
            except Exception as exc:
                if attempt >= max_attempts:
                    console.print(
                        f"[red]Failed to read worker config from MinIO: {exc}[/red]"
                    )
                    return False
                logger.warning(
                    "Worker config not ready yet (attempt %s/%s): %s",
                    attempt, max_attempts, exc,
                )
                await asyncio.sleep(5)

        assert openclaw_cfg is not None

        matrix_cfg = bridge.resolve_matrix_config(openclaw_cfg)
        homeserver = matrix_cfg.get("homeserver", "")
        user_id = matrix_cfg.get("userId", "") or f"@{self.worker_name}:"
        access_token = matrix_cfg.get("accessToken", "")
        own_room_id = os.environ.get("AGENTTEAMS_WORKER_ROOM_ID", "")
        password = self.sync.get_matrix_password()
        if password:
            password = password.strip()
            refreshed = self._matrix_relogin(homeserver, password)
            if refreshed:
                access_token = refreshed

        # Provider env for the CLI adapter (base URL / API key / model).
        adapter = get_adapter(self.config.runtime, workspace=str(self.config.workspace_dir))
        provider_env = bridge.adapter_env(openclaw_cfg, adapter)
        os.environ.update(provider_env)
        if not adapter.available():
            console.print(
                f"[red]CLI binary {adapter.binary!r} not found on PATH[/red]"
            )
            return False

        self.config.harness_home.mkdir(parents=True, exist_ok=True)
        store_path = str(self.config.harness_home / "matrix-store")
        Path(store_path).mkdir(parents=True, exist_ok=True)

        self._matrix_loop = attach_callbacks(
            MatrixLoop(
                homeserver=homeserver,
                user_id=user_id,
                access_token=access_token,
                own_room_id=own_room_id,
                on_task=self._on_task,
                password=password,
                store_path=store_path,
            )
        )
        await self._matrix_loop.login()
        await self._matrix_loop.join_own_room()

        self._bg_tasks.append(asyncio.create_task(
            sync_loop(
                self.sync,
                interval=self.config.sync_interval,
                on_pull=self._on_files_pulled,
            )
        ))
        self._bg_tasks.append(asyncio.create_task(
            push_loop(self.sync, check_interval=5)
        ))

        # Readiness marker consumed by the entrypoint's report-ready loop.
        ready_marker = self.config.harness_home / "ready"
        ready_marker.write_text(
            json.dumps({"runtime": self.config.runtime, "binary": adapter.binary})
        )

        console.print("[bold green]CLI-harness worker initialized.[/bold green]")
        return True

    # ------------------------------------------------------------------
    # Task dispatch
    # ------------------------------------------------------------------

    async def _on_task(self, room_id: str, sender: str, body: str) -> str:
        assert self.sync is not None
        # Best-effort refresh of Manager-managed files so the CLI sees the
        # latest skills/config before it runs.
        try:
            await asyncio.get_event_loop().run_in_executor(
                None, self.sync.pull_all
            )
        except Exception as exc:
            logger.warning("Pre-task pull failed (non-fatal): %s", exc)

        soul = self._read_text("SOUL.md")
        prompt = bridge.build_prompt(body, soul=soul)

        adapter = get_adapter(self.config.runtime, workspace=str(self.config.workspace_dir))
        result = await adapter.run(prompt)
        if result.timed_out:
            return f"task timed out; partial output:\n\n{result.output}"
        if not result.ok and not result.output:
            return f"CLI exited with code {result.exit_code} and no output"
        return result.output

    async def _on_files_pulled(self, changed: list[str]) -> None:
        logger.info("Manager-managed files updated: %s", changed)

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _read_text(self, name: str) -> Optional[str]:
        assert self.sync is not None
        path = self.sync.local_dir / name
        if not path.exists():
            return None
        try:
            return path.read_text(errors="replace")
        except OSError:
            return None

    def _matrix_relogin(self, homeserver: str, password: str) -> Optional[str]:
        """Password re-login; returns a fresh access token on success."""
        if not homeserver or not password:
            return None
        login_url = f"{homeserver}/_matrix/client/v3/login"
        body = json.dumps({
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": self.worker_name},
            "password": password,
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
        except Exception as exc:
            console.print(
                f"[yellow]Matrix re-login failed: {exc} — using existing token[/yellow]"
            )
            return None
        token = payload.get("access_token", "")
        if token:
            console.print(
                f"[green]Matrix re-login OK (device={payload.get('device_id', '')})[/green]"
            )
        return token or None
