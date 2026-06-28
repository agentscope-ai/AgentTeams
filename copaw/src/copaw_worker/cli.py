"""CLI entry point: copaw-worker"""
from __future__ import annotations

import asyncio
import logging
import signal
from pathlib import Path
from typing import Optional

import typer

from copaw_worker.config import WorkerConfig
from copaw_worker.worker import Worker

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)


def main() -> None:
    """Entry point registered in pyproject.toml."""

    def _run(
        name: str = typer.Option(..., "--name", help="Worker name"),
        fs: str = typer.Option(..., "--fs", help="MinIO endpoint"),
        fs_key: str = typer.Option(..., "--fs-key", help="MinIO access key"),
        fs_secret: str = typer.Option(..., "--fs-secret", help="MinIO secret key"),
        fs_bucket: str = typer.Option("hiclaw-storage", "--fs-bucket", help="MinIO bucket"),
        sync_interval: int = typer.Option(300, "--sync-interval", help="Sync interval (seconds)"),
        install_dir: Optional[str] = typer.Option(None, "--install-dir", help="Base install dir"),
        console_port: Optional[int] = typer.Option(None, "--console-port", help="Enable web console on this port (e.g. 8088, costs ~500MB extra RAM)"),
    ) -> None:
        """Start the CoPaw Worker and connect to Matrix."""
        config = WorkerConfig(
            worker_name=name,
            minio_endpoint=fs,
            minio_access_key=fs_key,
            minio_secret_key=fs_secret,
            minio_bucket=fs_bucket,
            sync_interval=sync_interval,
            install_dir=Path(install_dir) if install_dir else None,
            console_port=console_port,
        )
        worker = Worker(config)

        async def _async_run() -> None:
            loop = asyncio.get_running_loop()

            def _shutdown() -> None:
                asyncio.create_task(worker.stop())

            # Windows ProactorEventLoop does not support add_signal_handler;
            # fall back to KeyboardInterrupt handling below.
            try:
                for sig in (signal.SIGINT, signal.SIGTERM):
                    loop.add_signal_handler(sig, _shutdown)
            except NotImplementedError:
                pass

            await worker.run()

        try:
            asyncio.run(_async_run())
        except KeyboardInterrupt:
            pass

    typer.run(_run)
