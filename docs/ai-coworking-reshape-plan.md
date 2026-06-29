# AgentTeams → Personal AI Coworking — Reshape Plan

> **Status:** Draft v1 · **Date:** 2026-06-29 · **Owner:** @nattiini45
> Authored with Claude Code (Opus 4.8) after a full codebase analysis + live VPS survey.
>
> **How to continue at home:** this lives on branch `plan/ai-coworking-reshape`.
> `git fetch origin && git checkout plan/ai-coworking-reshape` (or merge into `main`).
> A logged-in **Claude Code instance on the VPS** can execute the on-box steps.
>
> Runtime target versions in §6 confirmed via web research on 2026-06-29.

---

## 1. What we're building

Reshape this AgentTeams (formerly HiClaw) fork into a **personal AI coworking system**: a
**Manager** over several **functional Teams** (engineering, design, marketing/social, gamedev…),
each with a **Team Lead**, executing real work against **Gitea**, managed through a **web
dashboard** — with Matrix demoted to pure conversation.

### Locked-in decisions
| Decision | Choice |
|---|---|
| Org model | Manager → functional **Teams** with **Team Leads** + workers (keep & lean into the Team model) |
| Users | **Solo** (just the operator) — strip multi-user permission machinery |
| Orchestrator runtime | **CoPaw / QwenPaw** (Manager + Team Leads) — *latest* version |
| Worker runtime | **Hermes** (autonomous coder) — *latest* version |
| LLM | **DeepSeek-v4-pro** via `openai-compat` (already configured) |
| Git | self-hosted **Gitea** (`git.pawcommit.com`) via the existing **Gitea MCP** behind Higress |
| Management surface | **New web dashboard** (Project browser + task board + file browser) |
| Matrix role | **Conversation only** — watch inter-team comms + talk to agents. NOT progress tracking |
| Deploy strategy | **Rebuild fresh from the fork**, served behind the existing **Traefik** on `*.pawcommit.com` |
| OpenHands | **Out of scope** (it's the user's separate Gitea coding agent) |

---

## 2. Target architecture

```
                 You (Element + Dashboard, behind Traefik/TLS)
                          │
        studio.pawcommit.com (Element)   hq.pawcommit.com (Dashboard, NEW)
                          │                        │
        matrix.pawcommit.com (Tuwunel) ── Matrix ──┤
                          │                        │ REST :8090 / MinIO / Gitea API
            ┌─────────────┴─────────────┐          │
            │   hiclaw-controller        │◄─────────┘
            │   (embedded: Higress +     │
            │    Tuwunel + MinIO + ctrl) │── proxy net ──► gitea-mcp:8080 ──► Gitea
            └─────────────┬──────────────┘
                          │ creates containers (CoPaw / Hermes)
          ┌───────────────┼───────────────────────────┐
          ▼               ▼                            ▼
     Manager (CoPaw)  Team Lead (CoPaw)          Workers (Hermes)
                       per functional team        autonomous coders, per team
```

- **Higress** = credential firewall + LLM/MCP proxy. Holds the DeepSeek key + the Gitea MCP
  upstream; agents authenticate with per-identity consumer keys.
- **MinIO** = shared filesystem + task store (`shared/tasks/`, `shared/projects/`, `agents/<name>/`).
- **Controller** = the factory: reconciles `Manager`/`Team`/`Worker`/`Human` CRs into containers,
  Matrix users/rooms, Higress consumers, MinIO prefixes.

---

## 3. Existing VPS infrastructure to integrate with (not rebuild)

Host `PawCommit-pawtner-1` — **8 vCPU / 16 GB / 301 GB** Ubuntu 24.04. Busy multi-project box.

| Capability | Already running | Use it for |
|---|---|---|
| Ingress / TLS | **Traefik** (`proxy` net, letsencrypt) + **Tailscale** + **Cloudflared** | Serve Element + Dashboard on `*.pawcommit.com`; no new TLS work |
| Git | **Gitea 1.26.4** @ `git.pawcommit.com` (internal `gitea:3000`, SSH host :4443) | Per-project repos; Gitea Actions CI already present (3× act_runner) |
| Git MCP | **gitea-mcp-server 1.3.0** — HTTP on internal `:8080`, `proxy` net, public `git-mcp.pawcommit.com` (Traefik basic-auth), single shared `GITEA_ACCESS_TOKEN` | Register as a Higress MCP server → workers get 18 git tools through the gateway |
| Files | **filebrowser** (web file browser, on `filebrowser_default` net which Gitea also joins) | Reuse or fold into the dashboard for file access |
| Execution | **Claude Code** logged in on the box | Run on-box build/deploy/debug steps |
| (Out of scope) | OpenHands 1.8, n8n, lifecycle-mcp, tavily-mcp, minimax-coding-plan-mcp | — |

**Domains to reuse** (used previously): `studio.pawcommit.com` (Element), `matrix.pawcommit.com`
(Tuwunel CS API), `console.pawcommit.com` (Higress console). **New:** `hq.pawcommit.com`
(or similar) for the dashboard.

**Networks:** `proxy` (external — Traefik + gitea-mcp), `hiclaw-net` (AgentTeams). The controller
container must join **both**.

**Prior config (from the stopped stack):** CoPaw manager, default worker runtime `copaw`,
Matrix domain `matrix-local.hiclaw.io:18080`, MinIO `:9000` bucket `hiclaw-storage`
(**creds are weak defaults — rotate**), workspace `/root/hiclaw-manager`. **14 existing CoPaw
workers** (godot-*, game-*, narrative-writer, engineer-backend-architect, …) — standalone, to be
restructured into Teams. **Daily snapshots** at `/root/backups/hiclaw-snapshot-*` (data is safe).

---

## 4. The phases (overview)

| # | Phase | Type | Effort | Delivers |
|---|---|---|---|---|
| 0 | Fresh deploy you control | Foundation | M | Clean instance from fork, on your domains, TLS, reproducible |
| 1 | Fix chat & assignment | Config/prompts | S | Natural delegation; solo-mode UX |
| 2 | Wire the Gitea MCP | Config/wiring | S | Workers do real git work through the gateway |
| 3 | Teams + Hermes workers | Config + validate | M | The functional-team org, autonomous coders |
| 4 | Projects with repos | Skill/storage | S | Work organized by Project = working repo + source repos |
| 5 | Management dashboard | Net-new build | L | Project browser + task board + file browser; chat demoted |
| 6 | Easy file access | Net-new | M | Drag-drop in chat reaches workers; results flow back |

**Sequencing:** 0 gates all. Then **1 ∥ 2** (parallel quick wins). Then **3** (de-risk the Hermes
interop spike during 0/1). Then **4 → 5** (the dashboard). **6** folds in. Hardening (§7) runs
alongside 0.

---

## 5. Phases in detail

### Phase 0 — Fresh deploy you control  · effort M · risk M
**Goal:** a clean AgentTeams instance built from this fork, reachable on your domains over TLS,
tear-down/rebuild at will.

**Steps**
1. **Build images from the fork** with bumped runtime pins (see §6): controller/embedded, manager
   (copaw), copaw-worker, hermes-worker. Build on the VPS (Claude Code) or locally and push to a
   registry the box can pull.
2. **Deploy shape:** run the embedded `hiclaw-controller` container, joined to **both** `hiclaw-net`
   and `proxy`, with **Traefik labels** instead of raw host-port binding:
   - `studio.pawcommit.com` → Element (internal `:18088`)
   - `matrix.pawcommit.com` → Tuwunel CS API (internal `:18080`/`:6167`)
   - `console.pawcommit.com` → Higress console (internal `:8001`) — keep behind Traefik auth
3. **Env config:** `HICLAW_MANAGER_RUNTIME=copaw`, `HICLAW_DEFAULT_WORKER_RUNTIME=hermes`,
   DeepSeek `openai-compat` key/model, **rotated MinIO creds**, Matrix domain set to
   `matrix.pawcommit.com`, `HICLAW_ELEMENT_HOMESERVER_URL=https://matrix.pawcommit.com`.
4. **Selective data import:** restore only the workspaces/personas you want from
   `/root/backups/hiclaw-snapshot-2026-06-29` (don't blanket-restore the old DB).
5. **Smoke test:** Manager comes up, joins the admin DM, LLM-auth probe passes, you can chat.

**Files/components:** `install/hiclaw-install.sh` (or a new compose with Traefik labels),
`hiclaw-controller/Dockerfile.embedded`, `hiclaw-controller/supervisord.embedded.conf`,
`manager/scripts/init/start-manager-agent.sh` + `start-copaw-manager.sh`,
`manager/scripts/init/setup-higress.sh`, `helm/hiclaw/values.yaml` (image tags).

**Acceptance:** open `studio.pawcommit.com`, log in, message the Manager, get a reply over TLS.
Rebuild from scratch in one command/script.

---

### Phase 1 — Fix chat & assignment  · effort S · risk L–M
**Goal:** delegating and talking to agents stops being a hassle.

**Steps**
1. **Bare `@mention` resolution** — resolve `@alice` → `@alice:domain` from the worker registry so
   natural mentions wake agents. *(`copaw/src/matrix/channel.py` `_was_mentioned()` + a localpart cache.)*
2. **In-turn acknowledgment** for CoPaw orchestrators — send an immediate ack on receiving a task/
   completion before deferring detailed work to the heartbeat. *(`copaw/src/matrix/channel.py`,
   `manager/agent/copaw-manager-agent/AGENTS.md`, `HEARTBEAT.md`.)*
3. **Tighter heartbeat** — make the interval configurable and drop from 1h to ~5–10 min.
   *(`manager/scripts/init/start-copaw-manager.sh`, `manager/configs/manager-openclaw.json.tmpl`
   for the OpenClaw analogue if ever used.)*
4. **Catch-up-sync replay safety net** — persist + replay messages received during first-boot sync
   instead of dropping them. *(`copaw/src/matrix/channel.py` `_sync_loop`, add `catch_up_replay`.)*
5. **Solo mode** (`HICLAW_SOLO_OPERATOR`): drop `PermissionLevel`, auto-resolve the operator as sole
   admin, force `PeerMentions=true`, skip the onboarding interview.
   *(`hiclaw-controller/api/v1beta1/types.go`, `internal/controller/{team,manager}_controller.go`,
   `internal/config/config.go`.)*

**Acceptance:** `@alice do X` (bare) works; you get an immediate ack; a freshly-created worker
doesn't drop your first message; no permission/onboarding friction.

---

### Phase 2 — Wire the Gitea MCP  · effort S · risk L
**Goal:** every worker can do real git/issue/PR work on `git.pawcommit.com`, through the gateway.

**Steps**
1. **Network:** attach the controller/Higress container to the `proxy` network (done in Phase 0).
2. **Register** `http://gitea-mcp:8080` as a Higress MCP server (same path as the GitHub MCP):
   `manager/scripts/init/setup-higress.sh` + `manager/agent/skills/mcp-server-management/`. Gate
   per-worker via `allowedConsumers`.
3. **Worker skill:** add `manager/agent/worker-skills/gitea-operations/SKILL.md` describing the 18
   Gitea tools (mirror `github-operations`).
4. **Hermes-native path:** Hermes workers (terminal sandbox) can additionally `git clone/push`
   directly against `git.pawcommit.com` with a scoped token/deploy key for fast iteration.
5. **Identity:** shared `GITEA_ACCESS_TOKEN` is fine for solo; per-*repo* scope comes from Project
   config (Phase 4), not per-worker tokens.

**Acceptance:** a worker lists issues / opens a PR on a Gitea repo via its gateway key; revoking it
from `allowedConsumers` cuts access.

---

### Phase 3 — Teams + Hermes workers  · effort M · risk M (interop)
**Goal:** the Manager → functional Teams (engineering / design / marketing-social / gamedev) with
**CoPaw leads** + **Hermes workers**, with the 14 existing personas migrated in.

**Steps**
1. **Define Team CRs** — one per function, each with a (CoPaw) Leader + member workers.
   *Note: Team Leader runtime is hard-coded to CoPaw (`team_controller.go:972`) — exactly what we want.*
2. **Switch workers to Hermes** — `HICLAW_DEFAULT_WORKER_RUNTIME=hermes` (image already wired via
   `HICLAW_HERMES_WORKER_IMAGE`), or per-member `runtime: hermes` in the Team spec.
3. **Migrate personas:** map godot-* / game-* / narrative-writer / engineer-backend-architect / … into
   the right teams as Hermes workers.
4. **⚠️ Validate interop (the one real risk):** confirm a **CoPaw lead ↔ Hermes worker** round-trip
   over Matrix (mention → task → result) works, and that Hermes workers can consume the team-worker
   skill templates. Run the **spike** (§7) early.

**Acceptance:** assign a task to a team in chat; the CoPaw lead delegates to a Hermes worker; the
worker executes and reports back; you see it in the (future) dashboard.

---

### Phase 4 — Projects with repos  · effort S · risk L
**Goal:** organize work by **Project = working repo (RW) + optional source repos (RO)** + assigned team.

**Steps** (storage-first; no new CRD yet)
1. Extend `manager/agent/skills/project-management/scripts/create-project.sh` + `meta.json` with a
   `repos[]` array: `{ url, access: rw|ro, team, workers[] }` under `shared/projects/{id}/`.
2. `git-delegation-management` reads repo URLs from the project manifest instead of inline paths.
3. Document the model in `project-management/SKILL.md` and `references/create-project.md`.
4. *(Later, if needed)* graduate to a `Project` CRD reconciled by the controller for declarative
   repo/access provisioning.

**Acceptance:** create a Project with one working repo + one source repo; a worker assigned to it
resolves the correct repos automatically; it's the data source the dashboard reads.

---

### Phase 5 — Management dashboard  · effort L · risk M
**Goal:** a web UI = **Project browser + visual task board + file browser**, behind Traefik at
`hq.pawcommit.com`. **Progress tracking moves out of Matrix.**

**Data sources (already exist — no agent re-instrumentation needed for v1):**
- Controller REST `:8090` — `GET /api/v1/{workers,teams,managers}` (Phase/State/Room/runtime…).
- Manager `state.json` (active task registry) — add `GET /api/v1/manager-tasks` to expose it.
- MinIO `shared/tasks/{id}/` (`meta.json` status/timestamps, `spec.md`, `result.md`, `progress/*.md`)
  and `shared/projects/{id}/` (Phase 4 manifest).
- Gitea API (repo/PR/issue context per project).

**Build (incremental)**
1. **v1 read-only:** a small same-origin SPA + thin backend proxy (avoids the missing-CORS problem
   on `:8090`); cards for Managers/Teams/Workers, a task table from `state.json`, a Project browser,
   a file browser over MinIO. Poll every ~15s.
2. **v2:** task detail panel (`meta.json` + `result.md` + latest `progress`), kanban by status,
   DAG render from `plan.md`.
3. **Serve behind Traefik** with auth (forward-auth or basic-auth like `git-mcp`). Options for serving:
   nginx alongside Element, or controller static dir (`internal/server/http.go`).

**Known gaps to design around:** no `%`-complete / per-subtask status / live stream today — v1 infers
from Phase + timestamps; add a lightweight event log (`shared/events/*.jsonl`) only if needed.

**Acceptance:** at `hq.pawcommit.com` you see all Projects, a live-ish task board, team/worker status,
and can browse/download files — without touching Matrix for tracking.

---

### Phase 6 — Easy file access  · effort M · risk M
**Goal:** drag-drop a file in chat and a worker receives it; results come back.

**Steps**
1. **Matrix-attachment → MinIO bridge:** on upload, push into the task/project dir so workers can
   `mc` it. *(`copaw/src/matrix/channel.py` `_on_room_media_event`, Hermes `overlay_adapter.py`.)*
2. **Dashboard file browser** (Phase 5) covers browse/download; optionally point the existing
   **filebrowser** at the AgentTeams workspace for a familiar UI.
3. Write-back: worker outputs surface in the dashboard + optionally posted to chat as attachments.

**Acceptance:** drop a spec/asset in a room → the assigned worker reads it; its output appears in the
dashboard and/or as a chat attachment.

---

## 6. Runtime upgrade plan (latest QwenPaw + Hermes)

Both runtimes are pinned old and carry version-sensitive patches.

### CoPaw / QwenPaw
- **Current:** `copaw-worker` **1.0.3** (this fork) → standard venv `copaw>=1.0.2,<2.0` (PyPI);
  **lite** venv = `johnlanni/CoPaw` @ commit **`212405a30380bc319b02397c166d5296029c89b8`**
  (`copaw/Dockerfile` `LITE_COPAW_COMMIT`). Based on **AgentScope**.
- **Version-sensitive patches (must re-check on bump):**
  - `copaw/Dockerfile:93-102` — `sed` patch for the "CoPaw 1.0.2 Matrix `_sync_loop` indentation
    bug". **Likely deletable** once on a fixed release.
  - `copaw/scripts/patch_{reme,agentscope,agentscope_runtime}_lazy.py` — lazy-import monkey-patches
    against specific module layouts; **verify they still apply** to the new AgentScope.
- **⚠️ Rebrand:** **CoPaw → QwenPaw** (2026-04-12). The canonical runtime now lives at
  `github.com/agentscope-ai/QwenPaw`; the pinned `johnlanni/CoPaw` lite fork is effectively
  superseded by it.
- **TARGET (latest stable): QwenPaw `v1.1.12.post2`** (2026-06-23). **Do NOT** jump to
  `v2.0.0-beta.1` (2026-06-26) — it's a breaking migration to **AgentScope 2.0** (event system,
  permissions, multi-tenancy) and is flagged unstable; it would also break the `patch_*_lazy.py`
  monkey-patches (they target AgentScope **1.x** internals). For reference: `copaw-worker` PyPI is
  already at 1.0.3 (2026-06-01); AgentScope hit 2.0.3 (2026-06-29).
- **To bump:** repoint `LITE_COPAW_REPO`/`LITE_COPAW_COMMIT` in `copaw/Dockerfile` to
  `agentscope-ai/QwenPaw` @ `v1.1.12.post2` (and bump `manager/Dockerfile.copaw` `COPAW_VERSION`);
  then **delete the 1.0.2 indentation `sed` patch** (`copaw/Dockerfile:93-102` — almost certainly
  fixed upstream; verify) and **re-validate** `patch_{reme,agentscope,agentscope_runtime}_lazy.py`
  against the new module layout. Re-test the Matrix channel overlay (`copaw/src/matrix/`).

### Hermes
- **Current:** `hermes-worker` **0.1.0** (this fork) → `hermes-agent` from git
  **`HERMES_GIT_REF=v2026.4.16`** (= "Hermes Agent v0.10.0", commit `1dd6b5d5`),
  repo `github.com/NousResearch/hermes-agent` (`hermes/Dockerfile:40`).
- **Version-sensitive patch:** `hermes/Dockerfile:138-140` renames hermes-agent's internal
  `gateway/platforms/matrix.py` → `_matrix_native.py` and installs `hermes_matrix/_shim.py` in its
  place. **Confirm that module path still exists** in the newer tag, or the shim breaks.
- **TARGET (latest): `HERMES_GIT_REF=v2026.6.19`** (= hermes-agent **v0.17.0**, "The Reach
  Release", 2026-06-19) — 7 releases past the pinned v0.10.0. Changes are **additive** (no breaking
  changes called out): background subagents (async delegation), Raft agent-network channel, faster
  cold start, and — relevant here — a **PyPI package** (`pip install hermes-agent==0.17.0`, added in
  v0.14.0) that could **replace the git-clone build path** entirely.
- **To bump:** set `HERMES_GIT_REF=v2026.6.19` in `hermes/Dockerfile` (or switch the install to
  `pip install hermes-agent==0.17.0`); then **verify the shim still applies** —
  `hermes/Dockerfile:138-140` depends on `gateway/platforms/matrix.py` existing, and
  `hermes/src/hermes_matrix/{_shim.py,overlay_adapter.py}` subclass the native `MatrixAdapter`
  (check its `__init__`/method signatures haven't shifted across 7 releases). Rebuild
  `hiclaw-hermes-worker`.

### Where image tags live
`helm/hiclaw/values.yaml` (`worker.defaultImage` per runtime, manager image) and the `Makefile`
image version vars. Bumping the runtime means rebuilding the corresponding `hiclaw-*-worker` /
`hiclaw-manager-copaw` images and updating tags.

---

## 7. Cross-cutting

**Hermes-interop spike (do early, during Phase 0/1):** stand up 1 CoPaw lead + 1 Hermes worker in a
throwaway team; confirm mention→task→result over Matrix and that Hermes consumes the team-worker
skills. This de-risks Phase 3 before the full restructure.

**Hardening:**
- **Rotate MinIO creds** (currently `admin` / weak default) and the gateway/LLM keys; keep them in
  the install env file, not in image layers.
- Keep `console.pawcommit.com` and any MinIO console behind Traefik auth.
- Continue the daily `hiclaw-snapshot` backups; snapshot before each phase's deploy.

**Execution:** the **Claude Code on the VPS** can run the build/deploy/debug steps directly on-box.
Prefer building images there (or pull from a registry the box can reach — note the default registry is
the Aliyun CN one; mirror or build locally).

---

## 8. Open decisions still pending
1. Dashboard domain name (`hq.pawcommit.com`? `studio` with a path?).
2. Projects always team-scoped, or also standalone (manager + workers, no Team)?
3. Dashboard auth: reuse Matrix login token, a controller API key, or Traefik forward-auth?
4. Hermes workers — deploy keys (read-only per repo) vs scoped API tokens for native git.
5. Do you want proactive nudges/digests from the Manager (heartbeat-driven), or stay reactive?
6. Graduate Projects to a real CRD (Phase 4 "later"), or keep storage-only indefinitely?

---

## 9. Appendix — key references

**Code hot-spots:** `manager/agent/AGENTS.md` · `manager/agent/copaw-manager-agent/` ·
`manager/agent/skills/` + `worker-skills/` · `copaw/src/matrix/channel.py` ·
`hermes/src/hermes_matrix/{_shim.py,overlay_adapter.py}` ·
`hiclaw-controller/internal/controller/{member_reconcile,team_controller,manager_controller}.go` ·
`hiclaw-controller/internal/service/provisioner.go` · `hiclaw-controller/internal/gateway/higress.go` ·
`hiclaw-controller/internal/server/http.go` (REST :8090) · `install/hiclaw-install.sh` ·
`hiclaw-controller/supervisord.embedded.conf`.

**Gitea MCP integration target:** `http://gitea-mcp:8080` on the `proxy` net → register as a Higress
MCP server with per-worker `allowedConsumers`; shared `GITEA_ACCESS_TOKEN`; public mirror at
`git-mcp.pawcommit.com`.

**Friction catalog (condensed, from analysis):** dead-air cold start; one-shot onboarding;
bare-`@mention` silent drop; CoPaw heartbeat-deferred replies; push-before-notify race; first-boot
message suppression; no status visibility without `docker exec`; agent-only file sync; GitHub-only git.
