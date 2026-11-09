# Create Team

## Choose your Team Admin first

If the requester already named a Team Admin, resolve that existing Human and use it. Otherwise run `agt get humans -o json`, identify the current requester from the trusted message sender Matrix ID, and ask who should administer the team. Offer the current requester and the other existing eligible Humans (team-level users or Global Admin); do not invent users or infer the requester from the message body. If you cannot resolve the requester, ask them to choose an existing Human.

In your reply, emit this fenced block with a unique `confirmation_id` and actual candidates from the Human list. Dashboard renders it as a dropdown. Include the Human resource name in each label so the reply remains unambiguous. Put the current requester first and mark its description as the current conversation user. Do not mark a default as recommended or submit on the user's behalf.

```team-admin
{"confirmation_id":"team-admin-<TEAM_NAME>-<UNIQUE_ID>","question":"请选择团队管理员：当前对话用户还是其他用户？","options":[{"id":"<HUMAN_RESOURCE_NAME>","label":"<DISPLAY_NAME>（<HUMAN_RESOURCE_NAME>）","description":"当前对话用户 · <MATRIX_USER_ID>"},{"id":"<OTHER_HUMAN_RESOURCE_NAME>","label":"<OTHER_DISPLAY_NAME>（<OTHER_HUMAN_RESOURCE_NAME>）","description":"<OTHER_MATRIX_USER_ID>"}]}
```

Stop this turn and wait for the selection. On clients without dropdown support, accept an explicit Human name as a text reply. Resolve the selected Human again, then pass `--admin <HUMAN_RESOURCE_NAME>` to `agt create team` (`scripts/create-team.sh` calls this flag `--team-admin`). Do not create the Team before receiving the answer. If no eligible Humans exist, ask the requester to create one first.

The Team Admin belongs in `spec.admin`; additional humans belong in `humanMembers`. Do not duplicate the Team Admin into `humanMembers` to grant API visibility. If Global Admin should also participate when another Human administers the Team, add Global Admin explicitly as a human member; global administrative visibility alone does not grant room membership.

## Create the Team

```bash
agt create team \
  --name <TEAM_NAME> \
  --leader-name <LEADER_NAME> \
  --admin <HUMAN_RESOURCE_NAME> \
  --workers <w1>,<w2>,<w3> \
  [--description "Team description"] \
  [--leader-heartbeat-every 30m]
```

Notes:
- `--name` and `--leader-name` are required
- `--leader-name` and `--workers` must name existing Worker CRs
- `--workers` is a comma-separated list of existing Worker names
- Always pass the confirmed Team Admin with `--admin`; only legacy API callers fall back to Global Admin
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
