"""CLI entry point: ``harness-worker``."""
from __future__ import annotations

import asyncio
import logging
import signal
from pathlib import Path
from typing import Optional

import typer

from harness_worker.config import WorkerConfig
from harness_worker.worker import Worker

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)


def main() -> None:
    def _run(
        name: str = typer.Option(..., "--name", help="Worker name"),
        fs: str = typer.Option(..., "--fs", help="MinIO endpoint"),
        fs_key: str = typer.Option(..., "--fs-key", help="MinIO access key"),
        fs_secret: str = typer.Option(..., "--fs-secret", help="MinIO secret key"),
        fs_bucket: str = typer.Option("hiclaw-storage", "--fs-bucket", help="MinIO bucket"),
        sync_interval: int = typer.Option(300, "--sync-interval", help="Sync interval (seconds)"),
        install_dir: Optional[str] = typer.Option(None, "--install-dir", help="Base install dir"),
        harness_type: str = typer.Option("claude", "--harness-type", help="Harness CLI: claude|gemini|opencode|codex"),
    ) -> None:
        config = WorkerConfig(
            worker_name=name,
            minio_endpoint=fs,
            minio_access_key=fs_key,
            minio_secret_key=fs_secret,
            minio_bucket=fs_bucket,
            sync_interval=sync_interval,
            install_dir=Path(install_dir) if install_dir else None,
            harness_type=harness_type,
        )
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


if __name__ == "__main__":
    main()