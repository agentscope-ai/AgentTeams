# CoPaw Subsystem Navigation Guide

This file helps AI Agents (and human developers) quickly understand the CoPaw subsystem of AgentTeams and find relevant code. It complements the root [AGENTS.md](../AGENTS.md); read that first for overall project structure.

## What is CoPaw in AgentTeams

CoPaw is a Python-based Agent runtime used in AgentTeams as an alternative to OpenClaw (Node.js). It appears in two places:

- **CoPaw Worker** — the Python Worker runtime (always CoPaw, no OpenClaw alternative for this binary). Packaged as `agentteams/copaw-worker:latest`.
- **Manager CoPaw runtime** — opt-in via `AGENTTEAMS_MANAGER_RUNTIME=copaw`; Manager then runs its own agent loop in CoPaw instead of OpenClaw. Packaged as `agentteams/manager-copaw:latest`.

AgentTeams does **not** vendor upstream CoPaw — it ships a fork based on the `feat/lite-copaw-worker-v1.0.0` branch. The fork's source lives **outside this repo** as a sibling checkout; its exact path depends on the developer's local setup. A Matrix channel implementation is vendored in-tree (`copaw/src/matrix/`) until the upstream PR lands.

## Scope of this guide

| In scope | Out of scope |
|---|---|
| `copaw/` (Python Worker package, Dockerfile, scripts, tests) | OpenClaw Worker (`worker/`) |
| `copaw/src/matrix/` (shared Matrix channel) | Manager OpenClaw runtime (`manager/agent/worker-agent/`) |
| `manager/Dockerfile.copaw` | Higress / Tuwunel / MinIO themselves |
| `manager/agent/copaw-manager-agent/`, `copaw-worker-agent/` (CoPaw-runtime agent content) | Upstream CoPaw source code (read that repo's AGENTS.md) |

## Subsystem Structure

```
copaw/                              # CoPaw Worker package (everything Worker-side)
├── Dockerfile                      # Worker image build (Python 3.11)
├── pyproject.toml                  # Python deps for copaw-worker
├── scripts/
│   ├── copaw-worker-entrypoint.sh  # Container entrypoint
│   └── patch_*_lazy.py             # Runtime monkey-patches for upstream libs
├── src/
│   ├── copaw_worker/               # The Worker Python package
│   │   ├── cli.py                  # CLI entrypoint
│   │   ├── config.py               # WorkerConfig dataclass
│   │   ├── sync.py                 # MinIO ↔ local file sync (FileSync class)
│   │   ├── bridge.py               # openclaw.json → CoPaw native config
│   │   └── worker.py               # Worker orchestration + startup
│   └── matrix/                     # Matrix channel (Manager + Worker共用, vendored)
│       ├── channel.py
│       ├── config.py
│       └── README.md               # Why this is vendored; migration plan
└── tests/                          # Python unit tests (pytest)

manager/Dockerfile.copaw            # Manager CoPaw runtime image
manager/agent/copaw-manager-agent/  # Manager agent content when runtime=copaw
│                                   # (overlays manager/agent/ at upgrade-builtins time)
├── AGENTS.md                       # Manager behavior (CoPaw-specific)
└── HEARTBEAT.md
manager/agent/copaw-worker-agent/   # CoPaw Worker builtin content (pushed by Controller to Worker L1)
├── AGENTS.md
└── skills/
```

## Core Concept: MinIO → OpenClaw Config → CoPaw Native Config

This is the single most important thing to understand when hacking on CoPaw. **Every operation in `copaw_worker/` ultimately serves this conversion chain.**

AgentTeams stores Agent specs in MinIO using **a runtime-agnostic OpenClaw-style format** (`openclaw.json` + `SOUL.md` + `AGENTS.md` + `skills/` + ...). This is the Controller's single source of truth, identical in shape whether the Worker will run OpenClaw or CoPaw.

For CoPaw runtime, the Worker must translate this into **CoPaw's native configuration layout** (`config.json` + `providers.json` + `agent.json` + `skill_pool/` + `workspaces/default/`) before the CoPaw agent can start. This translation is what `bridge.py` and the propagate logic in `worker.py` exist for.

```
┌─────────────────────────────┐
│ MinIO (agentteams-storage)      │  Controller writes; Worker pulls.
│ agents/{name}/              │  Runtime-agnostic OpenClaw-style spec.
│   openclaw.json             │
│   SOUL.md, AGENTS.md        │
│   skills/                   │
│   config/mcporter.json      │
│   credentials/              │
└──────────────┬──────────────┘
               │ pull: startup mirror_all / explicit shared pull (sync.py)
               │ push: FileSync.push_local                  (sync.py)
               ▼
┌─────────────────────────────┐
│ Local workspace             │  /root/.agentteams-worker/{name}/
│ (OpenClaw-style, verbatim)  │  Same layout as MinIO. For OpenClaw Worker, this
│                             │  is the final workspace; for CoPaw, it's the
│                             │  persistence layer, not what the agent reads.
└──────────────┬──────────────┘
               │ bridge: openclaw.json → CoPaw configs     (bridge.py)
               │ propagate: copy/link L2 files into CoPaw  (worker.py)
               │ save: write back agent edits              (sync.py, currently
               │                                            folded into push)
               ▼
┌─────────────────────────────┐
│ CoPaw runtime space         │  {local}/.copaw/
│ (CoPaw native layout)       │  What the CoPaw process actually reads.
│   config.json               │
│   providers.json            │
│   skill_pool/               │
│   workspaces/default/       │
│     SOUL.md, AGENTS.md      │
│     agent.json              │
│     skills/                 │
│     sessions/, memory/      │
└─────────────────────────────┘
```

### Field-level conversion map

`bridge.py` converts `openclaw.json` sections into three CoPaw native files:

| `openclaw.json` section | CoPaw output file | Merge strategy | Notes |
|---|---|---|---|
| `channels.matrix` | `.copaw/config.json` → `channels.matrix` | partial merge, overwrite | |
| `agents.running` | `.copaw/config.json` → `agents.running` | append | preserves user-defined entries |
| `security` | `.copaw/config.json` → `security` | setdefault | |
| `models.providers` | `.copaw.secret/providers.json` → `custom_providers` + `active_llm` | **full overwrite** | port remap applied; Controller owns LLM routing |
| `agent_id`, `channels`, `running`, `security`, `system_prompt_files` | `.copaw/workspaces/default/agent.json` | partial merge | |

### Side effects of `bridge()` (read before changing it)

`bridge()` is **not a pure function** — in addition to writing the three config files, it also mutates the Python process:

- Sets `os.environ["COPAW_WORKING_DIR"]`
- Monkey-patches `copaw.constant.{WORKING_DIR,SECRET_DIR,ACTIVE_SKILLS_DIR,...}`
- Monkey-patches `copaw.providers.store._PROVIDERS_JSON`
- Monkey-patches `copaw.envs.store._BOOTSTRAP_*`
- Copies `providers.json` into `.copaw.secret/` (CoPaw reads from the secret dir, not `.copaw/`)

These are required because upstream CoPaw hardcodes paths at import time. Unit tests that call `bridge()` multiple times across workspaces must reset env vars and reload affected modules.

### Propagate vs bridge vs save

Three verbs are used consistently in code and docs — keep them straight:

| Verb | Direction | What moves |
|---|---|---|
| `bridge` | openclaw.json → CoPaw config files | **structured config** (config.json / providers.json / agent.json) |
| `propagate` | L2 plain files → CoPaw workspace | `SOUL.md`, `AGENTS.md`, `skills/`, `mcporter.json` |
| `save` | CoPaw workspace → L2 | agent-edited files (`memory/`, `sessions/`, updated `SOUL.md`/`AGENTS.md`) |
| `pull` / `push` | MinIO ↔ L2 | everything under `agents/{name}/` (via `mc mirror` / `mc cp`) |

> Older comments in `sync.py` call `save` "Inner→Outer" (L526-549). Treat that as legacy; prefer `save` in new code and comments.

The authoritative design for the sync/bridge/propagate chain is [`docs/copaw-bridge-sync-design-v2.md`](../docs/copaw-bridge-sync-design-v2.md). Consult it when touching the conversion chain itself, not for day-to-day edits.

## Key Entry Points

### To modify the conversion chain
- [copaw/src/copaw_worker/bridge.py](src/copaw_worker/bridge.py) — `bridge_controller_to_copaw()`; template create + controller-field overlay. Templates live in [copaw/src/copaw_worker/templates/](src/copaw_worker/templates/).
- [copaw/src/copaw_worker/sync.py](src/copaw_worker/sync.py) — `FileSync` class: `mirror_all` / `push_local` / shared-file helpers; `push_loop`
- [copaw/src/copaw_worker/worker.py](src/copaw_worker/worker.py) — `Worker.start()` orchestrates startup mirror → bridge → runtime push loop

### To modify the Worker container
- [copaw/Dockerfile](Dockerfile) — Python 3.11 base, pip install `.`
- [copaw/scripts/copaw-worker-entrypoint.sh](scripts/copaw-worker-entrypoint.sh) — container startup
- [copaw/pyproject.toml](pyproject.toml) — Python dependencies (rebuild image on change)

### To modify the Manager CoPaw container
- [manager/Dockerfile.copaw](../manager/Dockerfile.copaw) — Manager image with CoPaw runtime
- [manager/scripts/init/start-manager-agent.sh](../manager/scripts/init/start-manager-agent.sh) — branches on `AGENTTEAMS_MANAGER_RUNTIME`
- [manager/scripts/init/upgrade-builtins.sh](../manager/scripts/init/upgrade-builtins.sh) — overlays `copaw-manager-agent/` on top of `manager/agent/` when runtime=copaw

### To modify the shared Matrix channel
- [copaw/src/matrix/channel.py](src/matrix/channel.py) — Manager + Worker both use this; behavior diverges through config, not code
- [copaw/src/matrix/README.md](src/matrix/README.md) — why this is vendored and how to migrate when upstream lands
- **Rebuild both `copaw-worker` and `agentteams-manager-copaw` images after any change here** — otherwise the two sides drift

### To modify CoPaw agent behavior
- [manager/agent/copaw-manager-agent/AGENTS.md](../manager/agent/copaw-manager-agent/AGENTS.md) — Manager behavior when runtime=copaw
- [manager/agent/copaw-manager-agent/HEARTBEAT.md](../manager/agent/copaw-manager-agent/HEARTBEAT.md)
- [manager/agent/copaw-worker-agent/AGENTS.md](../manager/agent/copaw-worker-agent/AGENTS.md) — CoPaw Worker builtin content
- [manager/agent/copaw-worker-agent/skills/](../manager/agent/copaw-worker-agent/skills/) — Worker builtin skills (CoPaw variant)
- `SOUL.md` and `TOOLS.md` are **shared** across runtimes — edit once in `manager/agent/`, do not fork
- When editing `manager/agent/AGENTS.md` or `HEARTBEAT.md`, always diff the `copaw-manager-agent/` counterparts

### To run tests
- [copaw/tests/](tests/) — Python unit tests (`cd copaw && pytest`)
- [tests/](../tests/) — repo-level integration suite (`make test`)

## Build, Install, Test

AgentTeams supports two local deployment shapes. Use Embedded for fast agent-behavior iteration; use In-cluster for anything that touches CRs, Controller reconcile, Helm, or PVCs. For log inspection and triage, see the `## Debugging` section below.

### Common prerequisites

```bash
export AGENTTEAMS_LLM_API_KEY=<your-llm-api-key>
export AGENTTEAMS_MANAGER_RUNTIME=copaw          # default is openclaw; must be set explicitly for CoPaw

# Optional: CMS tracing (OpenTelemetry → Aliyun CMS)
export AGENTTEAMS_CMS_TRACES_ENABLED=true
export AGENTTEAMS_CMS_ENDPOINT=<cms-endpoint>
export AGENTTEAMS_CMS_LICENSE_KEY=<cms-license-key>
export AGENTTEAMS_CMS_PROJECT=<cms-project>
export AGENTTEAMS_CMS_WORKSPACE=<cms-workspace>
```

### Embedded mode (local Docker, dual-container)

```bash
# Build + install (one shot)
AGENTTEAMS_LLM_API_KEY=<key> AGENTTEAMS_MANAGER_RUNTIME=copaw make install-embedded

# The install output prints Element Web URL + admin password — record them.

# Uninstall (keeps volumes)
make uninstall-embedded

# Full wipe (uninstall + delete volumes)
make uninstall-embedded && \
  docker volume ls | grep hiclaw | awk '{print $2}' | xargs -r docker volume rm
```

### In-cluster mode (kind + Helm)

```bash
# First-time full install (build all images + create cluster + deploy)
AGENTTEAMS_LLM_API_KEY=<key> AGENTTEAMS_MANAGER_RUNTIME=copaw ./hack/local-k8s-up.sh

# Redeploy without rebuilding (images already loaded into kind)
AGENTTEAMS_LLM_API_KEY=<key> AGENTTEAMS_MANAGER_RUNTIME=copaw AGENTTEAMS_SKIP_BUILD=1 make local-k8s-up

# Tear down cluster entirely
kind delete cluster --name agentteams

# OpenClaw all-in-one variant (for reference / comparison)
AGENTTEAMS_LLM_API_KEY=<key> AGENTTEAMS_BUILD_K8S_IMAGE=1 bash hack/local-k8s-up.sh
```

Incremental rebuild of a single image and reload into kind:

```bash
make build-manager-copaw
kind load docker-image agentteams/manager-copaw:latest --name agentteams

make build-copaw-worker
kind load docker-image agentteams/copaw-worker:latest --name agentteams

make build-agentteams-controller
kind load docker-image agentteams/agentteams-controller:latest --name agentteams
kubectl rollout restart deployment/agentteams-controller -n agentteams
```

Force Manager/Worker to pick up the new image (Controller reconciles on `status.phase=Pending`):

```bash
kubectl delete po -n agentteams agentteams-manager

kubectl patch manager default -n agentteams --subresource=status --type merge \
  -p '{"status":{"phase":"Pending","message":"force rebuild"}}'

kubectl patch worker <worker-name> -n agentteams --subresource=status --type merge \
  -p '{"status":{"phase":"Pending","message":"force rebuild"}}'
```

Port-forwards (for browser access / debugging):

```bash
kubectl port-forward -n agentteams svc/higress-gateway    18080:80    &
kubectl port-forward -n agentteams svc/hiclaw-element-web 18081:8080  &
kubectl port-forward -n agentteams pod/agentteams-manager     18799:18799 &
kubectl port-forward -n agentteams pod/agentteams-worker-<name> 18112:8088 &
```

Purge Matrix room history (when the Manager gets stuck replaying a poisoned message):

- `<admin_token>`: read from `/root/.creds/admin_token` inside `agentteams-manager`, or reuse the token issued during install.
- `<room_id>`: copy from Element Web (Room → Settings → Advanced), or list via `kubectl exec ... -- curl .../_matrix/client/v3/joined_rooms`.

```bash
kubectl exec -n agentteams agentteams-manager -- curl -s -X POST \
  "http://hiclaw-tuwunel.hiclaw.svc.cluster.local:6167/_synapse/admin/v1/purge_history/<room_id>" \
  -H "Authorization: Bearer <admin_token>" \
  -H "Content-Type: application/json" \
  -d '{"delete_local_events": true}'
```

### Code hot-reload (both modes)

The `hiclaw-debug` skill provides scripts for fast iteration without rebuilding images:

| Change | Script | Effect |
|---|---|---|
| `copaw/src/copaw_worker/*.py` | `.claude/skills/hiclaw-debug/scripts/dev-sync-copaw.sh` | syncs Python source into running container |
| `manager/agent/**` (SOUL.md, skills, etc.) | `.claude/skills/hiclaw-debug/scripts/dev-sync-agent.sh` | syncs agent content |

Full workflow: [`.claude/skills/hiclaw-debug/SKILL.md`](../.claude/skills/hiclaw-debug/SKILL.md).

**Image rebuild is required when changing:** `copaw/Dockerfile`, `copaw/pyproject.toml`, `copaw/scripts/`, `manager/Dockerfile.copaw`, `manager/scripts/init/`, `copaw/src/matrix/` (**both images**), or `openclaw-base/`.

### Testing

```bash
cd copaw && pytest                    # Python unit tests (bridge merge, sync whitelist, etc.)
cd copaw && pytest tests/test_bridge.py   # single file
make test                             # repo integration suite (10 cases, see tests/)
```

Place new Python tests under `copaw/tests/` as `test_<module>.py`. Priority targets for new tests: `bridge()` merge semantics, `FileSync` pull/push allowlists, workspace path calculation.

## Debugging

CoPaw runs across multiple containers (Manager + N×Worker + Tuwunel + Higress + MinIO). A single symptom — e.g. "agent is not replying" — can have a dozen causes at different layers. Work top-down through the layers below; do **not** jump straight to LLM logs.

### Log layout

Log files are split across containers. These tables tell you *where to look first* for each layer.

**Manager container (`agentteams-manager`)** — supervisord manages several processes, each with its own log:

| Log file | What it contains | Look here when |
|---|---|---|
| `/var/log/agentteams/manager-agent.log` | Manager agent stdout (OpenClaw or CoPaw runtime) | Manager didn't react to your message / skill error |
| `/var/log/agentteams/manager-agent-error.log` | Manager agent stderr | Manager crashed or a tool call threw |
| `/var/log/agentteams/agentteams-controller.log` | Controller reconcile loop | Worker creation / deletion / CR status issues |
| `/var/log/agentteams/agentteams-controller-error.log` | Controller errors (stderr) | Worker create failed, Docker/K8s API errors, authorizer denials |
| `/var/log/agentteams/tuwunel.log` | Matrix homeserver | Missing events, room join failures, device/token errors |
| `/var/log/agentteams/higress-gateway.log` | AI Gateway request log | LLM timeouts, routing errors, rate-limit hits |
| `/var/log/agentteams/higress-controller.log` | Higress control plane | Consumer / route config drift |
| `/var/log/agentteams/mc-mirror.log` | `mc mirror` output for Manager's workspace | `agents/…` files not propagating to/from MinIO |

Dump commands:

```bash
docker exec agentteams-manager tail -n 200 /var/log/agentteams/manager-agent.log
docker exec agentteams-manager tail -n 200 /var/log/agentteams/agentteams-controller.log
docker logs agentteams-manager --tail 200    # supervisord-level + any uncaught stdout
```

**Worker container (`agentteams-worker-<name>`)** — CoPaw Worker emits two log streams:

| Stream | Where |
|---|---|
| stdout (via `docker logs`) | INFO / WARNING / ERROR from `copaw.*` loggers |
| File log | `/root/.agentteams-worker/logs/<date>_<time>.log` (same content as stdout; created on startup) |

Level is controlled by `COPAW_LOG_LEVEL=debug|info|warning|error` (default `info`). AgentTeams Worker containers **do not** set it by default; bounce the container with `-e COPAW_LOG_LEVEL=debug` when you need `_download_mxc` / sync-loop detail.

> ⚠️ Only `copaw.*` and the root logger reach stdout. Any third-party or vendored logger using a different namespace is **silently dropped at INFO level.** The in-tree Matrix channel (`copaw/src/matrix/channel.py`) uses `logging.getLogger("copaw.channels.matrix")` so channel decisions are visible in normal Worker logs.

**Session files (per-agent conversation history):**

| Runtime | Path | Format |
|---|---|---|
| OpenClaw Manager | `/root/manager-workspace/.openclaw/agents/main/sessions/` | `.jsonl` — one message per line |
| CoPaw Manager | `/root/manager-workspace/.copaw/workspaces/default/sessions/` | `.json` — one file per session |
| CoPaw Worker | `/root/.agentteams-worker/<name>/.copaw/workspaces/default/sessions/` | `.json` — one file per session |

The two formats are **not** cross-compatible.

**Runtime configs in the Worker (quick reference for bridge correctness checks):**

| File | Owner | Use when |
|---|---|---|
| `/root/.agentteams-worker/<name>/openclaw.json` | Controller (via MinIO) | verify Matrix allowlist / provider config that the Manager *intends* |
| `/root/.agentteams-worker/<name>/.copaw/config.json` | `bridge()` output | verify what CoPaw actually sees for channels / security |
| `/root/.agentteams-worker/<name>/.copaw/workspaces/default/agent.json` | `bridge()` output | verify what the agent loop uses at runtime |
| `/root/.agentteams-worker/<name>/.copaw/workspaces/default/matrix_sync_token` | Channel state | reset to force full re-sync |

### Standard triage path

Use this ladder for *any* "agent didn't respond" symptom. Each step narrows where the break is; don't skip levels.

```
1. Did Matrix receive the user's message?
   → tuwunel.log: grep for the sender MXID + room_id around the send time
   → /_matrix/client/v3/rooms/{room}/messages   (verify event is stored)

2. Did the target agent's channel accept it?
   → Worker/Manager log: look for "allowlist" / "not mentioned" / "policy=<...> dropped"
   → If you see NOTHING, suspect logger namespace drop (see gotcha 2 below)
   → Inspect openclaw.json + .copaw/config.json for dm.policy/allow_from / groupPolicy/groupAllowFrom

3. Was the agent loop actually woken up?
   → Session file: new file (or updated timestamp) under sessions/?
   → Open latest session, look for the user message + any thinking blocks

4. Did the LLM round-trip complete?
   → higress-gateway.log: grep by timestamp / upstream / model
   → Look for timeout, 4xx, or consumer-auth-denied

5. Did the reply leave via Matrix?
   → tuwunel.log: new m.room.message event from the agent's MXID?
   → Worker/Manager log: "channels send" / "send_message" success
```

### Known gotchas while debugging

These make healthy runs look broken or sick runs look healthy — learn them before chasing false positives.

1. **Runtime config changes require a Worker restart.**
   CoPaw worker no longer runs a background Remote -> Local config pull loop. Startup `mirror_all()` restores MinIO state, and runtime Local -> Remote preservation is handled by `push_loop`. Shared data can still be pulled explicitly through the filesync tool, but `openclaw.json`, skills, and mcporter config are refreshed by restarting the Worker.

2. **Team Leader startup always logs `authorization denied: team-leader "<name>" cannot ready worker`.** `agentteams-controller/internal/auth/authorizer.go :: authorizeTeamLeaderWorkerAction` does not list `ActionReady` — the Leader's `agt worker report-ready` call inside `copaw-worker-entrypoint.sh` gets a 403. The Worker process keeps running fine; only the readiness self-report is lost. Ignore it while debugging non-response issues — it is *not* the cause.
   > Remove this gotcha once `authorizeTeamLeaderWorkerAction` accepts `ActionReady` for the Leader's own team.

3. **Manager-side MCP server re-setup can touch MinIO `openclaw.json`.** If you re-run `setup-mcp-server.sh` (or Manager triggers it on heartbeat), MinIO objects are rewritten even when content matches, bumping mtime. Combined with gotcha 1 this can show as a storm of re-bridges; check Manager logs around the reload moments.

4. **Session files survive `make uninstall-embedded`** (volumes are not removed). Reproducing a bug from a clean slate requires the full-wipe command in § Build, Install, Test — Embedded mode.

### Dev tooling

The `hiclaw-debug` skill (`.claude/skills/hiclaw-debug/`) bundles the scripts that match this layout:

| Task | Script / doc |
|---|---|
| Sync Python edits into a running Worker without rebuilding | `.claude/skills/hiclaw-debug/scripts/dev-sync-copaw.sh` |
| Sync `manager/agent/**` edits into a running Manager | `.claude/skills/hiclaw-debug/scripts/dev-sync-agent.sh` |
| List / inspect CoPaw sessions | `.claude/skills/hiclaw-debug/scripts/copaw-session-viewer.py --list` |
| Show only thinking blocks from recent turns | `copaw-session-viewer.py --thinking --last 20` |
| Worker workspace layout reference | `.claude/skills/hiclaw-debug/references/worker-directory-structure.md` |
| End-to-end workflow | `.claude/skills/hiclaw-debug/SKILL.md` |

CoPaw source itself lives outside this repo as a sibling checkout on branch `feat/lite-copaw-worker-v1.0.0` (clone path depends on developer setup). Read its `AGENTS.md` before assuming upstream APIs — the fork has diverged from the public release.

## Technology Stack (CoPaw-specific)

| Component | Where | Purpose |
|---|---|---|
| CoPaw agent framework | upstream fork, imported by `copaw_worker` | Python ReAct loop, tool calling, session management |
| Matrix channel | `copaw/src/matrix/` (vendored) | Agent ↔ Tuwunel communication; identical for Manager and Worker |
| FileSync | `copaw/src/copaw_worker/sync.py` | MinIO ↔ local mirroring via `mc` CLI |
| Bridge | `copaw/src/copaw_worker/bridge.py` | Convert AgentTeams `openclaw.json` into CoPaw native configs |
| mcporter | installed in Worker image | MCP Server tool invocation for the Agent |

Runtime pairings:

| Role | OpenClaw image | CoPaw image |
|---|---|---|
| Manager | `agentteams/manager` | `agentteams/manager-copaw` |
| Worker | `agentteams/worker-agent` | `agentteams/copaw-worker` |

## Development Pitfalls

1. **Every edit under `copaw/src/matrix/` requires rebuilding both the Worker and the Manager-CoPaw image** — the channel is vendored into both. Rebuilding only one causes silent behavior drift between Manager and Worker.

2. **Local base image variables must be set in pairs.** When building from a locally-modified `openclaw-base`, always set both `OPENCLAW_BASE_IMAGE=hiclaw/openclaw-base` and `OPENCLAW_BASE_VERSION=latest`. Setting only one pulls the remote registry's `:latest` tag and silently uses the wrong base.

3. **`bridge()` has runtime-level side effects.** Beyond writing three JSON files it sets env vars and monkey-patches upstream CoPaw modules. Calling it twice in the same process works (it is idempotent by design), but tests that simulate multiple workers in-process must reset `COPAW_WORKING_DIR` and reload `copaw.constant` / `copaw.providers.store` between runs.

4. **Merge logic belongs to the Controller, not the Worker.** MinIO is the single source of truth at startup. Runtime Remote → Local updates are explicit for shared data; config/skills/mcporter updates are applied by restarting the Worker and re-running the startup mirror/bridge path.

5. **Manager-pushed skills are exposed via `.copaw/workspaces/default/skills/`.** This path is a symlink to the standard-space `skills/` directory. Do not reintroduce writes to the legacy `.copaw/active_skills/` path or copy skills into runtime space.

6. **`SOUL.md` / `AGENTS.md` remote edits require restart.** Runtime agent edits are saved back to standard space and then pushed, but Controller edits in MinIO do not overwrite a running Worker. Restart to apply remote prompt/config changes.

7. **Manager runtime overlays.** `manager/agent/copaw-manager-agent/AGENTS.md` and `HEARTBEAT.md` are overlaid on top of `manager/agent/` at container build time (via `upgrade-builtins.sh`) when runtime=copaw. Changes to the shared versions only take effect for OpenClaw until you mirror them into `copaw-manager-agent/`. `SOUL.md` and `TOOLS.md` are shared across both runtimes — **do not fork them**.

8. **Team Leader runs inside the CoPaw container but loads OpenClaw-style builtins.** `LeaderSpec` has no `runtime` field; `agentteams-controller` *forces* every Team Leader Worker CR onto `runtime=copaw`, so the Leader pod starts from `agentteams/copaw-worker` and goes through the full `bridge` / `propagate` chain like any other CoPaw Worker. But `deployer.builtinAgentDir(role="team_leader", runtime=...)` ignores `runtime` and always returns `manager/agent/team-leader-agent/` (OpenClaw-shaped `AGENTS.md` / `SOUL.md.tmpl` / skills). So the Leader is "Python CoPaw process reading OpenClaw-style agent content." Consequences when editing:
   - Keep `manager/agent/team-leader-agent/**` runtime-agnostic — no Node-only paths, no OpenClaw-only tool invocations, because CoPaw reads it.
   - Python-specific guidance and paths in `manager/agent/copaw-worker-agent/**` do **not** apply to Leaders. Leaders do not pick up that directory.
   - `manager/agent/copaw-manager-agent/**` is the Manager's CoPaw overlay, not the Leader's. Do not mix.

9. **Manager must send Matrix messages via `copaw channels send` CLI, not raw Matrix API.** Direct `curl` to `/_matrix/client/v3/rooms/.../send/m.room.message` bypasses CoPaw's HTML formatting layer, producing messages without `formatted_body`. The rule itself lives in `manager/agent/copaw-manager-agent/AGENTS.md` (agent-facing); keep it consistent when editing skills, tools, or docs under `manager/agent/copaw-manager-agent/**` or `manager/agent/copaw-worker-agent/**`.

10. **Session file format differs from OpenClaw** — `.json` per session for CoPaw, `.jsonl` per message for OpenClaw. Not cross-loadable. Paths and details: see § Debugging — Session files.

11. **Upstream CoPaw in this project is a fork**, not the public release (branch `feat/lite-copaw-worker-v1.0.0`). The fork's source tree lives outside this repo — its clone location depends on the developer's local setup (not hardcoded here). Read that repo's `AGENTS.md` before assuming upstream API surfaces.

## Changelog Policy

Any change that affects the contents of a built image — i.e. modifications under `copaw/` or `manager/Dockerfile.copaw` (or anything it `COPY`s in) — **must** be recorded in [`changelog/current.md`](../changelog/current.md) before committing. Format:

```
- type(scope): description ([commit_hash](https://github.com/higress-group/hiclaw/commit/commit_hash))
```

This matches the repo-wide policy in the root AGENTS.md.

## Writing Convention

- This file and everything it references **for developers** (e.g. `copaw/README.md`, `copaw/src/matrix/README.md`) is developer-facing — use third-person, describe code as artifacts ("`bridge()` converts ...", "the Worker reads ...").
- Everything under `manager/agent/copaw-manager-agent/**` and `manager/agent/copaw-worker-agent/**` is **agent-facing** — use second person ("You are the Manager...", "Run `mcporter --config ...`"). See the root [AGENTS.md § Agent-Facing Content](../AGENTS.md#agent-facing-content-writing-convention).

Do not mix the two voices in one file.
