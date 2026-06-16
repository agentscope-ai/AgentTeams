---
name: teamharness-organization
description: "Use to understand TeamHarness team roles, responsibility boundaries, and who owns project, task, runtime, and control-plane work."
---

# Organization

Use this skill to reason about team roles and responsibility boundaries.

Leader owns planning, delegation, acceptance, and requester reporting. Workers
own assigned task execution. Remote members participate as invited local agents
or humans. Manager owns control-plane operations.

Do not let a Worker silently become project owner. Do not let a remote member
act like a controller-managed Worker unless the team explicitly models that
role.

## Role Boundaries

Leader owns:

- deciding the work breakdown
- creating project plans
- delegating task specs
- checking Worker results
- accepting or rejecting results for project progress
- reporting accepted outcomes to the requester

Worker owns:

- acknowledging assigned tasks
- reading `shared/tasks/{task-id}/spec.md`
- producing deliverables under `shared/tasks/{task-id}/`
- submitting `result.md`
- reporting blockers or completion to the Leader

Remote member owns only the work explicitly assigned to that member. Treat
remote members as participants, not controller-managed runtime resources.

Manager owns control-plane operations such as creating workers, changing model
configuration, deploying teams, and managing runtime state. TeamHarness skills
must not instruct agents to call control-plane APIs.

## Safety Boundary

Never write secrets, credentials, tokens, authorization headers, or live runtime
state into `TEAMS.md`, project files, task specs, task results, or Matrix
messages. If a task needs credentialed access, refer only to the approved
credential name or environment variable name, not its value.
