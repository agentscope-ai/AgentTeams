"""CLI entry point: ``harness-remote`` (local-environment harness worker).

Two modes:

  harness-remote [run-options]   # run the remote worker (default, no subcommand)
  harness-remote attach          # attach an interactive `claude` session to the
                                 # worker's current conversation (REPL slash commands)

Config is **env-backed**: every option binds to its ``HICLAW_*`` env var so the
documented environment contract works without a container entrypoint. Flags
override env. Unlike the in-cluster ``harness-worker``, the default install dir
is local (``~/.hiclaw/agents``).
"""
from __future__ import annotations

import asyncio
import logging
import os
import signal
from pathlib import Path
from typing import Optional

import typer

from harness_worker.config import WorkerConfig
from harness_worker.remote_worker import RemoteWorker
from harness_worker.worker import Worker

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)

app = typer.Typer(
    add_completion=False,
    help="HiClaw remote (local-environment) harness worker.",
    no_args_is_help=False,
)


def _default_install_dir() -> Path:
    env = os.environ.get("HICLAW_INSTALL_DIR")
    if env:
        return Path(env).expanduser()
    return Path.home() / ".hiclaw" / "agents"


@app.callback(invoke_without_command=True)
def run(
    ctx: typer.Context,
    name: Optional[str] = typer.Option(None, "--name", envvar="HICLAW_WORKER_NAME", help="Worker identity"),
    fs: Optional[str] = typer.Option(None, "--fs", envvar="HICLAW_FS_ENDPOINT", help="MinIO endpoint (externally reachable)"),
    fs_key: Optional[str] = typer.Option(None, "--fs-key", envvar="HICLAW_FS_ACCESS_KEY", help="MinIO access key"),
    fs_secret: Optional[str] = typer.Option(None, "--fs-secret", envvar="HICLAW_FS_SECRET_KEY", help="MinIO secret key"),
    fs_bucket: str = typer.Option("hiclaw-storage", "--fs-bucket", envvar="HICLAW_FS_BUCKET", help="MinIO bucket"),
    fs_secure: bool = typer.Option(False, "--fs-secure", envvar="HICLAW_FS_SECURE", help="Use TLS for MinIO"),
    matrix_domain: Optional[str] = typer.Option(None, "--matrix-domain", envvar="HICLAW_MATRIX_DOMAIN", help="Matrix homeserver domain"),
    matrix_homeserver: Optional[str] = typer.Option(None, "--matrix-homeserver", envvar="HICLAW_MATRIX_HOMESERVER", help="Override Matrix homeserver URL (e.g. http://localhost:6167 when port-forwarding)"),
    matrix_token: Optional[str] = typer.Option(None, "--matrix-token", envvar="HICLAW_WORKER_MATRIX_TOKEN", help="Pre-provisioned Matrix access token (override)"),
    gateway_url: Optional[str] = typer.Option(None, "--gateway-url", envvar="HICLAW_AI_GATEWAY_URL", help="Higress gateway URL (externally reachable)"),
    gateway_key: Optional[str] = typer.Option(None, "--gateway-key", envvar="HICLAW_WORKER_GATEWAY_KEY", help="Higress consumer key"),
    use_subscription: bool = typer.Option(False, "--use-subscription", envvar="HICLAW_USE_CLAUDE_SUBSCRIPTION", help="Use claude.ai subscription via OAuth (run `claude login` first). Skips AI gateway."),
    model: Optional[str] = typer.Option(None, "--model", envvar="HICLAW_MODEL", help="Override LLM model (e.g. claude-opus-4-5). In subscription mode defaults to claude-sonnet-4-5 if not set."),
    install_dir: Optional[Path] = typer.Option(None, "--install-dir", envvar="HICLAW_INSTALL_DIR", help="Local workspace root (default ~/.hiclaw/agents)"),
    sync_interval: int = typer.Option(60, "--sync-interval", envvar="HICLAW_SYNC_INTERVAL", help="Pull interval (seconds)"),
    harness_type: str = typer.Option("claude", "--harness-type", envvar="HICLAW_HARNESS_TYPE", help="Harness CLI: claude|gemini|opencode|codex"),
) -> None:
    # When a subcommand (e.g. `attach`) is invoked, do not start the worker.
    if ctx.invoked_subcommand is not None:
        return

    required = {
        "--name / HICLAW_WORKER_NAME": name,
        "--fs / HICLAW_FS_ENDPOINT": fs,
        "--fs-key / HICLAW_FS_ACCESS_KEY": fs_key,
        "--fs-secret / HICLAW_FS_SECRET_KEY": fs_secret,
    }
    missing = [k for k, v in required.items() if not v]
    if missing:
        raise typer.BadParameter("missing required values: " + ", ".join(missing))

    # Export the values that inherited code + the bridge read directly from the
    # environment (so passing them as flags works the same as exporting them).
    os.environ["HICLAW_WORKER_NAME"] = name
    if matrix_domain:
        os.environ["HICLAW_MATRIX_DOMAIN"] = matrix_domain
    if matrix_homeserver:
        os.environ["HICLAW_MATRIX_HOMESERVER"] = matrix_homeserver
    if matrix_token:
        os.environ["HICLAW_WORKER_MATRIX_TOKEN"] = matrix_token
    if use_subscription:
        os.environ["HICLAW_USE_CLAUDE_SUBSCRIPTION"] = "1"
    elif gateway_url:
        os.environ["HICLAW_AI_GATEWAY_URL"] = gateway_url
    if gateway_key:
        os.environ["HICLAW_WORKER_GATEWAY_KEY"] = gateway_key
    if model:
        os.environ["HICLAW_MODEL"] = model

    config = WorkerConfig(
        worker_name=name,
        minio_endpoint=fs,
        minio_access_key=fs_key,
        minio_secret_key=fs_secret,
        minio_bucket=fs_bucket,
        minio_secure=fs_secure,
        sync_interval=sync_interval,
        install_dir=install_dir.expanduser() if install_dir else _default_install_dir(),
        harness_type=harness_type,
        model=model,
    )
    _run_worker(RemoteWorker(config))


@app.command()
def attach(
    name: Optional[str] = typer.Option(None, "--name", envvar="HICLAW_WORKER_NAME", help="Worker identity"),
    install_dir: Optional[Path] = typer.Option(None, "--install-dir", envvar="HICLAW_INSTALL_DIR", help="Local workspace root"),
    harness_type: str = typer.Option("claude", "--harness-type", envvar="HICLAW_HARNESS_TYPE", help="Harness CLI"),
) -> None:
    """Attach an interactive session to the worker's current conversation.

    Runs ``claude --resume <current-session>`` in the workspace so a developer
    can issue REPL-only slash commands (``/clear``, ``/compact``, ``/model``) and
    steer manually. Caveat: do this while the relay is idle — a relay-spawned
    ``claude -p`` and this interactive session can race on the session file.
    """
    if not name:
        raise typer.BadParameter("missing --name / HICLAW_WORKER_NAME")
    if harness_type != "claude":
        raise typer.BadParameter("attach currently supports --harness-type claude only")

    base = install_dir.expanduser() if install_dir else _default_install_dir()
    workspace = base / name
    if not workspace.is_dir():
        raise typer.BadParameter(f"workspace not found: {workspace} (run the worker first)")

    # Source .harness/.env so any local secrets are available to the session.
    Worker._load_env_file(workspace / ".harness" / ".env")

    session_file = workspace / ".harness" / "sessions" / "current"
    session_id = session_file.read_text().strip() if session_file.exists() else None

    argv = ["claude"]
    if session_id:
        argv += ["--resume", session_id]
        typer.echo(f"Attaching to session {session_id} in {workspace}")
    else:
        typer.echo(f"No saved session; starting a fresh interactive claude in {workspace}")

    os.chdir(workspace)
    # Hand the TTY directly to claude (replace this process).
    os.execvpe(argv[0], argv, os.environ.copy())


def _run_worker(worker: RemoteWorker) -> None:
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


def main() -> None:
    app()


if __name__ == "__main__":
    main()
