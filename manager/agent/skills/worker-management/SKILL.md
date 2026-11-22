---
name: worker-management
description: Use when admin requests hand-creating or resetting a Worker, starting/stopping a Worker, managing Worker skills, enabling peer mentions, or opening a QwenPaw console. Use agentteams-find-worker only as a helper for Nacos-backed market import or when task assignment needs you to discover a suitable Worker.
---

# Worker Management

## Create a Worker

Read `references/create-worker.md` for the complete workflow. Use the admin's
existing choices; ask only for missing information that prevents creation.

- **Name:** lowercase letters, digits and hyphens, starting with a letter or digit.
- **Runtime/model:** use the requested values or the configured defaults. New
  Workers support OpenClaw, QwenPaw, Hermes, DeepSeek Harness and Codex; OpenHuman
  remains available where installed. CoPaw is for existing Workers only.
- **Role:** use the requested expertise as concise SOUL text. Pass it as a single
  literal `--soul` argument, or use an already available `--soul-file`.
- **Skills:** add explicitly requested or role-relevant skills; builtin skills
  are already included. Do not rediscover the whole skill catalog for every create.
- **Resources/MCP:** use `agt apply -f` for these structured settings; do not
  invent unsupported create flags.

For one Worker, make one foreground call:

```bash
agt create worker --name frontend-worker --runtime openclaw \
  --soul 'You are a frontend Worker. Implement requested UI changes and report results.' \
  --wait-timeout 45s -o json
```

The CLI owns readiness polling without further model turns. On timeout, report
acceptance and the returned status, then stop. Do not create again, run a new
polling loop, recreate the container, or send repeated greeting/ping messages.
For several Workers, use one `--no-wait -o json` call per Worker and acknowledge
acceptance. Use `agt worker wait-ready --name NAME --timeout 45s -o json` only
when a later operation needs the existing Worker ready.

Ordinary OpenClaw Workers default to autonomous checks off. Use the Dashboard
autonomous-check switch or `agt update worker --name NAME
--llm-heartbeat-enabled=true|false` to change that policy. Health reports and
independent Dreaming jobs are separate; do not create an environment override.

In OSS, use controller-managed Local Workers. Remote/pip flags are not part of
this path, and Edge onboarding uses its separate flow.

## Gotchas

- **Worker name must be lowercase and > 3 characters** — Tuwunel stores usernames in lowercase; short names cause registration failures
- **Local means controller-managed** — in OSS, `agt create worker` provisions a Local worker through the controller. Do not map "local mode" to Remote/pip worker flags.
- **Use `task-execution` for task collaboration**. The controller installs it as a builtin Worker skill.
- **Use `agentteams-find-worker` only for Nacos-backed market imports or Worker discovery during task assignment** — generic Worker creation and lifecycle changes stay in this skill
- **Peer mentions cause loops if not briefed** — after enabling, explicitly tell Workers to only @mention peers for blocking info, never for acknowledgments
- **Stop repeated diagnostics** — if the same lifecycle/status command returns empty, identical, or malformed output twice, stop and report what is known. Do not keep retrying `copaw channels list` or similar room probes after Worker deletion.
- **Always notify Workers to `file-sync` after writing files they need** — the 5-minute periodic sync is fallback only
- **Workers are stateless** — all state is in centralized storage. Reset = recreate config files
- **Matrix accounts persist in Tuwunel** (cannot be deleted via API) — reuse same username on reset
- **Changing a Worker's `--runtime` is a destructive operation** — the controller deletes the old container and creates a new one from the target runtime's image (openclaw/copaw/qwenpaw/hermes/openhuman). Matrix account, room, gateway consumer, MinIO data and persisted credentials are preserved; container-local state (caches, in-memory session, current task progress) is lost. Always confirm with admin first, and avoid switching runtime while the Worker is mid-task.

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Admin wants to... | Read | Key command / script |
|---|---|---|
| Create a new worker | `references/create-worker.md` | `agt create worker` |
| Start/stop/check idle workers | `references/lifecycle.md` | `scripts/lifecycle-worker.sh` |
| Install a new Skill from a ZIP attachment | `references/skills-management.md` | `scripts/install-worker-skill.sh` |
| Push/add/remove an existing Skill | `references/skills-management.md` | `scripts/push-worker-skills.sh` |
| Switch a worker's runtime (openclaw ↔ copaw ↔ qwenpaw ↔ hermes ↔ openhuman) | (this file, "Switching Runtime" below) | `scripts/update-worker-config.sh --runtime ...` |
| Open/close QwenPaw console | `references/console.md` | `scripts/enable-worker-console.sh` |
| Enable direct @mentions between workers | `references/peer-mentions.md` | `scripts/enable-peer-mentions.sh` |
| Reset a worker | `references/create-worker.md` | `agt delete worker` + `agt create worker` |
| Delete a worker (remove container) | `references/lifecycle.md` | `scripts/lifecycle-worker.sh` |

## Switching Runtime

To migrate a Worker between runtimes (e.g. openclaw → copaw, copaw → qwenpaw), use the wrapper script — it delegates to `agt update worker --runtime ...`, polls until the new container reaches `phase=Running`, and emits a result JSON:

```bash
bash /opt/agentteams/agent/skills/worker-management/scripts/update-worker-config.sh \
  --name <NAME> \
  --runtime <openclaw|copaw|qwenpaw|hermes|openhuman> \
  [--model <MODEL>] [--skills s1,s2] [--mcp-servers s1,s2]
```

What happens behind the scenes:

1. Controller writes the new `runtime` into the Worker CR's spec
2. Reconcile detects the spec change → deletes the old container → creates a new one from the target runtime's image
3. Agent config files (`openclaw.json`, `AGENTS.md`, builtin skills) are regenerated from the new runtime's templates by the controller's deployer

Constraints:

- `--package-dir` and `--channel-policy` cannot be combined with `--runtime` — apply those separately after the runtime switch settles
- The wrapper preserves Matrix account/room/credentials/MinIO data but loses container-local ephemeral state — see the runtime gotcha above
