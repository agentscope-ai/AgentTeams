# Create Team

## CLI Usage

```bash
agt create team \
  --name <TEAM_NAME> \
  --leader-name <LEADER_NAME> \
  --leader-model <MODEL_ID> \
  --workers <w1>,<w2>,<w3> \
  [--description "Team description"] \
  [--leader-heartbeat-every 30m] \
  [--worker-idle-timeout 12h]
```

Notes:
- `--name` and `--leader-name` are required
- `--workers` is a comma-separated list of worker names
- `--leader-model` defaults to the install-time configured model (`$AGENTTEAMS_DEFAULT_MODEL` propagated by the controller); falls back to `qwen3.5-plus` only when that is unset
- Team Admin defaults to Global Admin
- Controller forces `runtime: copaw` for all team members
- For CPU/memory requests and limits, use YAML with `agt apply -f`; the simple team CLI flags do not expose per-member resources

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

After `agt create team`, the controller's Team reconciler handles:

1. Creates Matrix rooms: Team Room (Leader + Team Admin + all workers) and Leader DM (Team Admin ↔ Leader)
2. Creates the Team Leader Worker CR with team-leader-agent skills
3. Creates each team worker Worker CR with copaw-worker-agent skills
4. Injects coordination context into Leader's AGENTS.md (Team Room ID, Leader DM Room ID, worker list)
5. Sets up shared team storage in MinIO
6. Updates legacy teams registry

> The legacy `scripts/create-team.sh` is deprecated. Use `agt create team` instead.

## After Creation

1. Verify team created: `agt get team <TEAM_NAME>`
2. @mention the Leader in the Leader Room to assign the task
3. The Leader will handle coordination with team workers from there
