"""Command-line entry point for the host-local Codex Worker bridge."""

from __future__ import annotations

import argparse
import json
import logging
import os
from pathlib import Path
import signal
import subprocess
import sys
from typing import Sequence

from .codex_client import CodexAppServer, CodexError, resolve_codex_command
from .config import ConfigError, RuntimeConfig
from .matrix import MatrixClient, MatrixError
from .security import Redactor
from .worker import CodexWorkerBridge, StateStore


def _teamharness_root() -> Path:
    return Path(__file__).resolve().parents[3]


def _state_dir(config: RuntimeConfig) -> Path:
    override = os.getenv("AGENTTEAMS_CODEX_WORKER_STATE_DIR", "").strip()
    if override:
        return Path(override).expanduser().resolve()
    return (Path.home() / ".agentteams" / "codex-worker" / config.member_name).resolve()


def _common_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--runtime-config", required=True, type=Path)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--plugin-root", type=Path, default=_teamharness_root())
    parser.add_argument("--codex-command", default="codex")
    parser.add_argument("--state-dir", type=Path)
    return parser


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="agentteams-codex-worker")
    subparsers = parser.add_subparsers(dest="command", required=True)
    doctor = subparsers.add_parser("doctor", parents=[_common_parser()])
    doctor.add_argument("--skip-matrix", action="store_true")
    run = subparsers.add_parser("run", parents=[_common_parser()])
    run.add_argument("--handshake-timeout", type=float, default=90.0)
    run.add_argument("--turn-timeout", type=float, default=1800.0)
    run.add_argument("--sync-timeout-ms", type=int, default=30000)
    run.add_argument("--once", action="store_true")
    run.add_argument("--verbose", action="store_true")
    return parser


def _load(args: argparse.Namespace) -> tuple[RuntimeConfig, Path, Path, str]:
    config = RuntimeConfig.from_path(args.runtime_config.resolve())
    workspace = args.workspace.resolve()
    plugin_root = args.plugin_root.resolve()
    if not workspace.is_dir():
        raise ConfigError(f"workspace is not a directory: {workspace}")
    if not (plugin_root / "mcp" / "server.py").is_file():
        raise ConfigError(f"TeamHarness MCP server not found under {plugin_root}")
    token = os.getenv(config.matrix_token_env, "")
    return config, workspace, plugin_root, token


def doctor(args: argparse.Namespace) -> int:
    config, workspace, plugin_root, token = _load(args)
    checks: list[dict[str, object]] = []
    command = resolve_codex_command(args.codex_command)
    version = subprocess.run(
        [command, "--version"],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=20,
        check=False,
    )
    checks.append({"name": "codex", "ok": version.returncode == 0, "detail": version.stdout.strip()})
    login = subprocess.run(
        [command, "login", "status"],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=20,
        check=False,
    )
    checks.append({"name": "codex-login", "ok": login.returncode == 0, "detail": login.stdout.strip()})
    checks.append({"name": "workspace", "ok": workspace.is_dir(), "detail": str(workspace)})
    checks.append({"name": "teamharness", "ok": True, "detail": str(plugin_root)})
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").strip()
    if args.skip_matrix:
        checks.append({"name": "matrix", "ok": True, "detail": "skipped by request"})
    else:
        checks.append(
            {
                "name": "matrix-token",
                "ok": bool(token),
                "detail": f"environment variable {config.matrix_token_env} is " + ("set" if token else "missing"),
            }
        )
        checks.append({"name": "matrix-url", "ok": bool(homeserver), "detail": homeserver or "missing"})
    if not args.skip_matrix and token and homeserver:
        matrix = MatrixClient(homeserver, token, config.matrix_user_id)
        actual = matrix.whoami()
        checks.append(
            {
                "name": "matrix-identity",
                "ok": actual == config.matrix_user_id,
                "detail": actual or "empty user id",
            }
        )
    print(json.dumps({"ok": all(bool(item["ok"]) for item in checks), "checks": checks}, indent=2))
    return 0 if all(bool(item["ok"]) for item in checks) else 1


def run(args: argparse.Namespace) -> int:
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    config, workspace, plugin_root, token = _load(args)
    homeserver = os.getenv("AGENTTEAMS_MATRIX_URL", "").strip()
    if not homeserver or not token:
        raise ConfigError(
            f"AGENTTEAMS_MATRIX_URL and {config.matrix_token_env} are required"
        )
    os.environ.setdefault("AGENTTEAMS_MATRIX_USER_ID", config.matrix_user_id)
    os.environ.setdefault("AGENTTEAMS_WORKER_NAME", config.member_name)
    os.environ.setdefault("AGENTTEAMS_AGENT_ROLE", "remote-member")
    os.environ.setdefault("AGENTTEAMS_AGENT_HOME", str(workspace))
    os.environ["AGENTTEAMS_WORKER_MATRIX_TOKEN"] = token
    os.environ.setdefault("TEAMHARNESS_RUNTIME_CONFIG", str(args.runtime_config.resolve()))
    os.environ.setdefault("TEAMHARNESS_SHARED_DIR", str(workspace / "shared"))

    redactor = Redactor([token])
    matrix = MatrixClient(homeserver, token, config.matrix_user_id)
    codex = CodexAppServer(
        codex_command=args.codex_command,
        mcp_server=plugin_root / "mcp" / "server.py",
        handshake_timeout=args.handshake_timeout,
        turn_timeout=args.turn_timeout,
        secret_values=[token],
    )
    state = StateStore((args.state_dir or _state_dir(config)).resolve())
    bridge = CodexWorkerBridge(
        config=config,
        workspace=workspace,
        plugin_root=plugin_root,
        matrix=matrix,
        codex=codex,
        state=state,
        redactor=redactor,
    )

    def stop_handler(*_: object) -> None:
        bridge.stop()

    signal.signal(signal.SIGINT, stop_handler)
    if hasattr(signal, "SIGTERM"):
        signal.signal(signal.SIGTERM, stop_handler)
    try:
        if args.once:
            bridge.sync_once(timeout_ms=args.sync_timeout_ms)
        else:
            bridge.run_forever(sync_timeout_ms=args.sync_timeout_ms)
    finally:
        bridge.stop()
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return doctor(args) if args.command == "doctor" else run(args)
    except (ConfigError, CodexError, MatrixError, OSError, subprocess.SubprocessError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
