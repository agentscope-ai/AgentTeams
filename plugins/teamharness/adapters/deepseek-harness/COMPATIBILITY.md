# DeepSeek Harness compatibility report

## Question

Can an unmodified DeepSeek Harness release act as an AgentTeams-managed Worker: load TeamHarness role context and Skills, use the existing MCP server, call a real model, communicate through Matrix, use MinIO, and recover after its Pod is replaced?

## Environment

- AgentTeams base: `223ddc2b8073e4c8b93bcbb15e1d717f196c04d9`
- DeepSeek Harness: `dsh-v0.1.1-rc.2` / `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`
- Node.js: `v22.21.0`
- pnpm: `11.7.0`
- Python: `3.12.10`
- Host: Windows

## Command

```powershell
pwsh -NoProfile -File plugins/teamharness/adapters/deepseek-harness/smoke.ps1 -DshRoot ../deepseek-harness-rc2
```

## Result

The local adapter smoke and all live checks completed with exit code 0 on 2026-08-24.

- DSH installed the packed adapter bundle into an isolated profile.
- The assembled DSH system prompt contained the TeamHarness team contract, Worker role prompt, and member facts read from the controller-shaped `runtime.yaml` fixture.
- The adapter read `plugin.yaml` and materialized only the five Skills allowed for the Worker role: `mcporter`, `find-skills`, `communication`, `file-sharing`, and `task-execution`.
- Leader-only Skills and the unregistered `organization` directory were not exposed.
- DSH registered `mcp__teamharness__health` and `mcp__teamharness__filesync` through its public MCP client.
- Calling `mcp__teamharness__health` returned `{"ok":true,"tool":"health","status":"ok"}`.
- Calling `mcp__teamharness__filesync` with `dryRun: true` resolved a real file path to an `mc cp` command without contacting object storage.
- The Worker-only runtime config hid `mcp__teamharness__message` from DSH's tool registry.
- A real model turn returned the member identity injected from `runtime.yaml`.
- The DSH `agentDefaultModel` service used the model selected from `runtime.yaml`; the managed image also preferred the projected provider gateway route over the cluster-wide fallback.
- A live TeamHarness MCP run sent and read back a Matrix event, then pushed, statted, read back, and removed a MinIO object.
- The Controller created a `deepseek-harness` Worker with the runtime-specific image and projected runtime contract.
- A real Matrix message reached the DSH Worker, produced a model reply, and returned to the same Matrix room.
- The bridge persisted its Matrix sync cursor to object storage. After the Worker Pod was deleted, the Controller recreated it and the replacement replied to a second message without replaying the first.
- A stable DSH session is now assigned to each Matrix room. A second message continued the first conversation after the Team Leader Pod was replaced and its session was restored from object storage.
- Matrix image/file events are downloaded into a path-confined Workspace inbox. Files created or changed in the Workspace outbox are uploaded and returned as Matrix file/image messages.
- Completed Matrix event IDs and pending answers are persisted. Matrix delivery uses deterministic transaction IDs, skips completed events after restart, and retries transient delivery failures from a persisted answer without running that model turn again. Failed DSH executions use a new deterministic message ID on the next attempt, so they perform a real follow-up turn instead of replaying the previous error outcome.
- A real Team containing a DSH Team Leader and DSH Worker received the same Team-room event with the correct role-specific runtime context. The same run covered session recovery and a real file round trip.
- Installing the same package again left exactly one TeamHarness MCP row and the second runtime smoke also passed.
- Removing the package removed its dependency and bundle rows from the profile.
- The successful run removed its temporary DSH home and workspace.

## Remaining limits

The bridge processes Matrix events sequentially within one Worker, and the live storage checks cover MinIO rather than OSS. The image is pinned to one DSH release candidate, so later DSH releases need a compatibility run before the pin is changed. This bridge does not implement Matrix E2EE and fails fast when `AGENTTEAMS_MATRIX_E2EE=1`; use unencrypted rooms for this experimental runtime.

## Design answer

The managed runtime is viable without patching DSH or running it inside QwenPaw. The Controller remains responsible for Worker state and `runtime.yaml`; the runtime image owns the DSH process and Matrix loop. The adapter builds a role-specific Skill root from `plugin.yaml`, because exposing the whole TeamHarness Skills tree would leak Leader-only Skills to Workers.
