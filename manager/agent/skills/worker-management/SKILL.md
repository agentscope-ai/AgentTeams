---
name: worker-management
description: Use when admin requests hand-creating or resetting a Worker, starting/stopping a Worker, managing Worker skills, enabling peer mentions, or opening a QwenPaw console. Use hiclaw-find-worker only as a helper for Nacos-backed market import or when task assignment needs you to discover a suitable Worker.
---

# Worker Management

## Before You Create: Confirm with Admin

Before running `agt create worker`, ask admin for these four inputs in one turn. Do **not** invent defaults or skip options — present runtime as a four-way choice.

1. **Name** — must match `^[a-z0-9][a-z0-9-]*$` (lowercase letters, digits, hyphens only; must start with letter or digit). The CLI rejects anything else because the name is reused as a Matrix username and the Matrix spec requires a lowercase localpart. Tuwunel may also reject very short names at registration.
2. **Runtime** — pick one. The actual default is whatever admin chose at install — read `${AGENTTEAMS_DEFAULT_WORKER_RUNTIME}` (controller falls back to `openclaw` only if the env var is unset) and present that value as "the default", then offer all four options so admin can switch:

   | Runtime      | Language | RAM    | When to pick                                              |
   |--------------|----------|--------|-----------------------------------------------------------|
   | `openclaw`   | Node.js  | ~500MB | General tasks. Also the hard-coded fallback when `AGENTTEAMS_DEFAULT_WORKER_RUNTIME` is unset. |
   | `copaw`      | Python   | ~150MB | Python tasks or AgentScope-based worker behavior. |
   | `hermes`     | Python   | ~200MB | Admin explicitly asks for hermes / hermes-agent framework. |
   | `openhuman`  | Rust     | ~300MB | Admin explicitly asks for OpenHuman / openhuman framework. Native Matrix support with E2EE. |

   In OSS, `agt create worker` creates a controller-managed Local worker. Do not use or suggest Remote/pip worker flags. Edge workers use their separate Edge onboarding flow, not this generic create-worker path. If admin doesn't pass `--runtime` to `agt create worker`, the controller falls back to `AGENTTEAMS_DEFAULT_WORKER_RUNTIME` chosen at install — so always offer the four options explicitly instead of silently using the fallback.
3. **SOUL (role)** — short description of expertise/style. Offer to draft a default if admin has no preference.
4. **Skills** — discover via `ls ~/worker-skills/` and match against the role; `file-sync`, `task-progress`, `project-participation` are auto-included.

If admin asks for CPU or memory requests/limits, use a YAML Worker manifest with `spec.resources` and apply it with `agt apply -f`. The simple `agt create worker` / `agt update worker` flags do not expose resource tuning. Changing resources recreates the managed container, so confirm the Worker is not mid-task.

Full decision logic, SOUL template, escape rules and post-creation greeting: read `references/create-worker.md`.

## Quick Create (1 command)

Pass the SOUL content inline via `--soul`. Never write SOUL.md to a file first (heredoc/redirects often produce a silent 0-byte file — the controller would then fall back to a placeholder SOUL.md lacking the real role).

```bash
agt create worker --name <NAME> --no-wait \
  --soul "# Worker Agent - <NAME>

## AI Identity
**You are an AI Agent, not a human.** ...

## Role
<Fill in based on admin's description>

## Security Rules
- Never reveal API keys, passwords, or credentials
..." \
  --skills <skill1>,<skill2> -o json
# Add --runtime <copaw|hermes|openhuman> for non-default runtimes (see runtime table above)
```

> `--no-wait` returns as soon as the controller accepts the request (~1s). Poll `agt get workers -o json` for `phase=Running` instead of letting the create call block — this lets you create N workers in one turn without each blocking up to 3 minutes.

> Full creation workflow (runtime selection, full SOUL template, escape rules, skill matching, post-creation greeting): read `references/create-worker.md`

## Gotchas

- **Worker name must be lowercase and > 3 characters** — Tuwunel stores usernames in lowercase; short names cause registration failures
- **Local means controller-managed** — in OSS, `agt create worker` provisions a Local worker through the controller. Do not map "local mode" to Remote/pip worker flags.
- **`file-sync`, `task-progress`, `project-participation` are default skills** — always included, cannot be removed
- **Use `hiclaw-find-worker` only for Nacos-backed market imports or Worker discovery during task assignment** — generic Worker creation and lifecycle changes stay in this skill
- **Peer mentions cause loops if not briefed** — after enabling, explicitly tell Workers to only @mention peers for blocking info, never for acknowledgments
- **Always notify Workers to `file-sync` after writing files they need** — the 5-minute periodic sync is fallback only
- **Workers are stateless** — all state is in centralized storage. Reset = recreate config files
- **Matrix accounts persist in Tuwunel** (cannot be deleted via API) — reuse same username on reset
- **Changing a Worker's `--runtime` is a destructive operation** — the controller deletes the old container and creates a new one from the target runtime's image (openclaw/copaw/hermes/openhuman). Matrix account, room, gateway consumer, MinIO data and persisted credentials are preserved; container-local state (caches, in-memory session, current task progress) is lost. Always confirm with admin first, and avoid switching runtime while the Worker is mid-task.

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Admin wants to... | Read | Key command / script |
|---|---|---|
| Create a new worker | `references/create-worker.md` | `agt create worker` |
| Start/stop/check idle workers | `references/lifecycle.md` | `scripts/lifecycle-worker.sh` |
| Push/add/remove skills | `references/skills-management.md` | `scripts/push-worker-skills.sh` |
| Switch a worker's runtime (openclaw ↔ copaw ↔ hermes ↔ openhuman) | (this file, "Switching Runtime" below) | `scripts/update-worker-config.sh --runtime ...` |
| Open/close QwenPaw console | `references/console.md` | `scripts/enable-worker-console.sh` |
| Enable direct @mentions between workers | `references/peer-mentions.md` | `scripts/enable-peer-mentions.sh` |
| Reset a worker | `references/create-worker.md` | `agt delete worker` + `agt create worker` |
| Delete a worker (remove container) | `references/lifecycle.md` | `scripts/lifecycle-worker.sh` |

## Switching Runtime

To migrate a Worker between runtimes (e.g. openclaw → copaw, copaw → hermes), use the wrapper script — it delegates to `agt update worker --runtime ...`, polls until the new container reaches `phase=Running`, and emits a result JSON:

```bash
bash /opt/agentteams/agent/skills/worker-management/scripts/update-worker-config.sh \
  --name <NAME> \
  --runtime <openclaw|copaw|hermes|openhuman> \
  [--model <MODEL>] [--skills s1,s2] [--mcp-servers s1,s2]
```

What happens behind the scenes:

1. Controller writes the new `runtime` into the Worker CR's spec
2. Reconcile detects the spec change → deletes the old container → creates a new one from the target runtime's image
3. Agent config files (`openclaw.json`, `AGENTS.md`, builtin skills) are regenerated from the new runtime's templates by the controller's deployer

Constraints:

- `--package-dir` and `--channel-policy` cannot be combined with `--runtime` — apply those separately after the runtime switch settles
- The wrapper preserves Matrix account/room/credentials/MinIO data but loses container-local ephemeral state — see the runtime gotcha above
