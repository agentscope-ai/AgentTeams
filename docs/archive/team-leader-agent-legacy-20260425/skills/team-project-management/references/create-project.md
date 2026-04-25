# Create Team Project

## When to Create

Create a team project when:
- Manager delegates a complex task requiring multiple workers
- Team Admin requests work that needs multi-worker collaboration
- Tasks have dependencies that benefit from DAG orchestration

## Steps

### 1. Determine Source

Identify who initiated the task:
- **Leader Room** @mention → `source=manager`, note the `parent-task-id`
- **Leader DM** message → `source=team-admin`, note the `requester` Matrix ID

### 2. Generate Project ID

```bash
PROJECT_ID="tp-$(date +%Y%m%d-%H%M%S)"
```

### 3. Run create-team-project.sh

```bash
# Manager source
bash ./skills/team-project-management/scripts/create-team-project.sh \
  --id "${PROJECT_ID}" --title "Project Title" \
  --workers alice,bob,charlie \
  --source manager --parent-task-id task-xxx

# Team Admin source
bash ./skills/team-project-management/scripts/create-team-project.sh \
  --id "${PROJECT_ID}" --title "Project Title" \
  --workers alice,bob \
  --source team-admin --requester "@admin:domain"
```

The script will:
1. Create `shared/projects/{project-id}/meta.json` + `plan.md`
2. Publish the project files to team shared storage
3. Register in `team-state.json`

### 4. Fill in plan.md

Edit the generated `plan.md` to add the DAG task plan. See `references/plan-format.md` for the format.

### 5. Validate DAG

```bash
bash ./skills/team-project-management/scripts/resolve-dag.sh \
  --plan shared/projects/{project-id}/plan.md \
  --action validate
```

### 6. Confirm with Requester

- **Manager source**: No explicit confirmation needed (Manager already approved via spec.md)
- **Team Admin source**: Post the plan summary in Leader DM and wait for Team Admin to confirm before activating

### 7. Activate

Update `shared/projects/{project-id}/plan.md` Status from `planning` to `active`. Update `meta.json` status. Publish the updated project files.

### 8. Start Execution

Run `resolve-dag.sh --action ready` to get initial tasks, then follow `references/dag-execution.md`.
