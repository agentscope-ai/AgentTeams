"""Bootstrap CoPaw Manager workspace then launch the CoPaw app."""

from __future__ import annotations

import argparse
import logging
import os
import sys
from pathlib import Path

from copaw_worker.manager_bootstrap import (
    configure_cms_plugin,
    configure_manager_dm_rooms,
    load_openclaw_json,
    materialize_manager_workspace,
    maybe_archive_legacy_manager_config,
    start_dm_room_refresh_watcher,
    start_openclaw_json_watcher,
)
from copaw_worker.run_copaw_app import main as run_copaw_app_main

logger = logging.getLogger(__name__)


def bootstrap_manager_runtime(
    *,
    standard_dir: Path | None = None,
    copaw_working_dir: Path | None = None,
    openclaw_json: Path | None = None,
    watch_openclaw_json: bool = True,
) -> Path:
    """Materialize Manager CoPaw workspace; return copaw working dir."""
    home = standard_dir or Path(os.environ.get("HOME", "/root/manager-workspace"))
    copaw_dir = copaw_working_dir or Path(
        os.environ.get("COPAW_WORKING_DIR", home / ".copaw")
    )
    openclaw_path = openclaw_json or (home / "openclaw.json")

    copaw_dir.mkdir(parents=True, exist_ok=True)
    (copaw_dir / "custom_channels").mkdir(parents=True, exist_ok=True)
    (copaw_dir / ".secret").mkdir(parents=True, exist_ok=True)

    if not openclaw_path.is_file():
        raise FileNotFoundError(f"openclaw.json not found at {openclaw_path}")

    maybe_archive_legacy_manager_config(copaw_dir)

    openclaw_cfg = load_openclaw_json(openclaw_path)
    logger.info("Bridging openclaw.json -> CoPaw config (manager profile)")
    materialize_manager_workspace(home, copaw_dir, openclaw_cfg)

    configure_manager_dm_rooms(copaw_dir)
    start_dm_room_refresh_watcher(copaw_dir)
    configure_cms_plugin(home)

    os.environ["COPAW_WORKING_DIR"] = str(copaw_dir)
    if watch_openclaw_json:
        start_openclaw_json_watcher(openclaw_path, copaw_dir, home)

    return copaw_dir


def main(argv: list[str] | None = None) -> None:
    logging.basicConfig(
        level=getattr(
            logging,
            os.environ.get("COPAW_LOG_LEVEL", "info").upper(),
            logging.INFO,
        )
    )

    parser = argparse.ArgumentParser(
        prog="python -m copaw_worker.run_manager_app",
        description="Bootstrap CoPaw Manager workspace and run the CoPaw app.",
    )
    parser.add_argument(
        "--no-watch",
        action="store_true",
        help="Disable background openclaw.json re-bridge watcher",
    )
    parser.add_argument(
        "copaw_args",
        nargs=argparse.REMAINDER,
        help="Arguments forwarded to copaw app (default: app --host 0.0.0.0 --port 18799)",
    )
    args = parser.parse_args(argv)

    bootstrap_manager_runtime(watch_openclaw_json=not args.no_watch)

    copaw_args = args.copaw_args
    if not copaw_args:
        copaw_args = ["app", "--host", "0.0.0.0", "--port", "18799"]
    elif copaw_args[0] == "--":
        copaw_args = copaw_args[1:]

    sys.argv = ["copaw_worker.run_copaw_app", *copaw_args]
    logger.info("Starting CoPaw Manager (app mode)")
    run_copaw_app_main()


if __name__ == "__main__":
    main()
