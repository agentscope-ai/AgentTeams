"""OpenClaw Worker Matrix bootstrap — E2EE crypto wipe and re-login (O13.5)."""

from __future__ import annotations

import argparse
import json
import logging
import os
import shutil
import sys
import urllib.error
import urllib.request
from datetime import datetime
from pathlib import Path
from typing import Any

from agentteams_sync import mc as mc_ops
from agentteams_sync.contract import RUNTIME_CONTRACTS

logger = logging.getLogger(__name__)


def _worker_log(message: str) -> None:
    """Match ``worker-entrypoint.sh`` log prefix."""
    ts = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"[agentteams-worker {ts}] {message}", flush=True)


def resolve_workspace() -> Path:
    """Worker workspace — ``HOME`` when set, else ``AGENTTEAMS_ROOT/agents/{name}``."""
    home = os.environ.get("HOME")
    if home:
        return Path(home)
    worker_name = os.environ.get("AGENTTEAMS_WORKER_NAME", "")
    root = Path(os.environ.get("AGENTTEAMS_ROOT", "/root/hiclaw-fs"))
    return root / "agents" / worker_name


def wipe_matrix_crypto(home: Path | None = None) -> None:
    """Remove local Matrix crypto storage (matches ``rm -rf ~/.openclaw/matrix``)."""
    target_home = home or resolve_workspace()
    matrix_dir = target_home / ".openclaw" / "matrix"
    shutil.rmtree(matrix_dir, ignore_errors=True)
    _worker_log("Cleaned Matrix crypto storage (will re-establish E2EE sessions)")


def _read_matrix_password(worker_name: str) -> str | None:
    prefix = os.environ.get("AGENTTEAMS_STORAGE_PREFIX", "")
    if not prefix:
        return None
    password_path = f"{prefix}/agents/{worker_name}/credentials/matrix/password"
    try:
        result = mc_ops.mc(
            "cat",
            password_path,
            check=False,
            warn_on_error=False,
            log_output=False,
        )
    except Exception:
        return None
    if result.returncode != 0 or not result.stdout:
        return None
    return result.stdout.strip()


def _load_openclaw_config(path: Path) -> dict[str, Any]:
    if not path.is_file():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def _write_openclaw_config(path: Path, cfg: dict[str, Any]) -> None:
    tmp = path.with_suffix(".relogin.json")
    tmp.write_text(json.dumps(cfg, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    tmp.replace(path)


def relogin_matrix(
    *,
    worker_name: str,
    workspace: Path | None = None,
    openclaw_path: Path | None = None,
) -> bool:
    """Re-login to Matrix and refresh ``accessToken`` in openclaw.json.

    Returns ``True`` when a new token was written, ``False`` otherwise.
    """
    ws = workspace or resolve_workspace()
    config_path = openclaw_path or ws / "openclaw.json"

    password = _read_matrix_password(worker_name)
    if not password:
        _worker_log(
            "No Matrix password found in MinIO, skipping re-login "
            "(E2EE may not work after restart)"
        )
        return False

    cfg = _load_openclaw_config(config_path)
    matrix_cfg = cfg.get("channels", {}).get("matrix", {})
    homeserver = (matrix_cfg.get("homeserver") or "").strip()
    if not homeserver:
        _worker_log("WARNING: Missing homeserver URL in openclaw.json, skipping Matrix re-login")
        return False

    _worker_log("Re-logging into Matrix to get fresh access token and device ID...")
    login_url = f"{homeserver.rstrip('/')}/_matrix/client/v3/login"
    login_body = json.dumps(
        {
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": worker_name},
            "password": password,
        }
    ).encode()

    login_resp_text = ""
    try:
        req = urllib.request.Request(
            login_url,
            data=login_body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            login_resp_text = resp.read().decode()
        login_resp = json.loads(login_resp_text)
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as exc:
        _worker_log(
            "WARNING: Matrix re-login failed, using existing access token "
            "(E2EE may not work with Element Web)"
        )
        detail = login_resp_text or str(exc)
        _worker_log(f"  Response: {detail}")
        return False

    new_token = login_resp.get("access_token") or ""
    new_device = login_resp.get("device_id") or ""
    if not new_token or new_token == "null":
        _worker_log(
            "WARNING: Matrix re-login failed, using existing access token "
            "(E2EE may not work with Element Web)"
        )
        _worker_log(f"  Response: {login_resp_text}")
        return False

    cfg.setdefault("channels", {}).setdefault("matrix", {})["accessToken"] = new_token
    _write_openclaw_config(config_path, cfg)
    _worker_log(
        f"Matrix re-login successful (new device: {new_device}, "
        f"token prefix: {new_token[:10]}...)"
    )
    return True


def bootstrap_openclaw_matrix(
    *,
    worker_name: str | None = None,
    workspace: Path | None = None,
) -> None:
    """Wipe crypto storage then re-login — entrypoint Step 5 parity."""
    name = worker_name or os.environ.get("AGENTTEAMS_WORKER_NAME")
    if not name:
        raise SystemExit("AGENTTEAMS_WORKER_NAME is required for openclaw-matrix")
    ws = workspace or resolve_workspace()
    wipe_matrix_crypto(ws)
    relogin_matrix(worker_name=name, workspace=ws)


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(
        description="OpenClaw Worker Matrix E2EE bootstrap (crypto wipe + re-login)"
    )
    parser.add_argument(
        "command",
        choices=["openclaw-matrix"],
        help="Run Matrix crypto wipe and re-login before OpenClaw starts",
    )
    parser.add_argument(
        "--contract",
        required=True,
        choices=sorted(RUNTIME_CONTRACTS),
        help="SyncContract preset (only openclaw is supported)",
    )
    args = parser.parse_args(argv)
    if args.contract != "openclaw":
        parser.error(f"openclaw-matrix only supports contract=openclaw, got {args.contract!r}")
    if args.command != "openclaw-matrix":
        parser.error(f"unknown command {args.command!r}")
    try:
        bootstrap_openclaw_matrix()
    except KeyboardInterrupt:
        sys.exit(0)


if __name__ == "__main__":
    main()
