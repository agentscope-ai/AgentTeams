"""CLI entrypoints for the OpenHuman Worker bridge."""
from __future__ import annotations

import argparse
import logging
import os
import subprocess
import sys
from pathlib import Path

from openhuman_worker.bridge import bridge_openclaw_file, write_config_toml

logger = logging.getLogger(__name__)


def _configure_logging(verbose: bool) -> None:
    level = logging.DEBUG if verbose else logging.INFO
    logging.basicConfig(level=level, format="%(levelname)s: %(message)s")


def _apply_llm_settings(workspace: Path, llm) -> None:
    env = os.environ.copy()
    env["OPENHUMAN_CONFIG"] = str(workspace / "config.toml")
    subprocess.run(
        [
            "openhuman-core",
            "config",
            "update_model_settings",
            "--inference_url",
            llm.base_url,
            "--api_key",
            llm.api_key,
            "--default_model",
            llm.default_model,
        ],
        check=False,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
    )


def cmd_bridge(args: argparse.Namespace) -> int:
    workspace = Path(args.workspace).expanduser()
    openclaw_json = Path(args.openclaw_json).expanduser()
    result = bridge_openclaw_file(openclaw_json)
    config_path = write_config_toml(workspace, result)
    logger.info("Wrote %s", config_path)

    if result.llm is None:
        logger.error(
            "LLM gateway not configured (AGENTTEAMS_AI_GATEWAY_URL or "
            "AGENTTEAMS_WORKER_GATEWAY_KEY missing). Refusing to start."
        )
        return 1

    logger.info(
        "Configuring LLM: endpoint=%s model=%s",
        result.llm.base_url,
        result.llm.default_model,
    )
    _apply_llm_settings(workspace, result.llm)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="openhuman-worker")
    parser.add_argument("-v", "--verbose", action="store_true")
    sub = parser.add_subparsers(dest="command", required=True)

    bridge = sub.add_parser(
        "bridge",
        help="Bridge openclaw.json into config.toml and apply LLM settings",
    )
    bridge.add_argument(
        "--workspace",
        required=True,
        help="OpenHuman workspace directory (config.toml destination)",
    )
    bridge.add_argument(
        "--openclaw-json",
        required=True,
        help="Path to openclaw.json pulled from MinIO",
    )
    bridge.set_defaults(func=cmd_bridge)
    return parser


def main(argv: list[str] | None = None) -> None:
    parser = build_parser()
    args = parser.parse_args(argv)
    _configure_logging(args.verbose)
    raise SystemExit(args.func(args))


if __name__ == "__main__":
    main()
