# Plan Changes, Blockers, and Team Changes

## Step 4: Handle Blocked Tasks

When a Worker reports a blocker (`[!]` marker):

1. Update plan.md: `[~]` → `[!]`
2. Assess if resolvable (missing dependency, unclear requirement, needs another Worker's input)
3. If you can resolve (clarify requirements, reassign): do so and re-assign
4. If it needs human input: escalate in DM with admin

## Step 5: Plan Changes

### Minor changes (no human gate)
- Reordering tasks within a phase
- Adjusting task scope slightly based on Worker feedback
- Adding sub-tasks to clarify deliverables

Document in plan.md Change Log and sync.

### Major changes (require human confirmation)
- Adding or removing Workers from the project
- Changing overall deliverables or project goal
- Reassigning >2 tasks between Workers
- Splitting or merging phases that alter timeline significantly
- Creating a new Worker role (explain skill gap first; see Step 7)

For major changes: draft in DM with admin, explain rationale and impact, wait for confirmation, then update plan.md and notify project room.

## Step 6: Onboard Mid-Project Worker

### 6a. Add to project room

```bash
curl -X POST "${HICLAW_MATRIX_SERVER}/_matrix/client/v3/rooms/${ROOM_ID}/invite" \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"user_id": "@<new-worker>:<matrix_domain>"}'
```

Also add to Manager's `groupAllowFrom` and sync to MinIO.

### 6b. Send onboarding message

@mention the new Worker in project room with: background, current progress, their role, and link to plan.md. Then notify admin in DM.

## Step 7: New Worker Headcount Request

When the project needs a Worker role that doesn't exist:

1. Explain the skill gap
2. Explain the impact (what's blocked)
3. Propose Worker profile: name, role, skills, MCP access

Present to admin in DM. After approval, use worker-management skill to create.

## Heartbeat — Project Monitoring

During heartbeat, for each active project:
1. Scan `shared/projects/*/meta.json` for `status: "active"`
2. Check plan.md for `[~]` tasks
3. For each in-progress task, check if Worker has sent an @mention recently
4. If no activity since last heartbeat: @mention Worker asking for update
