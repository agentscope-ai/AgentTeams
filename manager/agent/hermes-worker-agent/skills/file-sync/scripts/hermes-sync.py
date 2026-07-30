#!/usr/bin/env python3
"""
hermes-sync - Manual sync trigger for Hermes Worker.

Reads MinIO credentials from environment variables and triggers an immediate
sync of config files (openclaw.json, SOUL.md, AGENTS.md, skills) from MinIO,
then re-bridges them into ``${HERMES_HOME}/{config.yaml,.env,SOUL.md,AGENTS.md}``.

Environment variables (set by the container at startup):
- ``AGENTTEAMS_WORKER_NAME``  worker name
- ``AGENTTEAMS_FS_ENDPOINT``  MinIO endpoint  (e.g. http://fs-local.agentteams.io:18080)
- ``AGENTTEAMS_FS_ACCESS_KEY`` MinIO access key (= worker name)
- ``AGENTTEAMS_FS_SECRET_KEY`` MinIO secret key
- ``AGENTTEAMS_FS_BUCKET``    MinIO bucket    (default: agentteams-storage)
- ``HERMES_HOME``         hermes-agent workspace
                          (default: $HOME/.hermes, i.e.
                          /root/agentteams-fs/agents/<worker_name>/.hermes)
"""
from __future__ import annotations

import os
import sys
from pathlib import Path

_VENV_REEXEC_MARKER = "_HERMES_SYNC_VENV_REEXEC"


def _find_venv_python() -> str | None:
    """Return the first venv python that ships hermes_worker."""
    for venv in ("/opt/venv/hermes",):
        py = Path(venv) / "bin" / "python3"
        if py.exists():
            return str(py)
    return None


try:
    from hermes_worker.bridge import bridge_openclaw_to_hermes
    from hermes_worker.sync import FileSync
except ImportError:
    src_path = Path(__file__).parent.parent.parent.parent / "src"
    if src_path.exists():
        sys.path.insert(0, str(src_path))
        try:
            from hermes_worker.bridge import bridge_openclaw_to_hermes
            from hermes_worker.sync import FileSync
        except ImportError:
            pass

    if "hermes_worker" not in sys.modules and not os.environ.get(_VENV_REEXEC_MARKER):
        venv_py = _find_venv_python()
        if venv_py:
            os.environ[_VENV_REEXEC_MARKER] = "1"
            os.execv(venv_py, [venv_py] + sys.argv)
        print(
            "Error: hermes-worker package not found and no venv detected.\n"
            "Please install it with: pip install hermes-worker",
            file=sys.stderr,
        )
        sys.exit(1)
    elif "hermes_worker" not in sys.modules:
        print("Error: hermes-worker package not found even in venv.", file=sys.stderr)
        sys.exit(1)


def main() -> None:
    worker_name = os.getenv("AGENTTEAMS_WORKER_NAME")
    minio_endpoint = os.getenv("AGENTTEAMS_FS_ENDPOINT")
    minio_access_key = os.getenv("AGENTTEAMS_FS_ACCESS_KEY")
    minio_secret_key = os.getenv("AGENTTEAMS_FS_SECRET_KEY")
    minio_bucket = os.getenv("AGENTTEAMS_FS_BUCKET", "agentteams-storage")
    hermes_home = os.getenv("HERMES_HOME")

    if not all([worker_name, minio_endpoint, minio_access_key, minio_secret_key]):
        print("Error: Missing required environment variables", file=sys.stderr)
        print(
            "Required: AGENTTEAMS_WORKER_NAME, AGENTTEAMS_FS_ENDPOINT, "
            "AGENTTEAMS_FS_ACCESS_KEY, AGENTTEAMS_FS_SECRET_KEY",
            file=sys.stderr,
        )
        sys.exit(1)

    # Workspace == HOME (== MinIO mirror root); aligned with openclaw worker.
    workspace_dir = Path.home()
    if hermes_home:
        hermes_home_path = Path(hermes_home)
    else:
        hermes_home_path = workspace_dir / ".hermes"

    print(f"Syncing files for worker: {worker_name}")
    print(f"MinIO endpoint: {minio_endpoint}")
    print(f"Workspace:     {workspace_dir}")
    print(f"HERMES_HOME:   {hermes_home_path}")

    sync = FileSync(
        endpoint=minio_endpoint,
        access_key=minio_access_key,
        secret_key=minio_secret_key,
        bucket=minio_bucket,
        worker_name=worker_name,
        secure=minio_endpoint.startswith("https://"),
        local_dir=workspace_dir,
    )

    try:
        changed = sync.pull_all()
        if changed:
            print(f"✓ Synced {len(changed)} file(s): {', '.join(changed)}")

            if any("openclaw.json" in f for f in changed):
                print("Re-bridging openclaw.json to hermes config...")
                openclaw_cfg = sync.get_config()
                soul = sync.get_soul()
                agents = sync.get_agents_md()
                bridge_openclaw_to_hermes(
                    openclaw_cfg, hermes_home_path, soul=soul, agents_md=agents,
                )
                print(
                    "✓ Config re-bridged. Restart the worker if you changed model "
                    "or matrix settings; SOUL.md / skill changes apply on the next "
                    "message."
                )
        else:
            print("✓ No changes detected. All files are up to date.")
    except Exception as exc:
        print(f"✗ Sync failed: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
