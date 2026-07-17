"""Background sync daemon entrypoint for bash worker runtimes."""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys

from agentteams_sync.contract import OPENCLAW, RUNTIME_CONTRACTS
from agentteams_sync.openclaw import FileSync as OpenClawFileSync
from agentteams_sync.openclaw import run_openclaw_daemon

logger = logging.getLogger(__name__)


def _configure_logging() -> None:
    level_name = os.environ.get("AGENTTEAMS_SYNC_LOG_LEVEL", "INFO").upper()
    level = getattr(logging, level_name, logging.INFO)
    logging.basicConfig(
        level=level,
        format="[agentteams-sync %(asctime)s] %(message)s",
        datefmt="%Y-%m-%d %H:%M:%S",
    )


def _resolve_fs_credentials() -> tuple[str, str, str, str]:
    runtime = os.environ.get("AGENTTEAMS_RUNTIME", "")
    bucket = os.environ.get("AGENTTEAMS_FS_BUCKET", "agentteams-storage")
    if runtime == "aliyun":
        return (
            os.environ.get("AGENTTEAMS_FS_ENDPOINT", "https://oss-placeholder.aliyuncs.com"),
            "rrsa",
            "rrsa",
            bucket,
        )
    endpoint = os.environ.get("AGENTTEAMS_FS_ENDPOINT")
    access_key = os.environ.get("AGENTTEAMS_FS_ACCESS_KEY")
    secret_key = os.environ.get("AGENTTEAMS_FS_SECRET_KEY")
    if not endpoint or not access_key or not secret_key:
        raise SystemExit(
            "AGENTTEAMS_FS_ENDPOINT, AGENTTEAMS_FS_ACCESS_KEY, and "
            "AGENTTEAMS_FS_SECRET_KEY are required for sync daemon"
        )
    return endpoint, access_key, secret_key, bucket


def _build_openclaw_sync() -> OpenClawFileSync:
    worker_name = os.environ.get("AGENTTEAMS_WORKER_NAME")
    if not worker_name:
        raise SystemExit("AGENTTEAMS_WORKER_NAME is required")
    endpoint, access_key, secret_key, bucket = _resolve_fs_credentials()
    return OpenClawFileSync(
        endpoint=endpoint,
        access_key=access_key,
        secret_key=secret_key,
        bucket=bucket,
        worker_name=worker_name,
    )


async def _run_contract(contract: str) -> None:
    if contract != "openclaw":
        supported = ", ".join(sorted(RUNTIME_CONTRACTS))
        raise SystemExit(
            f"Unsupported sync contract {contract!r}; daemon supports: openclaw "
            f"(other contracts use runtime shims: {supported})"
        )
    sync = _build_openclaw_sync()
    logger.info(
        "Starting OpenClaw sync daemon (push=%ss pull=%ss) for worker %s",
        OPENCLAW.push_check_interval_seconds,
        OPENCLAW.pull_interval_seconds,
        sync.worker_name,
    )
    await run_openclaw_daemon(sync)


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description="AgentTeams background file sync daemon")
    parser.add_argument(
        "command",
        choices=["daemon"],
        help="Run background sync loops",
    )
    parser.add_argument(
        "--contract",
        required=True,
        choices=sorted(RUNTIME_CONTRACTS),
        help="SyncContract preset to honor",
    )
    args = parser.parse_args(argv)
    _configure_logging()
    if args.command != "daemon":
        parser.error(f"unknown command {args.command!r}")
    try:
        asyncio.run(_run_contract(args.contract))
    except KeyboardInterrupt:
        logger.info("Sync daemon interrupted")
        sys.exit(0)


if __name__ == "__main__":
    main()
