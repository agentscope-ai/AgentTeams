# WorkerFlow Runtime Contract

WorkerFlow is for one Worker to organize its own internal execution. It does
not create TeamHarness Workers, project DAGs, task rooms, requester reply
routes, or Leader acceptance state.

Use WorkerFlow only for bounded work owned by the current Worker:

- Native QwenPaw subagents for short internal parallelism.
- Dynamic workflows that create temporary QwenPaw agents from custom
  `AGENTS.md` and skill templates under the default workspace
  `subagents/` directory.
- Direct execution by the current default agent when no subagent is needed.

Do not present temporary QwenPaw agents as durable team members.

For temporary-agent fan-out, the current Worker remains the coordinator. It
should prefer `workflow_run`: pass the explicit current DM/conversation
`roomId`, the source input, and either a `subagents` fan-out plan or DAG
`nodes` plan.
`workflow_run` starts the workflow card, creates temporary agents, writes
`workflow.json`, and returns submit prompts for nodes that are ready now. The
Worker still sends those prompts through the available QwenPaw agent
communication tools, waits for upstream outputs before submitting dependent
nodes, merges the results, marks failed subagents explicitly, and deletes
temporary agents before reporting completion.

Temporary agents created with `/api/agents` have separate workspaces. When file
sharing is needed, use the run-level `shared` directory returned by
`create_temp_agent`, not the default agent workspace as the temporary
`workspaceDir`.

When WorkerFlow progress should be visible in Matrix, the current Worker may
publish one workflow card to the explicit current DM/conversation `roomId` and
edit it as phases change. WorkerFlow does not fallback to Team Room or personal
room. This does not make temporary agents TeamHarness members.
