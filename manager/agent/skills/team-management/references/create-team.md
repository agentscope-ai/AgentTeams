# Create Team

> **Preferred path:** `hiclaw create team` (see below). The skill wrapper `scripts/create-team.sh` delegates to it by default and preserves the same flags (`--leader` is accepted as an alias for `--leader-name`).

## CLI Usage

```bash
hiclaw create team \
  --name <TEAM_NAME> \
  --leader-name <LEADER_NAME> \
  --leader-model <MODEL_ID> \
  --workers <w1>,<w2>,<w3> \
  [--description "Team description"] \
  [--leader-heartbeat-every 30m] \
  [--worker-idle-timeout 12h] \
  [--team-admin <USERNAME>] \
  [--team-admin-matrix-id @user:domain] \
  [--peer-mentions true|false] \
  [--worker-models m1,m2] \
  [--worker-runtimes copaw,hermes] \
  [--worker-skills s1,s2:s3] \
  [--worker-mcp-servers github,slack:filesystem] \
  [--leader-mcp-servers github] \
  [--team-channel-policy '{"groupAllowExtra":["@user:domain"]}'] \
  [--leader-channel-policy '{"dmAllowExtra":["@user:domain"]}'] \
  [--worker-channel-policies '{"groupAllowExtra":["@w1:domain"]}|{"groupAllowExtra":["@w2:domain"]}']
```

Notes:
- `--name` and `--leader-name` are required
- `--workers` is a comma-separated list of worker names
- `--leader-model` defaults to the install-time configured model (`$AGENTTEAMS_DEFAULT_MODEL` propagated by the controller); falls back to `qwen3.5-plus` only when that is unset
- Team Admin defaults to Global Admin; override with `--team-admin` and/or `--team-admin-matrix-id`
- `--peer-mentions` defaults to controller behavior (enabled); pass `false` to disable inter-worker mentions
- Per-worker lists use comma alignment with `--workers`: comma within a worker for models/skills/MCP names, colon between workers for skills/MCP, pipe between workers for channel-policy JSON
- `--worker-runtimes` sets per-worker runtime (`openclaw`, `copaw`, `hermes`, `openhuman`); Team Leader runtime remains controller-default `copaw`
- `--team-channel-policy-file` / `--leader-channel-policy-file` accept JSON files instead of inline JSON
- For CPU/memory requests and limits, custom worker images, or full MCP server URLs, use YAML with `hiclaw apply -f`

## Skill wrapper (`create-team.sh`)

The wrapper calls `hiclaw create team` with mapped flags, then runs Manager-side-only post-hooks:

| Step | Owner | What |
|------|-------|------|
| Team CR + Matrix rooms + Worker CRs + MinIO team storage + Leader coordination context | Controller | `hiclaw create team` |
| Human `groupAllowFrom` backfill + Team Room invite when a Human in `~/humans-registry.json` already lists this team in `accessible_teams` | Manager (`create-team.sh` post-hook) | Not in controller CLI |

Force the legacy shell implementation (Matrix pre-provision + direct `create-worker.sh`) with:

```bash
HICLAW_TEAM_CREATE_IMPL=shell bash scripts/create-team.sh ...
```

Use legacy mode only when debugging an environment without a working controller API.

## CPU and memory resources

Use `leader.resources` and `workers[].resources` when admin asks for per-member CPU or memory requests/limits:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: <TEAM_NAME>
spec:
  leader:
    name: <LEADER_NAME>
    resources:
      requests:
        cpu: 300m
        memory: 768Mi
      limits:
        cpu: "2"
        memory: 3Gi
  workers:
    - name: <WORKER_NAME>
      runtime: hermes
      resources:
        requests:
          cpu: 200m
          memory: 512Mi
        limits:
          cpu: "1"
          memory: 2Gi
```

Changing resources recreates the affected member container. Confirm the team is idle or that admin accepts interruption before applying resource changes.

## What the Controller Does

After `hiclaw create team`, the controller's Team reconciler handles:

1. Creates Matrix rooms: Team Room (Leader + Team Admin + all workers) and Leader DM (Team Admin ↔ Leader)
2. Creates the Team Leader Worker CR with team-leader-agent skills
3. Creates each team worker Worker CR with copaw-worker-agent skills (or runtime-specific agent dir when `--worker-runtimes` / YAML `runtime` is set)
4. Injects coordination context into Leader's AGENTS.md (Team Room ID, Leader DM Room ID, worker list)
5. Sets up shared team storage in MinIO
6. Updates legacy teams registry

## After Creation

1. Verify team created: `hiclaw get teams <TEAM_NAME>`
2. @mention the Leader in the Leader Room to assign the task
3. The Leader will handle coordination with team workers from there
