"""CLI entry point for manually triggering CoPaw Worker file sync."""

from __future__ import annotations

import os
import sys
from pathlib import Path

from copaw_worker.bridge import bridge_openclaw_to_copaw
from copaw_worker.sync import FileSync


def main() -> None:
    """Run one remote-to-local sync cycle for the current CoPaw Worker."""
    worker_name = os.getenv("HICLAW_WORKER_NAME") or os.getenv("COPAW_WORKER_NAME")
    minio_endpoint = os.getenv("HICLAW_FS_ENDPOINT") or os.getenv("COPAW_MINIO_ENDPOINT")
    minio_access_key = os.getenv("HICLAW_FS_ACCESS_KEY") or os.getenv("COPAW_MINIO_ACCESS_KEY")
    minio_secret_key = os.getenv("HICLAW_FS_SECRET_KEY") or os.getenv("COPAW_MINIO_SECRET_KEY")
    minio_bucket = os.getenv("HICLAW_FS_BUCKET") or os.getenv("COPAW_MINIO_BUCKET") or "hiclaw-storage"
    working_dir_env = os.getenv("COPAW_WORKING_DIR")

    if not all([worker_name, minio_endpoint, minio_access_key, minio_secret_key]):
        print("Error: Missing required environment variables", file=sys.stderr)
        print(
            "Required: HICLAW_WORKER_NAME, HICLAW_FS_ENDPOINT, "
            "HICLAW_FS_ACCESS_KEY, HICLAW_FS_SECRET_KEY",
            file=sys.stderr,
        )
        sys.exit(1)

    working_dir = (
        Path(working_dir_env)
        if working_dir_env
        else Path.home() / ".copaw-worker" / worker_name / ".copaw"
    )

    print(f"Syncing files for worker: {worker_name}")
    print(f"MinIO endpoint: {minio_endpoint}")
    print(f"Working directory: {working_dir}")
    workspace_dir = working_dir / "workspaces" / "default"

    sync = FileSync(
        endpoint=minio_endpoint,
        access_key=minio_access_key,
        secret_key=minio_secret_key,
        bucket=minio_bucket,
        worker_name=worker_name,
        secure=str(minio_endpoint).startswith("https://"),
        local_dir=working_dir.parent,
        shared_dir=workspace_dir / "shared",
        global_shared_dir=workspace_dir / "global-shared",
    )

    try:
        changed = sync.pull_all()
        if not changed:
            print("OK: No changes detected. All files are up to date.")
            return

        print(f"OK: Synced {len(changed)} file(s): {', '.join(changed)}")

        if any("openclaw.json" in f for f in changed):
            print("Re-bridging openclaw.json to CoPaw config...")
            openclaw_cfg = sync.get_config()
            soul = sync.get_soul()
            agents = sync.get_agents_md()

            if soul:
                (working_dir / "SOUL.md").write_text(soul)
            if agents:
                (working_dir / "AGENTS.md").write_text(agents)

            bridge_openclaw_to_copaw(openclaw_cfg, working_dir)
            print("OK: Config re-bridged. CoPaw will hot-reload automatically.")
    except Exception as exc:
        print(f"ERROR: Sync failed: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
