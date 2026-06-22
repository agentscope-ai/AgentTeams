---
name: teamharness-team-coordination
description: "Use before a TeamHarness Leader chooses task boundaries, dependency shape, ownership, acceptance criteria, or follow-up work."
---

# Team Coordination

Use this skill when acting as Leader to coordinate several members.

Break work into bounded tasks, assign one owner per task, keep dependencies
visible, and avoid asking multiple workers to silently own the same result.

Report accepted outcomes, not every intermediate worker message.

## Leader Strategy

You are the Team Leader. Design context boundaries, task ownership, dependency
shape, and acceptance criteria. Do not act as a message router that simply
forwards the requester's raw message to Workers.

Before delegation, clarify:

- what done means
- which Worker owns each deliverable
- which task can run independently
- which result must be accepted before downstream work starts
- where the requester should receive final updates

If the goal, acceptance standard, ownership, or safe next step is ambiguous, ask
the requester before dispatching work.

## Choose Execution Mode

TeamHarness Project Work supports DAG and Loop execution modes.

Choose DAG when the work is finite and the dependency graph can be planned now.

DAG fits:

- known phases
- known dependencies
- fan-out and fan-in work
- one-shot implementation
- bounded verification

Choose Loop when the work repeats until a stop condition, quality gate,
evidence threshold, or maximum iteration count is reached.

Loop fits:

- many rounds
- repeated research waves
- quality improvement until accepted
- build-test-fix until passing
- exploration where the next question depends on current results
- requester language such as "iterate", "repeat", "retry until passing", or
  "up to N rounds"

Do not pre-expand repeated Loop rounds into a large DAG. Plan the current
iteration, evaluate it, then decide whether to continue, replan, ask the
requester, stop successfully, or stop blocked.

## Good Task Boundaries

A good task has:

- one owner
- one inspectable output
- enough context to work independently
- no write conflict with sibling tasks
- clear acceptance criteria
- deliverables inside `shared/tasks/{task-id}/`

Do not assign multiple Workers to write the same file, directory, final answer,
or decision record.

## Result Decisions

A Worker completion is not automatic project progress. After `check_task`, decide
whether to:

- accept the result and mark the project node `completed`
- request revision through a new task
- add a verifier task
- ask the requester for clarification
- report a blocker

Only accepted results should unblock downstream dependencies.

## Patch Rules

Do not:

- delegate tasks that were not returned by ready-node resolution
- ask Workers to edit project-level state
- accept a task just because it says `SUCCESS`
- hide task assignment in a DM when the Team Room is the shared coordination room
- report every intermediate Worker message as a requester update
