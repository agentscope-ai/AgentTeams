#!/usr/bin/env python3
"""Pull Manager-managed files from MinIO into this CLI-harness worker's workspace.

Mirrors the `hermes-sync` script: best-effort `mc mirror` of openclaw.json,
skills, and shared/, so the agent can refresh on demand instead of waiting
for the periodic sync loop.
"""
import os
import subprocess
import sys
from pathlib import Path


def storage_prefix() -> str:
    prefix = os.environ.get("AGENTTEAMS_STORAGE_PREFIX", "")
    if prefix:
        return prefix.rstrip("/")
    bucket = os.environ.get("AGENTTEAMS_FS_BUCKET", "agentteams-storage")
    return f"agentteams/{bucket}"


def worker_name() -> str:
    name = os.environ.get("AGENTTEAMS_WORKER_NAME", "")
    if name:
        return name
    home = Path.home()
    return home.name if home.parent.name == "agents" else ""


def run(cmd: list[str], required: bool = False) -> bool:
    result = subprocess.run(cmd, capture_output=True, text=True)
    ok = result.returncode == 0
    if not ok and required:
        print(f"pull failed: {' '.join(cmd)}\n{result.stderr}", file=sys.stderr)
    return ok


def main() -> int:
    name = worker_name()
    if not name:
        print("AGENTTEAMS_WORKER_NAME not set and workspace layout unrecognized", file=sys.stderr)
        return 1

    prefix = storage_prefix()
    workspace = Path.home()

    mirror_targets = [
        (f"{prefix}/agents/{name}/skills/", str(workspace / "skills")),
        (f"{prefix}/shared/", str(workspace / "shared")),
    ]
    for remote, local in mirror_targets:
        Path(local).mkdir(parents=True, exist_ok=True)
        run(["mc", "mirror", remote + "/", local + "/", "--overwrite"])

    config = workspace / "openclaw.json"
    run(["mc", "cat", f"{prefix}/agents/{name}/openclaw.json"], required=not config.exists())
    return 0


if __name__ == "__main__":
    sys.exit(main())
