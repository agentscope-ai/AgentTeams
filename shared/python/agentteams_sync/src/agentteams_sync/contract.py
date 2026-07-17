"""Per-runtime sync contracts — preserve intentional divergence (Phase 6 Y6.1)."""

from __future__ import annotations

from dataclasses import dataclass

from agentteams_sync.policy import PushPolicy


@dataclass(frozen=True)
class SyncContract:
    """Documented sync semantics for one worker runtime."""

    runtime: str
    implementation: str
    startup_mirror_agent_prefix: bool
    startup_mirror_shared: bool
    startup_mirror_global_shared_for_team_leader: bool
    background_pull_loop: bool
    pull_interval_seconds: int | None
    pull_all_includes_shared: bool
    pull_all_includes_global_shared: bool
    merge_openclaw_on_pull: bool
    background_push_loop: bool
    push_check_interval_seconds: int
    push_policy: PushPolicy
    on_demand_shared_pull: bool
    notes: str = ""


COPAW = SyncContract(
    runtime="copaw",
    implementation="shared/python/agentteams_sync (via copaw_worker.sync shim)",
    startup_mirror_agent_prefix=True,
    startup_mirror_shared=True,
    startup_mirror_global_shared_for_team_leader=True,
    background_pull_loop=True,
    pull_interval_seconds=60,
    pull_all_includes_shared=False,
    pull_all_includes_global_shared=False,
    merge_openclaw_on_pull=True,
    background_push_loop=True,
    push_check_interval_seconds=5,
    push_policy=PushPolicy.copaw(),
    on_demand_shared_pull=True,
    notes=(
        "Shared auto-mirrors at startup only; runtime pull_all excludes shared/. "
        "Explicit filesync pull/push for shared paths. Inner→outer bridge before push."
    ),
)

HERMES = SyncContract(
    runtime="hermes",
    implementation="shared/python/agentteams_sync (via hermes_worker.sync shim)",
    startup_mirror_agent_prefix=True,
    startup_mirror_shared=True,
    startup_mirror_global_shared_for_team_leader=True,
    background_pull_loop=True,
    pull_interval_seconds=300,
    pull_all_includes_shared=True,
    pull_all_includes_global_shared=True,
    merge_openclaw_on_pull=True,
    background_push_loop=True,
    push_check_interval_seconds=5,
    push_policy=PushPolicy.hermes(),
    on_demand_shared_pull=True,
    notes="pull_all mirrors shared/ every sync tick; team id from AGENTS.md not hiclaw CLI.",
)

OPENCLAW = SyncContract(
    runtime="openclaw",
    implementation=(
        "shared/python/agentteams_sync daemon + openclaw-matrix CLI (worker-entrypoint.sh)"
    ),
    startup_mirror_agent_prefix=True,
    startup_mirror_shared=True,
    startup_mirror_global_shared_for_team_leader=False,
    background_pull_loop=True,
    pull_interval_seconds=300,
    pull_all_includes_shared=True,
    pull_all_includes_global_shared=False,
    merge_openclaw_on_pull=True,
    background_push_loop=True,
    push_check_interval_seconds=5,
    push_policy=PushPolicy.openclaw(),
    on_demand_shared_pull=True,
    notes="PULL_MARKER mtime guard; SOUL/AGENTS/HEARTBEAT pushed via per-file mc cp when newer than marker.",
)

OPENHUMAN = SyncContract(
    runtime="openhuman",
    implementation="openhuman/scripts/openhuman-worker-entrypoint.sh (bash mc loops; PushPolicy.openhuman preset)",
    startup_mirror_agent_prefix=True,
    startup_mirror_shared=True,
    startup_mirror_global_shared_for_team_leader=False,
    background_pull_loop=True,
    pull_interval_seconds=300,
    pull_all_includes_shared=True,
    pull_all_includes_global_shared=False,
    merge_openclaw_on_pull=False,
    background_push_loop=True,
    push_check_interval_seconds=30,
    push_policy=PushPolicy.openhuman(),
    on_demand_shared_pull=False,
    notes=(
        "Uses AGENTTEAMS_STORAGE_ALIAS from hiclaw-env (fixed in Phase 13). "
        "Bash push mirrors memory/ and shared/ separately; shared push excludes spec.md and base/. "
        "No openclaw merge loop."
    ),
)

QWENPAW = SyncContract(
    runtime="qwenpaw",
    implementation="shared/python/agentteams_sync (via qwenpaw_worker.sync shim)",
    startup_mirror_agent_prefix=True,
    startup_mirror_shared=True,
    startup_mirror_global_shared_for_team_leader=False,
    background_pull_loop=False,
    pull_interval_seconds=None,
    pull_all_includes_shared=False,
    pull_all_includes_global_shared=False,
    merge_openclaw_on_pull=False,
    background_push_loop=True,
    push_check_interval_seconds=5,
    push_policy=PushPolicy.qwenpaw(),
    on_demand_shared_pull=False,
    notes="Push loop only; runtime config pulled via separate runtime.yaml path.",
)

TEAMHARNESS_MCP = SyncContract(
    runtime="teamharness-mcp",
    implementation="shared/python/agentteams_sync.FileSync (via plugins/teamharness/mcp/tools/filesync.py)",
    startup_mirror_agent_prefix=False,
    startup_mirror_shared=False,
    startup_mirror_global_shared_for_team_leader=False,
    background_pull_loop=False,
    pull_interval_seconds=None,
    pull_all_includes_shared=False,
    pull_all_includes_global_shared=False,
    merge_openclaw_on_pull=False,
    background_push_loop=False,
    push_check_interval_seconds=0,
    push_policy=PushPolicy.copaw(),
    on_demand_shared_pull=True,
    notes=(
        "On-demand pull/push/stat/list only via MCP filesync tool. "
        "workspaceDir maps shared/ and global-shared/ under the runtime workspace; "
        "storage.sharedPrefix and storage.globalSharedPrefix override remote roots "
        "(runtime.yaml / AGENTTEAMS_STORAGE_PREFIX). global-shared/ push is read-only."
    ),
)

RUNTIME_CONTRACTS: dict[str, SyncContract] = {
    "copaw": COPAW,
    "hermes": HERMES,
    "openclaw": OPENCLAW,
    "openhuman": OPENHUMAN,
    "qwenpaw": QWENPAW,
    "teamharness-mcp": TEAMHARNESS_MCP,
}
