---
name: workerflow-internal-workflow
description: Use when a QwenPaw-backed Worker needs to decide whether to do work directly, use native subagents for internal parallelism, or create a temporary QwenPaw agent with a custom AGENTS.md and skills.
---

# Worker Internal Workflow

Use this skill before splitting work inside a single Worker. WorkerFlow is not
TeamHarness delegation.

## Decision

| Need | Path |
| --- | --- |
| Current Worker can do it directly | No subagent |
| Same Worker needs short parallel inspection | QwenPaw native subagent |
| Same Worker needs visible fan-out to custom roles, workspaces, `AGENTS.md`, or skills | Dynamic workflow via `worker_agentflow` `workflow_run` |
| Same Worker needs one manually controlled temporary subagent | `create_temp_agent` directly |
| Work needs a persistent team member or Leader acceptance | Do not use WorkerFlow; use the team workflow |

## Native Subagent

Use QwenPaw native subagents for short internal parallelism. The current Worker
still owns the final result.

Do not create a temporary agent when no custom prompt, workspace, or skill set
is required.

## Temporary QwenPaw Agent

Use `worker_agentflow` with `workflow_run` when a custom AgentSpec-style
template has already been placed in the default QwenPaw workspace:

```text
<QWENPAW_WORKING_DIR>/workspaces/default/subagents/<role>/
  AGENTS.md
  PROFILE.md        # optional
  SOUL.md           # optional
  skills/
    <skill-id>/
      SKILL.md
```

Use `list_subagents` first when you need to discover the templates available in
the default workspace. Use `create_temp_agent` directly only when you need
manual control outside a dynamic workflow.

Subagent template rules:

- `AGENTS.md` should tell the temporary agent to begin analysis immediately.
- `AGENTS.md` should forbid greetings, self-introductions, and capability
  summaries.
- `AGENTS.md` should define the subagent's focus area so the Worker can send the
  complete source input instead of manually slicing the input.
- When multiple subagent results will be merged, each `AGENTS.md` should require
  the same fixed output shape.

Dynamic workflow lifecycle:

1. Call `workflow_run` with the explicit current DM/conversation `roomId`,
   `title`, source `input`, optional `merge.instruction`, and either
   `subagents` or DAG `nodes`.
2. Each subagent or node must name a default-workspace template with `subagent`
   and a bounded `task`.
3. `workflow_run` starts the Matrix card, creates `tmp-...` subagents, creates the
   run-level shared directory, stores `workflow.json`, and returns
   `submitInstructions` for nodes that are ready now.
4. Send each returned `submitPrompt` to its `agentId` using the available
   QwenPaw agent communication tools.
5. When a subagent finishes, call `workflow_update` with a `steps` row whose
   `id` matches the node id, `status` is `done`, and `summary` contains the
   short result.
6. For DAG `nodes`, inspect every `workflow_update` response. If it returns
   `readyInstructions`, immediately send each returned `submitPrompt` to its
   `agentId`.
7. Use `workflow_update` at meaningful phase changes: submitted, running,
   retrying, merging, cleanup, done, or failed.
8. Retry a timed-out subagent once when the task is still useful.
9. Mark subagents that still fail as missing or failed in the merged result.
10. Merge subagent results into the current Worker's own output.
11. Delete every temporary agent with `delete_temp_agent`.
12. Call `workflow_finish` or `workflow_fail`.
13. After merging, either keep the shared run directory as evidence or remove it
    with `cleanup_shared`.

Example `workflow_run` input:

```json
{
  "action": "workflow_run",
  "roomId": "!current-dm:example.test",
  "runId": "run-review-auth",
  "title": "Review auth module",
  "input": "Review auth.py for this task...",
  "subagents": [
    {
      "id": "security",
      "subagent": "security-reviewer",
      "task": "Check authentication and authorization risks."
    },
    {
      "id": "tests",
      "subagent": "test-reviewer",
      "task": "Check test coverage gaps."
    }
  ],
  "merge": {
    "instruction": "Return one severity-ordered finding list."
  }
}
```

DAG nodes example:

```json
{
  "action": "workflow_run",
  "roomId": "!current-dm:example.test",
  "runId": "pig-diagnosis-b001",
  "title": "猪病诊断报告生成",
  "input": "批次号 B001 的猪病诊断请求...",
  "nodes": [
    {
      "id": "base_info",
      "name": "基础信息梳理",
      "role": "基础信息梳理",
      "subagent": "base-info-agent",
      "task": "处理基础信息。"
    },
    {
      "id": "feed_cough",
      "name": "采食与咳喘指标",
      "role": "采食与咳喘指标",
      "subagent": "feed-cough-agent",
      "task": "分析采食及咳嗽波动。"
    },
    {
      "id": "pathogen",
      "name": "病原线索分析",
      "role": "病原线索分析",
      "subagent": "pathogen-agent",
      "task": "分析病原及亚型。"
    },
    {
      "id": "disease_analysis",
      "name": "疾病综合研判",
      "role": "疾病综合研判",
      "subagent": "disease-analysis-agent",
      "task": "综合基础信息、采食咳嗽和病原结果，判断猪病。",
      "dependsOn": ["base_info", "feed_cough", "pathogen"]
    },
    {
      "id": "medication",
      "name": "用药与处置措施",
      "role": "用药与处置措施",
      "subagent": "medication-measures-agent",
      "task": "基于猪病分析生成药物及措施方案。",
      "dependsOn": ["disease_analysis"]
    },
    {
      "id": "diagnosis_advice",
      "name": "诊断建议",
      "role": "诊断建议",
      "subagent": "diagnosis-advice-agent",
      "task": "基于猪病分析生成诊断建议。",
      "dependsOn": ["disease_analysis"]
    },
    {
      "id": "report",
      "name": "最终报告汇总",
      "role": "最终报告汇总",
      "subagent": "report-agent",
      "task": "汇总上游结果生成最终诊断报告。",
      "dependsOn": ["medication", "diagnosis_advice"]
    }
  ]
}
```

For DAG workflows, `workflow_run` creates every node's temporary agent. Only
dependency-free nodes appear in `submitInstructions`. Dependent nodes appear in
`waitingInstructions`. The current Worker is the scheduler: submit ready nodes,
wait for their replies, call `workflow_update` with done `steps`, then submit
any downstream `readyInstructions` returned by that update. WorkerFlow uses the
done step ids as the dependency-completion signal and appends upstream summaries
to newly unblocked submit prompts.

File sharing:

- Native QwenPaw subagents share the current Worker's workspace.
- Temporary QwenPaw agents created through `/api/agents` have separate
  workspaces. Do not point their `workspaceDir` at the default workspace.
- `workflow_run` and `create_temp_agent` expose a run-level shared directory:
  `<default-workspace>/shared/workerflow/<sharedRunId>/`.
- Put input files under `shared/inputs/`.
- Each subagent writes only under `shared/outputs/<agent-id>/`.
- The subagent workspace contains `.workerflow/shared.json` and a `shared`
  symlink for discovery, but prompts should still include the absolute
  `shared.path`, `shared.inputs`, and `shared.output` returned by the tool.
- Do not place the shared directory inside a temporary agent workspace, because
  `cleanupWorkspace` may delete that workspace.

Workflow visibility:

- The current Worker is the Matrix sender for WorkerFlow workflow cards.
- Temporary agents do not send Matrix status directly.
- Prefer `workflow_run` for visible multi-subagent work; it creates the first
  Matrix `m.notice` card and records the returned `eventId`.
- `workflow_run` and `workflow_start` require an explicit current
  DM/conversation `roomId`.
  WorkerFlow does not fallback to Team Room or personal room.
- Use `workflow_update`, `workflow_finish`, and `workflow_fail` to edit the same
  card through Matrix `m.replace`.
- The card state is also stored in
  `<default-workspace>/shared/workerflow/<run-id>/workflow.json`.
- Show phase-level changes only: spawning subagents, running, retrying, merging,
  cleanup, done, or failed.
- Do not expose full prompts, secrets, raw large inputs, or tool traces in the
  card. Use shared paths and short summaries.

Fan-out rules:

- Send the full source input to each subagent unless there is a clear token,
  privacy, or cost reason to narrow it.
- Let each subagent extract the subset relevant to its `AGENTS.md`.
- Request a fixed output shape for all subagents when the results need
  programmatic or low-friction merging.
- Keep a small ledger of created temporary agent ids so cleanup can run even
  when one subagent fails.
- Keep the shared run id in the same ledger when file sharing is used.

Rules:

- Temporary agents are implementation details of the current Worker.
- Always use `tmp-` ids for temporary agents.
- Do not store temporary agent ids as TeamHarness workers.
- Delete temporary agents after the bounded task completes or fails; treat this
  as a finally step.
- If the work must survive the current Worker run, use a durable team workflow
  instead.
