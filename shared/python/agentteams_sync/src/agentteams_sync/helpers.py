"""Small helpers shared by FileSync implementations."""

from __future__ import annotations

STARTUP_SYNC_FILES = (
    "openclaw.json",
    "AGENTS.md",
    "SOUL.md",
    "HEARTBEAT.md",
    "config/mcporter.json",
    "mcporter-servers.json",
)


def team_storage_name_from_worker_team(bucket: str, team_ref: str) -> str:
    """Derive the temporary storage team name from a WorkerResponse team ref."""
    team_name = team_ref.strip()
    bucket_name = (bucket or "").strip()
    prefixes = [bucket_name]
    if bucket_name.startswith("hiclaw-"):
        prefixes.append(bucket_name.removeprefix("hiclaw-"))

    for prefix in prefixes:
        if prefix and team_name.startswith(f"{prefix}-"):
            return team_name[len(prefix) + 1 :]
    return team_name
