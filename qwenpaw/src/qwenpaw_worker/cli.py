"""CLI entry point: qwenpaw-worker."""

from __future__ import annotations

import asyncio
from pathlib import Path
import signal
from typing import Optional

import typer

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.log import configure_worker_logging
from qwenpaw_worker.worker import Worker


def main() -> None:
    """Entry point registered in pyproject.toml."""

    def _run(
        name: str = typer.Option(..., "--name", help="Worker name"),
        cr_name: Optional[str] = typer.Option(None, "--cr-name", help="Worker CR name"),
        fs: str = typer.Option(..., "--fs", help="MinIO/OSS endpoint"),
        fs_key: str = typer.Option(..., "--fs-key", help="MinIO/OSS access key"),
        fs_secret: str = typer.Option(..., "--fs-secret", help="MinIO/OSS secret key"),
        fs_bucket: str = typer.Option("agentteams-storage", "--fs-bucket", help="Storage bucket"),
        install_dir: Optional[str] = typer.Option(None, "--install-dir", help="Base install dir"),
        storage_prefix: Optional[str] = typer.Option(None, "--storage-prefix", help="Storage prefix"),
        shared_prefix: Optional[str] = typer.Option(None, "--shared-prefix", help="Shared storage prefix"),
        runtime_config: Optional[str] = typer.Option(None, "--runtime-config", help="Local runtime.yaml path"),
        console_port: int = typer.Option(8088, "--console-port", help="QwenPaw API port"),
    ) -> None:
        """Start the QwenPaw Worker."""
        config = WorkerConfig(
            worker_name=name,
            worker_cr_name=cr_name,
            fs_endpoint=fs,
            fs_access_key=fs_key,
            fs_secret_key=fs_secret,
            fs_bucket=fs_bucket,
            install_dir=Path(install_dir) if install_dir else None,
            storage_prefix=storage_prefix,
            shared_prefix=shared_prefix,
            runtime_config_path=Path(runtime_config) if runtime_config else None,
            console_port=console_port,
        )
        configure_worker_logging(config.qwenpaw_working_dir)
        worker = Worker(config)

        async def _async_run() -> None:
            loop = asyncio.get_running_loop()

            def _shutdown() -> None:
                asyncio.create_task(worker.stop())

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
