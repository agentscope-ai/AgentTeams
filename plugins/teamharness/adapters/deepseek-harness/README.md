# TeamHarness DeepSeek Harness adapter

This adapter lets a stock DeepSeek Harness release load Controller-projected TeamHarness context and role-specific Skills, connect to the existing TeamHarness MCP server, and resume a caller-supplied DSH session without changing DSH itself. The managed Matrix bridge supplies one stable session ID per room and a deterministic message ID per Matrix event.

This adapter is installed directly into the managed Worker's dedicated headless DSH profile. The runtime is experimental while it remains pinned to a DeepSeek Harness release candidate; the Controller-facing `deepseek-harness` runtime name and persisted state format are stable within AgentTeams.

The smoke run is pinned to DeepSeek Harness `dsh-v0.1.1-rc.2` at commit `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`. Build that checkout first, then run:

```powershell
pwsh -File plugins/teamharness/adapters/deepseek-harness/smoke.ps1 -DshRoot ../deepseek-harness-rc2
```

The script uses a temporary `DSH_HOME`, installs this directory as a DSH bundle, boots both Worker and standalone compositions, verifies the prompt/Skill/MCP path, removes the bundle, and deletes the temporary home after a successful run.

Additional live checks are documented in `deepseek-harness/README.md`. They cover a real model turn, Matrix text and file delivery, MinIO-backed session recovery, Controller-driven Pod replacement, and a real Team Leader/Worker scenario.

The runtime config `member` block (see `fixtures/runtime.yaml`) also carries the trusted review identity: the MCP server resolves the transition-actor account from `member.name` -> `member.runtimeName` -> `matrixUserId` localpart (overridable via the `AGENTTEAMS_WORKER_NAME` env), never from a tool payload field. A payload `account_id` is provenance only — it is used when no trusted identity exists (local / test runs) and is never authorization (#1467).
