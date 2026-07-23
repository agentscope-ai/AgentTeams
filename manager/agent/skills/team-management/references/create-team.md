# Create Team

## CLI Usage

```bash
agt create team \
  --name <TEAM_NAME> \
  --leader-name <LEADER_NAME> \
  --workers <w1>,<w2>,<w3> \
  [--description "Team description"] \
  [--leader-heartbeat-every 30m]
```

Notes:
- `--name` and `--leader-name` are required
- `--leader-name` and `--workers` must name existing Worker CRs
- `--workers` is a comma-separated list of existing Worker names
- Team Admin defaults to Global Admin
- Configure each Worker's model, runtime, image, resources, identity, skills, MCP, channel policy, and lifecycle on that Worker CR before creating the Team

## CPU and memory resources

Use `Worker.spec.resources` when admin asks for CPU or memory requests/limits:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: <WORKER_NAME>
spec:
  resources:
    requests:
      cpu: 200m
      memory: 512Mi
    limits:
      cpu: "1"
      memory: 2Gi
```

Changing resources recreates that Worker's container. Confirm the Worker is idle or that admin accepts interruption before applying resource changes.

## What the Controller Does

After `agt create team`, the controller's Team reconciler handles:

1. Validates every `workerMembers` reference and the single `team_leader` role
2. Creates Matrix rooms: Team Room (Leader + Team Admin + all workers) and Leader DM (Team Admin ↔ Leader)
3. Injects Team-owned coordination context into the referenced Workers
4. Sets up shared Team storage in MinIO
5. Aggregates referenced Worker readiness into Team status without owning their lifecycle

## After Creation

1. Verify team created: `agt get team <TEAM_NAME>`
2. @mention the Leader in the Leader Room to assign the task
3. The Leader will handle coordination with team workers from there
