# WorkerFlow Tools

The WorkerFlow MCP server exposes `worker_agentflow`.

Supported actions:

- `list_agents`: list QwenPaw agents through the local API.
- `list_subagents`: list runtime subagent templates under the default
  workspace `subagents/` directory.
- `create_temp_agent`: create a bounded temporary QwenPaw agent and optionally
  copy a default-workspace subagent template or external template containing
  `AGENTS.md` and `skills/`. The response includes a run-level `shared`
  directory. Pass the same `sharedRunId` to several temporary agents when they
  should share input/output files.
- `delete_temp_agent`: delete a temporary QwenPaw agent and optionally clean
  its workspace when the path is safe.
- `cleanup_shared`: remove a shared run directory after the Worker has merged
  results. It only removes paths under the default workspace
  `shared/workerflow/` root.
- `workflow_run`: start a Worker-owned dynamic workflow from a declarative
  `subagents` fan-out plan or DAG `nodes` plan. It creates the workflow card
  and temporary agents from default-workspace `subagents/<name>` templates,
  writes `workflow.json`, and returns `submitInstructions` for dependency-free nodes.
  Nodes blocked by `dependsOn` are returned as `waitingInstructions`.
- `workflow_update`: update the same Matrix workflow card with `m.replace`.
  When `steps` mark dependencies as `done`, the response can include
  `readyInstructions`; submit those prompts immediately to continue the DAG.
- `workflow_start`: show a Worker-owned workflow card in the explicit current
  Matrix DM/conversation `roomId` and return the Matrix `eventId`.
- `workflow_finish` / `workflow_fail`: finish the card as done or failed.

Workflow cards are sent as the current Worker Matrix user. Temporary agents do
not send Matrix status directly; the Worker coordinates and updates the card.
WorkerFlow does not choose or fallback to Team Room or personal room. Pass the
current DM/conversation `roomId` explicitly.

Prefer `workflow_run` for visible multi-subagent fan-out or a small DAG. Use the
lower-level `workflow_start`, `create_temp_agent`, `workflow_update`, and
cleanup actions only when the Worker needs manual control over each phase.
