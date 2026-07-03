# AgentTeams → Personal AI Coworking — Reshape Plan

> **Status:** Draft v2.3 · **Date:** 2026-06-29 (v1) · 2026-06-30 (v2) · 2026-07-02 (v2.2/v2.3) · **Owner:** @nattiini45
> Authored with Claude Code (Opus 4.8) after a full codebase analysis + live VPS survey.
>
> **v2 (home session) added:** multi-provider LLM (Phase 2b), harness enrichment / skills + CLIs on
> every worker (Phase 4.5), a local Docker Desktop satellite (Phase 0b), and Matrix verbosity control
> (Phase 5b). Each was investigated against the code by a multi-agent workflow and adversarially
> verified; `file:line` anchors are inline. Where a verifier overturned a finding, the corrected fact
> is what's written here (flagged ⚠️). **v2.1** adds §10 — implementation-ready build specs (Project
> CRD, dashboard data contracts, spike exit-criteria, migration mapping) for the four weakest spots.
> **v2.2 (2026-07-02):** a 48-agent post-verification pass re-checked every anchor against the fork.
> Folded in: the **mcp consumer-isolation fix** (Step-5 override, decision #14), the **CoPaw two-channel
> re-anchor** (S1 statically resolved, decision #15), new spike **S9** (Hermes inbound media, Phase 6),
> an orphaned CoPaw patch script (§6), PAT-lifecycle hardening (§7), and ~10 anchor corrections.
> **v2.3 (2026-07-02):** an improvement round (~50 agents over usability / stability / agent-workflows /
> web-UI project control) folded in: **projectflow↔CRD federation** (#16 — the runtime already has a
> full project-execution system the plan had missed), **dashboard control tiers** (#17), **project
> completion lifecycle** (#18), Phase 0 backend hardening (docker limits + restart policy), Phase 1
> chat-ops + new Phase 1b task-lifecycle robustness, spikes **S10 + S-BACKUP**, a §7 capacity budget,
> and the §10.2 table-F shape-contradiction fix.
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
| LLM | **Multiple OpenAI-compatible providers behind Higress, chosen per agent.** Default **DeepSeek-v4-pro** (`openai-compat`); also register **Xiaomi MiMo** + **Ollama Cloud** (+ room for more). Model per agent via `spec.model`; **upstream pinned per agent** via `spec.modelProvider` (decided policy). → Phase 2b |
| Git | self-hosted **Gitea** (`git.pawcommit.com`) via the existing **Gitea MCP** behind Higress — **per-worker Gitea identity** (each worker = its own Gitea user+PAT via a per-worker `mcp-gitea-<worker>` registration; → Phase 2/§10.1) |
| Management surface | **New web dashboard** (Project browser + task board + file browser) |
| Matrix role | **Conversation only** — watch inter-team comms + talk to agents. NOT progress tracking |
| Deploy strategy | **Rebuild fresh from the fork**, served behind the existing **Traefik** on `*.pawcommit.com` |
| Deploy topology | **VPS (primary, public) + optional local Docker Desktop satellite.** The local box runs heavier teams (more RAM) and shares the VPS **Gitea** + the DeepSeek upstream key; each instance keeps its **own embedded Matrix/MinIO/Higress**. Dashboard is the cross-instance pane. → Phase 0b |
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
| Git MCP | **gitea-mcp-server 1.3.0** — HTTP on internal `:8080`, `proxy` net, public `git-mcp.pawcommit.com` (Traefik basic-auth); its env `GITEA_ACCESS_TOKEN` is **overridden per-request** by a per-server `Authorization: Bearer` (verified) | Register **one Higress MCP server per worker** (`mcp-gitea-<worker>`, that worker's own PAT as upstream `defaultCredential`) → each worker gets the 18 git tools as its **own Gitea user** |
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
| 0b | Local satellite (Docker Desktop) | Foundation | M | Second instance on your home box (more RAM); shares the VPS Gitea |
| 1 | Fix chat & assignment | Config/prompts | S | Natural delegation; solo-mode UX |
| 1b | Task-lifecycle robustness | Skills/scripts | S–M | Cancel/reassign, orphaned-task sweep, id de-dupe (v2.3) |
| 2 | Wire the Gitea MCP | Config/wiring | S | Workers do real git work through the gateway |
| 2b | Multi-provider LLM | Config/wiring | S–M | DeepSeek + Xiaomi MiMo + Ollama Cloud + more; per-agent model/provider |
| 3 | Teams + Hermes workers | Config + validate | M | The functional-team org, autonomous coders |
| 4 | Projects with repos | CRD + storage | M | Team-scoped Projects via a Project CRD; repos bound; per-worker gitea-mcp access authorized |
| 4.5 | Harness enrichment (skills + CLIs) | Config + image | S–M | Your own / market skills on every worker; CLI parity baked into images |
| 5 | Management dashboard | Net-new build | L | Project browser + task board + file browser; chat demoted |
| 5b | Matrix verbosity control | Config/code | S | Quiet rooms: activity-of-life, no 300-msg/turn tool-call spam |
| 6 | Easy file access | Net-new | M | Drag-drop in chat reaches workers; results flow back |

**Sequencing:** 0 gates all. Then **1 ∥ 2** (parallel quick wins). Then **3** (de-risk the Hermes
interop spike during 0/1). Then **4 → 5** (the dashboard). **6** folds in. Hardening (§7) runs
alongside 0. **v2 additions:** **0b** (local satellite) parallels 0 and reuses the same images;
**2b** (multi-provider) extends 2's Higress work; **4.5** (harness enrichment) and **5b** (Matrix
verbosity) fold in alongside 4/5. **5b** should land before/with **5** so the dashboard — not Matrix —
carries the detail you're quieting.

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
6. **Backend hardening (v2.3):** make the Docker backend honor the resource limits the CRDs already
   carry end-to-end — `buildCreatePayload` (`internal/backend/docker.go:517-599`) never reads
   `req.Resources` and `dockerHostConfig` has no Memory/CPU fields; add them, defaulting to the same
   1000m/2Gi the k8s backend uses (`kubernetes.go:433-479`), via new `DockerConfig.{Worker,Manager}{CPU,Memory}`.
   Also set `createReq.RestartPolicy = "unless-stopped"` for team members in `createMemberContainer`
   (`member_reconcile.go:414-425`) — today only the Manager gets it (`manager_reconcile_container.go:222`),
   so a VPS/dockerd restart brings the Manager back and leaves every lead/worker dark until the next
   reconcile. Unit-test both (assert HostConfig.Memory/NanoCpus set from req.Resources and from defaults).

**Files/components:** `install/hiclaw-install.sh` (or a new compose with Traefik labels),
`hiclaw-controller/Dockerfile.embedded`, `hiclaw-controller/supervisord.embedded.conf`,
`manager/scripts/init/start-manager-agent.sh` + `start-copaw-manager.sh`,
`manager/scripts/init/setup-higress.sh`, `helm/hiclaw/values.yaml` (image tags).

**Acceptance:** open `studio.pawcommit.com`, log in, message the Manager, get a reply over TLS.
Rebuild from scratch in one command/script.

---

### Phase 0b — Local satellite instance (Docker Desktop)  · effort M · risk M
**Goal:** a second AgentTeams instance on your home box (where you have RAM) running heavier teams,
sharing the VPS's **Gitea** + DeepSeek key, but keeping its **own** embedded Matrix/MinIO/Higress.
This is the "just a second *team set*" idea — same externals, different teams.

**What a local bring-up actually is (verified)**
- ONE embedded `hiclaw-controller` container (Higress + Tuwunel + MinIO + Element + controller under
  `supervisord`) that spawns **separate** manager/worker containers via the mounted Docker socket
  (`member_reconcile.go:414-425`). The Manager is **not** inside the embedded image.
- Differs from the VPS: **no Traefik/TLS**, ports bound to `127.0.0.1` (`18080`/`18001`/`18088`,
  `install/hiclaw-install.ps1:3174-3176`), `*-local.hiclaw.io` network aliases (`LOCAL_ONLY`,
  `ps1:3076-3079`; legacy non-embedded path `:2727-2730`).
- ⚠️ **The Docker backend sets NO per-container memory/CPU limits** — `buildCreatePayload` never reads
  `req.Resources`; limits are k8s-only (`internal/backend/docker.go:491-599` vs `kubernetes.go:433-479`).
  Your **WSL2 `.wslconfig memory=` ceiling is the only governor** — set it generously *before* first boot.
  *(v2.3: Phase 0 step 6 fixes this in the shared backend code — once landed, the local satellite gets
  hard caps too and `.wslconfig` becomes a second line of defense, not the only one.)*
- Per-worker RAM: OpenClaw ~500 MB, **QwenPaw ~150 MB**, Hermes between (`docs/windows-deploy.md:50,294-305`).
  HW min 2c/4 GB; recommended 4c/8 GB (`windows-deploy.md:46-47`). Workers are **stateless** — no host
  volumes; their FS is MinIO inside the controller `/data`.

**Steps**
1. Set a generous `.wslconfig` `memory=` (the only RAM cap), start Docker Desktop.
2. Bring up the embedded controller. ⚠️ `make install-embedded` runs **`bash ./install/hiclaw-install.sh`**
   (`Makefile:633`) — **not** the PowerShell installer — so on Windows it needs Git Bash/WSL + `make`;
   it sets `HICLAW_NON_INTERACTIVE=1`, the `HICLAW_INSTALL_*_IMAGE` overrides, `HICLAW_MATRIX_E2EE=0`,
   and builds/overrides **6–7 images**. Pure-PowerShell path: run `install/hiclaw-install.ps1` directly
   with `HICLAW_INSTALL_*_IMAGE` set.
3. Reach it at `http://127.0.0.1:18088` (local Element). ⚠️ E2EE is forced off (`Makefile:632`) — don't
   create encrypted Private rooms locally or agents can't read them.

**Sharing the VPS externals (the "second team set")**
What collides and what doesn't (track verified **solid**):
- ✅ **CRs/containers do NOT collide** across instances — each controller runs its own embedded
  apiserver + kine (`app.go:627-646`) and its own local `docker.sock`; containers are name-prefixed.
- ❌ **No instance discriminator exists today.** Matrix localparts/aliases are raw names (worker
  localpart = CR name; manager = literal `"manager"`, `provisioner.go:308-311`; alias
  `hiclaw-<kind>-<name>`, `:211-213`). `EnsureUser`/`CreateRoom` silently log into / return an existing
  account/alias (`client.go:225-273` EnsureUser; CreateRoom's `M_ROOM_IN_USE` alias-reuse `:481-496`)
  → silent hijack on a shared homeserver.
- ❌ Matrix **AppService defaults ON and claims an exclusive `@.*:<domain>` namespace** (`config.go:319`,
  `appservice.go:33-45`) → two AppServices on one homeserver = hard conflict.
- ❌ Single MinIO bucket `hiclaw-storage`; `hiclaw-config/` is mirrored and **ingested as CRs**
  (`app.go:658-667`) → a shared bucket cross-feeds both controllers.
- ℹ️ Embedded mode rewrites worker-facing Matrix/FS/AIGateway URLs to the controller container
  (`config.go:379-385`), but `MatrixDomain`/`MatrixServerURL` are **independent and can be external**.

**Two designs for "one place to see all agents":**
- **Option A — separate Matrix per instance (recommended; matches your read that Tuwunel is embedded).**
  Each instance keeps its own embedded Tuwunel + MinIO + Higress. **Share only Gitea + the DeepSeek key.**
  No homeserver/bucket collision because they're distinct. Set `HICLAW_FS_BUCKET=hiclaw-storage-home`
  on the local box so the two don't share the config-watched bucket. You get **two Elements**; the
  **dashboard (Phase 5) aggregates both controllers' REST `:8090`** → one pane for tracking. Lowest risk.
- **Option B — shared homeserver (one Element).** Technically possible: point the local controller at
  the VPS `matrix.pawcommit.com` (those URLs aren't rewritten) and run **no** embedded Tuwunel. But you
  must avoid collisions — either run local with `HICLAW_MATRIX_APPSERVICE_ENABLED=false`
  (password/registration-token mode) to dodge the exclusive `@.*` namespace, **and** prefix
  localparts/aliases/consumers; or add a net-new `HICLAW_INSTANCE_ID` threaded into `provisioner.go:211`
  + `:308-311`, `docker.go:162`, the consumer names, and the AppService user-namespace regex. Also patch
  the embedded host-rewrite to skip the external Matrix URL, and beware Tuwunel
  `DELETE_ROOMS_AFTER_LEAVE=true` (`start-tuwunel.sh:26-27`) pruning a shared-alias room.

**✓ DECIDED (2026-06-30): Option A.** Matrix stays per-instance; the dashboard is the cross-instance
unifier. Option B is parked — revisit only if a single Element across both boxes proves worth the
namespace surgery. (So `HICLAW_INSTANCE_ID` / namespace prefixing is **not needed now** — open decision #9
is deferred with it.)

**Caveats**
- Local has no `proxy` net/Traefik, so Phase 2's "Gitea-MCP behind Higress" path doesn't transfer
  verbatim — register the Gitea MCP for the local instance via `host.docker.internal` (workers get
  `host.docker.internal:host-gateway` on the docker backend, `member_reconcile.go:443-448`) or the tailnet.
- The embedded image **build** pulls Higress base layers from Aliyun (`Dockerfile.embedded:15-34`) — mirror
  those for a fully offline home build; the `HICLAW_INSTALL_*` overrides only skip *run-time* pulls.
- Verify the manager-workspace mount target: embedded controller binds host workspace →
  `/root/hiclaw-fs/agents/manager` (`ps1:3168`) while the spawned manager binds → `/root/manager-workspace`
  (`manager_reconcile_container.go:209-213`).

**Acceptance:** local Element at `127.0.0.1:18088`; message a local Manager; it spawns workers locally
that push to the **shared VPS Gitea**; the (future) dashboard shows both VPS and local teams.

---

### Phase 1 — Fix chat & assignment  · effort S · risk L–M
**Goal:** delegating and talking to agents stops being a hassle.

**Steps**
1. **Bare `@mention` resolution** — resolve `@alice` → `@alice:domain` from the worker registry so
   natural mentions wake agents. **Patch BOTH channel impls (✓ #15 — S1 statically resolved):** the
   Manager runs the overlay *(`copaw/src/matrix/channel.py` `_was_mentioned()` `:981-1015` — baked into
   the manager image by `manager/Dockerfile.copaw:86-96`)*; CoPaw leads/workers run the standalone
   near-duplicate *(`copaw/src/copaw_worker/matrix_channel.py` `_was_mentioned()` `:662` — installed
   into `custom_channels/` at startup by `worker.py:570-583`)*. Add the localpart cache to each.
2. **In-turn acknowledgment** for CoPaw orchestrators — send an immediate ack on receiving a task/
   completion before deferring detailed work to the heartbeat. *(Both channel files per #15 —
   `copaw/src/matrix/channel.py` for the Manager, `copaw/src/copaw_worker/matrix_channel.py` for
   leads/workers; `manager/agent/copaw-manager-agent/AGENTS.md`, `HEARTBEAT.md`.)*
3. **Tighter heartbeat** — make the interval configurable and drop from 1h to ~5–10 min.
   *(`manager/scripts/init/start-copaw-manager.sh`, `manager/configs/manager-openclaw.json.tmpl`
   for the OpenClaw analogue if ever used.)*
4. **Catch-up-sync replay safety net** — persist + replay messages received during first-boot sync
   instead of dropping them. *(Both files per #15 — overlay `_sync_loop` `channel.py:750-798` for the
   Manager, standalone `copaw_worker/matrix_channel.py:537-569` for leads/workers; near-duplicate
   "catch-up sync (messages suppressed)" logic in each — add `catch_up_replay` to both.)*
5. **Solo mode** (`HICLAW_SOLO_OPERATOR`): drop `PermissionLevel`, auto-resolve the operator as sole
   admin, force `PeerMentions=true`, skip the onboarding interview.
   *(`hiclaw-controller/api/v1beta1/types.go`, `internal/controller/{team,manager}_controller.go`,
   `internal/config/config.go`. **The interview itself lives elsewhere:** the 4-question Q&A prompt is
   rendered by `internal/service/provisioner.go` `renderManagerWelcomeBody` `:1540-1568` and sent via
   `internal/controller/manager_reconcile_welcome.go` — that's where the solo-mode skip goes.)*
6. **Proactive Manager nudges (✓ decided #5):** drive heartbeat-based nudges so the Manager pings you on
   meaningful events (a task **stalls**, **finishes**, or **needs input**) rather than staying purely
   reactive. Keep them coarse (Manager→you, event-level) so they coexist with quiet rooms (Phase 5b) —
   these are status nudges, not per-turn worker chatter. *(`manager/agent/copaw-manager-agent/HEARTBEAT.md`,
   `AGENTS.md`.)*
7. **Structured task intake (v2.3):** add a fill-in-the-blanks `spec.md` skeleton to
   `task-management/references/finite-tasks.md` (deliverable, acceptance criteria, target team/worker,
   priority, due) — Manager fills by best guess, asks at most one clarifying question, confirms in one
   message. Doubles as the schema behind the future dashboard task form (#17).
8. **Blocked-task routing (v2.3):** `manage-state.sh` gains `mark-blocked --task-id T --reason` /
   `unblock --task-id T` (writes `status:"blocked"` + `blocked_since` on the active_tasks entry);
   escalations are formatted `[task-id] blocker text` and the Manager parses that tag from the admin's
   reply — so two concurrent blockers in one DM don't collide. HEARTBEAT gains a blocked-age nudge.
9. **Daily digest (v2.3):** time-gate HEARTBEAT.md Step 7's healthy branch — once per 24h
   (`last_digest_sent_at` in state.json) send a cross-team digest (per-team active counts, idle workers,
   blocked items, prior-day completions) instead of silence on quiet days.

**Acceptance:** `@alice do X` (bare) works; you get an immediate ack; a freshly-created worker
doesn't drop your first message; no permission/onboarding friction; the Manager nudges you when a task
stalls/finishes without you asking.

---

### Phase 1b — Task-lifecycle robustness (v2.3)  · effort S–M · risk L
Three gaps from the improvement round; all Manager-skill/script changes, no controller code:
1. **Cancel/reassign primitive** — nothing in the stack can cancel or reassign a task today. Add
   `cancel` / `reassign` actions to `manage-state.sh` plus a `task-management` reference describing the
   protocol (notify the worker's room, update `meta.json`, remove/replace the active_tasks entry).
   Surfaced later as dashboard actions (#17).
2. **Orphaned-task reconciliation** — a container delete+recreate (any spec change,
   `member_reconcile.go:316-329`) strands the in-flight task in `state.json` forever. Add a HEARTBEAT
   step: for each active task, check the assigned worker is Running and the task dir shows recent
   progress; stale → nudge the operator or mark blocked.
3. **Task-id collision guard** — ids are second-granularity timestamps (`task-YYYYMMDD-HHMMSS`).
   `action_add_finite` already de-dupes on exact id (`manage-state.sh:55-61`, SKIP if present) — which
   is exactly the problem: two *distinct* same-second creates collide on id and the second is
   **silently skipped**, not added. On id collision with a different title/assignee, suffix the id and
   add, instead of skipping.

**Acceptance:** a running task can be cancelled and reassigned from chat; recreating a worker container
mid-task produces a heartbeat alert instead of a permanent ghost entry; two add-finite calls in the same
second yield two distinct ids.

---

### Phase 2 — Wire the Gitea MCP  · effort S · risk L
**Goal:** every worker can do real git/issue/PR work on `git.pawcommit.com`, through the gateway.

**Steps**
1. **Network:** attach the controller/Higress container to the `proxy` network (done in Phase 0).
2. **Register** the existing `http://gitea-mcp:8080` as **one Higress MCP server per worker** —
   `mcp-gitea-<worker>` via `setup-mcp-proxy.sh`, each carrying that worker's own PAT as the upstream
   `defaultCredential` and gated via `allowedConsumers` to that worker's single consumer (the operator
   helper #12 runs this loop). Higress holds the PATs; the worker presents only its consumer key.
   ⚠️ **Do NOT reuse the script's Step 5 as-is (✓ #14):** Step 5 (`setup-mcp-proxy.sh:326-339`)
   REPLACE-broadcasts **every** registry worker's consumer onto the new server and rewrites **all**
   workers' `config/mcporter.json` — silently voiding the per-worker isolation. The helper runs the
   script's steps 1–4 only, then itself PUTs `/v1/mcpServer/consumers` = `[<that worker>]` and updates
   only that worker's `mcporter.json`. Budget the ~10s auth-plugin wait per server (~2–3 min sequential
   over 14 workers); N registrations also mean N DNS service-sources pointing at the same gitea-mcp
   backend (harmless, just visible in the console).
   *(`manager/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh` — note the real path;
   earlier drafts cited `manager/scripts/init/`, which never held it.)*
3. **Worker skill + review loop (v2.3):** add `manager/agent/worker-skills/gitea-operations/SKILL.md`
   describing the **actual** gitea-mcp tools — **S-GIT first enumerates the live tool names/schemas**
   (don't assume GitHub parity; mirror `github-operations` in shape only). Add a **lead-review-loop**
   reference for the team-leader agent: worker opens a PR via its own `mcp-gitea-<worker>` → lead
   fetches the diff via *its own* identity, reviews, requests changes → worker fixes → lead merges.
   Workers report PR deliverables with review state (open / changes-requested / merged) in the Result
   Contract — this is what per-worker attribution (#4) buys you.
4. **Per-worker reach + Hermes-native path:** each worker reaches Gitea as its **own Gitea user** via its
   own `mcp-gitea-<worker>` registration (Higress holds that worker's PAT as the upstream `defaultCredential`;
   the worker presents only its consumer key). When a working tree is needed (gitea-mcp is API-based, no
   working tree), a Hermes **native checkout** reuses that **worker's own PAT** via a git credential helper —
   **not** a shared token, **not** per-repo deploy keys. Open spike S-GIT.
5. **Identity:** identity is **per-worker** — each worker has its **own Gitea user + scoped PAT**, provisioned
   by the operator helper (`scripts/provision-worker-gitea.sh`, #12). Per-repo `access: rw|ro` (Phase 4) is
   **enforced** via the worker-user's Gitea repo-collaborator role (#13: ro→read, rw→write), driven by the
   Project manifest — not advisory, not gateway-only.

**Acceptance:** a worker lists issues / opens a PR on a Gitea repo via its own `mcp-gitea-<worker>` (the PR
is attributed to that worker's Gitea user); revoking its consumer from that server's `allowedConsumers` cuts
access. **Isolation (✓ #14):** worker A's consumer key is rejected (401/403) on worker B's
`mcp-gitea-<worker>` — verified as part of spike S-GIT.

---

### Phase 2b — Multi-provider LLM  · effort S–M · risk L
**Goal:** DeepSeek + **Xiaomi MiMo** + **Ollama Cloud** (+ more), selected **per agent** — replacing the
single-LLM lock-in. Track verified **solid**.

**Why it's mostly already built (don't over-engineer this)**
- Every agent (Manager / Team Lead / worker, all three runtimes) hits **ONE** Higress URL with ONE
  per-identity key; the upstream is chosen by the **`model` string Higress routes** (`generator.go:126-144`,
  `manager-openclaw.json.tmpl:49-79`).
- Higress AI routes already fan out to **N providers by model-name prefix** — the documented OpenRouter
  pattern (`docs/faq.md:540-566`) and the model-switch skill (`model-switch/SKILL.md:44-51`).
- The CRDs already carry **per-agent `model` AND `modelProvider`** (`api/v1beta1/types.go:106-107,
  268-269, 309-310, 516-517`), and the controller already resolves `modelProvider` → its Higress route and
  pins that agent's consumer (`higress.go:564-643`; `worker_controller.go:129-134`;
  `member_reconcile.go:399-400`). Per-worker model switching is live (`worker-model-switch/SKILL.md`).
- **The only gaps:** the installer registers exactly ONE provider + a path-only `default-ai-route`
  (`initializer.go:432-438`, `setup-higress.sh`), and the model catalog is hard-coded in **3+ places**.

**Steps**
1. **Register upstreams in Higress** (extend `setup-higress.sh` / `initializer.go`, or seed via the
   console): keep DeepSeek as the default-route `openai-compat` provider; add `mimo` and `ollama` as
   type `openai` providers (`openaiCustomUrl` + Bearer key) — same path as `setup-higress.sh:198-228`.
   Give **each its own AI Route with a model-name-prefix match rule** (e.g. service `ollama`, match
   `^ollama/.*$`, URL `https://ollama.com/v1`; service `mimo`, match `^mimo`). ⚠️ Service-source names
   must **not** contain `/` — the slash lives only in the match rule (`faq.md:550-552`). Use a route name
   **≠ `default-ai-route`** (the controller rewrites that one every boot, `setup-higress.sh:160-281`).
2. **Surface the model IDs** in all catalog spots so they're selectable with sane context windows:
   `generator.go` `allModelSpecs`/`allModelAliases` (`:451-494`), `manager/configs/known-models.json`,
   `manager-openclaw.json.tmpl`, **and** the `update-manager-model.sh` ctx/max case table (4 spots — keep
   consistent or the Manager self-switch pre-flight uses wrong context windows). Unknowns default to
   150k ctx / 128k max (`generator.go:402`).
3. **Select per agent** — model via `spec.model` (or `hiclaw update worker --name X --model ollama/<model>`);
   **pin every agent's upstream with `spec.modelProvider`** (decided policy — e.g. gamedev team on `ollama`,
   engineering on `deepseek`). Both already implemented + unit-tested (`team_controller_test.go:308`,
   `manager_container_test.go:240`) — **no new controller plumbing**.
4. *(optional, solo-friendly)* a Manager skill that registers a new provider+route on request (reuse the
   `setup-higress.sh` `higress_api` calls) so onboarding a 4th provider is chat-driven.

**Provider compatibility (both verified OpenAI-compatible → no proxy code)**
- **Ollama Cloud:** base `https://ollama.com/v1`, `Authorization: Bearer <OLLAMA_API_KEY>`. Model names
  carry `:tag` / `:cloud` suffixes — fine in the request `model` field, just keep them out of the
  service-source name.
- **Xiaomi MiMo:** OpenAI-compatible; ⚠️ confirm the exact hosted base URL at provision time (hosted
  `platform.xiaomimimo.com`, or via WaveSpeedAI / OpenRouter, or self-host vLLM). Current models are
  **MiMo-V2.5 / V2.5-Pro** (the older `MiMo-7B-RL` ids are dated). Route by `^mimo` prefix regardless.

**Authorization policy — ✓ DECIDED (2026-06-30): pin per agent/team.** With **no** `spec.modelProvider`
set, `AuthorizeAIRoutes` uses an empty `providerFilter` → the consumer is authorized on **every** AI route
(`higress.go:192-323`). To avoid that, **set `spec.modelProvider` on every agent**, and template a
team-wide default per Team (the Team CRD has no team-wide field today — only per-leader/per-worker,
`types.go:268-269,309-310`, so set it on the leader + each member or add a `Team.spec.modelProvider`). Net:
every agent is scoped to one upstream; an autonomous Hermes worker can't silently jump to a pricier model.

**Acceptance:** one worker on DeepSeek, one on MiMo, one on Ollama Cloud in the same team — all routing to
their own upstream simultaneously; `hiclaw update worker --model` and the model-switch skill move an agent
between providers; revoking a pinned agent's route cuts its provider.

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
5. **Coordination patterns (v2.3)** — document in the `team-leader-agent` skills: (a) **cross-team
   requests** are Manager-mediated (requesting lead → Manager → new task/Project in the serving team,
   result relayed back; keeps #2's team-scoping intact); (b) an **Escalation Report envelope** for every
   needs-input ping — project/task id, blocker category (ambiguous requirement / technical / needs
   credential / needs decision), what was tried, the specific question — reused later by the dashboard's
   needs-you queue; (c) a note that `PeerMentions` only enables ad-hoc worker↔worker @mention chat — it
   creates no tasks and grants no delegation authority.

**Acceptance:** assign a task to a team in chat; the CoPaw lead delegates to a Hermes worker; the
worker executes and reports back; you see it in the (future) dashboard.

**→ Functional-team taxonomy + worker→team migration table: see §10.4.**

---

### Phase 4 — Projects with repos  · effort M · risk L–M
**Goal:** organize work by **Project = working repo (RW) + optional source repos (RO)**, **always scoped to
one Team** (✓ decided #2).

**✓ Decided (#6): build a real `Project` CRD now** — declarative repo/access provisioning reconciled by the
controller, consistent with the Worker/Team/Manager CRs (not the original storage-only path).

**Steps**
1. **Define the `Project` CRD** — `hiclaw-controller/api/v1beta1/` + `config/crd/projects.hiclaw.io.yaml`
   **and the Helm copy `helm/hiclaw/crds/` (keep both in sync** — the multi-provider track flagged Helm/CRD
   drift). Spec: `team` (required — team-scoped), `repos[] { url, access: rw|ro }`, optional `workers[]`.
2. **Add a reconciler** `internal/controller/project_controller.go` mirroring the worker/team controllers
   (periodic reconcile, same ~5-min cadence).
3. **Record git access for the per-worker Gitea identity (✓ #4/#12/#13).** On reconcile the controller only
   **records** the assigned workers + per-repo `access` into the MinIO manifest (step 5). The **operator
   helper** `scripts/provision-worker-gitea.sh` (#12, run by the operator, **not** the controller) then
   creates each worker's Gitea user + scoped PAT, registers its `mcp-gitea-<worker>` Higress server, and sets
   the repo-collaborator role (rw→write, ro→read) — so RW/RO is **enforced** (#13), not advisory. **The
   controller makes NO Gitea calls** — no Gitea client, no per-repo deploy keys/tokens, no PAT handling.
4. **Wire `git-delegation-management`** to read repo URLs + which credential to use from the Project spec,
   not inline paths.
5. **MinIO projection** at `shared/projects/{id}/` (manifest mirrored from the CR) so the dashboard +
   workers get a flat read path — **the CR is source of truth, MinIO the cache.**
6. Update `project-management/SKILL.md` + `references/create-project.md`; `create-project.sh` creates the CR
   via `hiclaw apply` instead of a bare manifest.

**Acceptance:** `hiclaw apply` a Project (team + 1 RW working repo + 1 RO source repo) → the controller
projects the manifest to MinIO (recording the assigned Hermes workers + per-repo access); the operator helper
(#12) provisions each worker's Gitea user/PAT + `mcp-gitea-<worker>` + collaborator role from it; those
workers can reach **both** repos as their **own Gitea user** — pushing to the RW repo but not the RO one
(collaborator-role enforced, #13) — and, if a native checkout is needed, clone via their **own PAT** helper;
the Project shows in the dashboard.

**→ Full CRD schema, Go types, reconciler & gitea-mcp access model: see §10.1.**

---

### Phase 4.5 — Harness enrichment: skills + CLIs on every worker  · effort S–M · risk L
**Goal:** author a skill once (or pull one from `market.hiclaw.io/skills`) → it reaches **every** worker;
and bring CLI tooling to parity across runtimes. This is the "enhance every worker image" ask — answered
below by *which layer* each enhancement belongs in.

**Three layers (don't conflate them):**
| Enhancement | Lives in | How it's delivered |
|---|---|---|
| **MCP servers** (Gitea MCP, …) | **Higress** (gateway-global) | per-identity consumer keys (Phase 2) |
| **CLIs / tools** (`mc`, `git`, `ripgrep`, `mcporter`…) | **baked into the worker image** | Dockerfile layers |
| **Skills** (`SKILL.md` bundles) | **MinIO, synced at runtime** — *not* baked | controller mirror + worker file-sync |

**Skill delivery model (verified)**
- Skill **content is never baked into worker images** — only the `skills` / `mcporter` / `@nacos-group/cli`
  **NPM binaries** are (`copaw/Dockerfile:71` bakes only `mcporter skills`; `hermes/Dockerfile:79` adds
  `@nacos-group/cli`). The lone exception is OpenHuman, which COPYs its skill template at build
  (`openhuman/Dockerfile:165`).
- **Builtin** skills are baked into the **manager + controller** images (`manager/Dockerfile.copaw:119`
  `COPY manager/agent/ /opt/hiclaw/agent/`); the controller mirrors `builtinAgentDir(role,runtime)/skills/*`
  → each worker's MinIO prefix with `Overwrite:true` (`deployer.go:835-857`; runtime map `:882-898`, incl.
  `openhuman`).
- ⚠️ **This push runs on EVERY ~5-min reconcile, not just at create** (`worker_controller.go:29,151,171` →
  `member_reconcile.go:244` → `deployer.go:835`). So a skill dropped in a builtin template dir + a
  controller **redeploy** self-heals onto **all existing workers within ~5 min** — *no backfill command
  needed* (this overturns the first-pass assumption that you'd have to re-push to running workers).
- **On-demand** skills: manager pool `~/worker-skills/` via `push-worker-skills.sh` (per-worker; `--skill`
  only re-pushes to workers that *already* list it, `:281-294`); declarative `remoteSkills` is **nacos://
  only** (`deployer.go:596-619`), also re-applied + overwritten every reconcile.
- The `find-skills` CLI's **effective default registry is `nacos://market.hiclaw.io:80/public`**
  (`install/hiclaw-install.sh:3059` → `config.go:371` → `worker_env.go:147`); `https://skills.sh` is only
  the fallback if both env vars are unset.

**Recommended approach — skills**
1. **The "every worker" seam = builtin template dirs.** Drop your skill under
   `manager/agent/{worker-agent,copaw-worker-agent,hermes-worker-agent,openhuman-worker-agent,team-leader-agent}/skills/<name>/`
   (one source dir **copied** across runtimes by a small Makefile/CI copy step — **not a git symlink**:
   this checkout has `core.symlinks=false` and Docker COPY doesn't dereference, so a committed symlink
   ships as a broken text file), rebuild + **redeploy the controller
   image** → it reaches every worker (incl. the 14 migrated) within ~5 min. The real constraint is
   "controller rebuild + redeploy," not "create-only."
2. **Market skills → pre-fetch at build time** (`@nacos-group/cli skill-get`) into a builtin dir rather than
   relying on a runtime nacos fetch; point `HICLAW_SKILLS_API_URL` at your registry. ⚠️ `market.hiclaw.io`
   reachability from your boxes is unverified — self-host Nacos or switch to a `skills.sh`-style HTTP
   registry if it's not reachable.
3. ✓ **Workers self-install skills (DECIDED 2026-06-30).** So **stop `pull_all` from pruning local skills
   absent from MinIO** (`copaw/src/copaw_worker/sync.py:709-716`) — otherwise a worker's `find-skills
   install` is reverted on the next ~5-min sync. Keep the operator's builtin set as the floor and let
   workers add on top. **Floor semantics, explicit:** the ~5-min `Overwrite:true` builtin mirror still
   wins any *name collision* — a self-installed skill sharing a builtin's directory name is re-overwritten
   every reconcile regardless of the pruning fix; workers durably own only non-builtin names. Also fix the stale "workers can't modify own skills" docs (`skills-management.md:38`,
   `worker-skills/README.md:56`). ⚠️ Verify Hermes FileSync has no equivalent pruning during the §7 spike.
   (Heads-up: this loosens reproducibility — a worker's exact skill set is no longer fully operator-defined;
   the dashboard file browser, Phase 5, is where you'd inspect what a worker self-installed.)

**Recommended approach — CLIs (image baking)**
- CLI baseline is **uneven**: Hermes richest (`ripgrep`/`ffmpeg`/`build-essential`/`cmake`/`@nacos-group/cli`);
  **CoPaw lacks** `ripgrep`/`ffmpeg`/`@nacos-group/cli`; OpenHuman has no node/mcporter/skills (Rust binary).
- Prefer **per-role images on a shared enriched base** + a CLI-parity layer per Dockerfile (e.g. CoPaw
  `+= ripgrep ffmpeg @nacos-group/cli`) over **one fat multi-GB image**. Bump tags in
  `helm/hiclaw/values.yaml:268-281` + the `Makefile` `*_IMAGE` vars.
- ⚠️ A baked **baseline skill set must be runtime-aware** — only `find-skills` + `mcporter` are common to
  all three runtimes; CoPaw's builtin set differs entirely (communication/file-sharing/organization/
  task-management). There is no single "universal worker skills" list.

**Worker = config + image (so you know where each enhancement goes)**
The **IMAGE** (chosen by `spec.runtime`; `spec.image` overrides) carries the engine + CLIs + stock builtin
skills. The **CONFIG** (persona/role/model/skills/MCP/env) is per-worker data in the CR / himarket template.
A himarket "worker template" is a **CONFIG package only — it carries no image**. So: persona/skills/MCP/model
= config (fast path, no rebuild); **new CLIs/runtimes = image** (rare rebuild). ⚠️ Note the image default
basenames differ (`hiclaw/copaw-worker:latest` etc.), and the installer never sets
`HICLAW_OPENHUMAN_WORKER_IMAGE`. ✓ **Default worker runtime DECIDED (2026-06-30): `hermes`** — make the
three sources agree: update the installer (`copaw`) and `helm/hiclaw/values.yaml:282` (`openclaw`) to
`hermes` (the plan + locked-in table already say Hermes).

**Acceptance:** drop one operator skill in the builtin dir + redeploy the controller → every worker has it
within ~5 min with zero per-worker steps; a CoPaw image bump gives CoPaw workers `ripgrep`/`ffmpeg`.

---

### Phase 5 — Management dashboard  · effort L · risk M
**Goal:** a web UI = **Project browser + visual task board + file browser**, behind Traefik at
`hq.pawcommit.com`. **Progress tracking moves out of Matrix.**

**Data sources (already exist — no agent re-instrumentation needed for v1):**
**→ Exact JSON contracts + the new `/api/v1/manager-tasks` endpoint: see §10.2.**
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
3. **Serve behind Traefik at `hq.pawcommit.com` with forward/basic-auth (✓ decided #1, #3)** — same pattern
   as `git-mcp.pawcommit.com`, no app-level auth code. Serve via nginx alongside Element (the controller
   has **no** static-file serving in `internal/server/` today — hosting the SPA from the controller would
   be net-new code; §10.2's same-origin proxy design assumes the nginx path).
4. **Control tiers (✓ #17 — v2.3):** the controller already has the write surface; the dashboard climbs
   a ladder instead of staying read-only forever. **v1** read-only (as above) → **v1.1** wake/sleep
   buttons proxying the existing `POST /api/v1/workers/{name}/wake|sleep|ensure-ready`
   (`lifecycle_handler.go:35-160`) with a confirm dialog + proxy request-log (Traefik basic-auth can't
   identify callers, so the log is the audit trail) → **v1.5** two ~20-line controller endpoints
   `POST /api/v1/managers/{name}/message` + `POST /api/v1/teams/{name}/message` (gated
   `RequireAuthz(ActionUpdate, …)`, delivered via the existing `SendMessageAsAdmin`,
   `internal/matrix/client.go:645-658`) so "start project X", "pause project" (→ the lead's
   `projectflow(pause_project)`, #16), and needs-input replies become dashboard actions riding the
   existing chat orchestration → **Phase 4+** native `POST /api/v1/projects` mirroring the existing
   `CreateTeam` handler pattern (zero new authz code — `authorizer.go` admin/manager roles don't inspect
   ResourceKind). The proxy's GET-only rule becomes a **scoped allowlist**; this consciously amends #3's
   "no app-level auth code" spirit — still no login UI, but method-scoped writes + logging. `/docker/`
   is **never** exposed to the UI beyond read-only logs (it allows exec behind one coarse authz check).
5. **Observability kit (v2.3):** (a) a per-container **Logs tab** via the existing `/docker/` reverse
   proxy (`http.go:123-128`, embedded mode; GET-only passthrough) — nearly free; (b) enable the
   built-but-disabled Prometheus endpoint (`app.go:651` `BindAddress:"0"` → `":8091"`, container-network
   only, never through Traefik) and render a controller health card; (c) the `shared/events/*.jsonl`
   schema is **specified now, built only if the inferred-progress view proves insufficient**:
   `{ts, actor, event: progress|subtask_done|blocked, subtask?, detail}` — hermes/openhuman workers
   already `mc mirror` the whole task dir, so the file would ride along free; (d) Higress exposes **no
   per-consumer LLM-spend** API (console API = provisioning endpoints only) — don't design a spend view
   against it.

**Known gaps to design around:** no `%`-complete / per-subtask status / live stream today — v1 infers
from Phase + timestamps; add a lightweight event log (`shared/events/*.jsonl`) only if needed.

**Acceptance:** at `hq.pawcommit.com` you see all Projects, a live-ish task board, team/worker status,
and can browse/download files — without touching Matrix for tracking.

---

### Phase 5b — Matrix verbosity control  · effort S · risk L–M
**Goal:** quiet rooms — keep **activity-of-life** ("am I still working?") but drop the 300-msg/turn
tool-call spam. Detail lives in the dashboard (Phase 5), so this pairs with it.

**Reconciling with proactive Manager nudges (✓ decided #5):** these are *not* in tension. The noise to kill
is per-tool-call **worker** chatter; the nudges to keep are coarse **Manager→you** status events (a task
stalls / finishes / needs input), driven by the Manager heartbeat (Phase 1 step 6). Quiet the workers,
keep the Manager's event-level pings.

**Verified mechanics (the two runtimes need different fixes)**
- **CoPaw (✓ #15 — two files, fix both):** the overlay channel (`copaw/src/matrix/channel.py`) is the
  **Manager's** runtime channel (baked in by `manager/Dockerfile.copaw:86-96`); CoPaw leads/workers run
  the near-duplicate standalone `copaw_worker/matrix_channel.py` — apply the same suppression to both.
  The overlay posts only the **final `m.text`
  reply** + a readiness probe; the per-tool-call / reasoning chatter comes from CoPaw's installed
  `BaseChannel` (external package), gated by forwarded flags. The one open tap is **`show_tool_details`**
  (defaults **True**, never set anywhere — `config.py:1164`). Typing indicators + read receipts are already
  an activity-of-life signal (`channel.py:2090,2107-2186`). All sends are `m.text` (no `m.notice`), so
  clients can't down-rank them.
- **Hermes:** all outbound Matrix flows through hermes-agent's **native** mautrix adapter (cloned at build,
  renamed `_matrix_native.py`); the fork overlay only adds inbound gating + an `m.mentions` wrapper.
  ⚠️ The bridge writes `MATRIX_FILTER_TOOL_MESSAGES` / `MATRIX_FILTER_THINKING="true"` but **nothing reads
  them — they are dead knobs** (the bridge comment is wrong). The overlay's `send_message_event` wrapper
  (`overlay_adapter.py:109-132`) is a real outbound chokepoint where suppression can be added **without
  forking upstream**.

**Recommended default**
1. **CoPaw:** inject `"show_tool_details": false` into the bridge-written **`config.json`** matrix dict
   (`bridge.py:226-239` — the runtime-effective one; also the agent.json overlay), behind an env gate
   `HICLAW_QUIET_ROOMS`. Keep typing/read-receipts as the heartbeat-of-life. ⚠️ **Efficacy is UNCERTAIN** —
   the stream lives in the external `BaseChannel`; validate on a live worker before committing (do this in
   the §7 spike).
2. **Hermes:** wire the already-planted (dead) `MATRIX_FILTER_*` flags into the overlay. Add a
   `should_suppress_outbound(content)` to `policies.py` (mirroring `apply_outbound_mentions`) and enforce it
   at the `send_message_event` wrapper — skip tool-call/intermediate/thinking events, **always pass
   start/finish/heartbeat**. Handle `m.relates_to.rel_type == "m.replace"` if steps stream as edits. Fix the
   misleading bridge comment.
3. **Heartbeat-of-life:** lengthen/consolidate the heartbeat (CoPaw cadence 30m manager / 10m worker,
   `agent.manager.json:11-14`) and lean on typing indicators; define "heartbeat" semantics so suppression
   doesn't kill wanted start/finish/keepalive messages.

**⚠️ Confirm in the Hermes-interop spike (§7):** the streaming *shape* — new events (easy to drop) vs
`m.replace` edits of one message (keep only the final) — and whether the native adapter bypasses
`send_message_event` for streaming (then a lower-level method must be overridden). Version-dependent: pin is
hermes **v0.10.0**, plan targets **v0.17.0** — on the bump, check for a native `platforms.matrix.*` suppress
flag and prefer it if present.

**Acceptance:** a worker runs a multi-tool task → the room shows **start + periodic still-working + final
result**, not a message per tool call; the dashboard still has the full detail.

---

### Phase 6 — Easy file access  · effort M · risk M
**Goal:** drag-drop a file in chat and a worker receives it; results come back.

**Steps**
1. **Matrix-attachment → MinIO bridge:** on upload, push into the task/project dir so workers can
   `mc` it. *(CoPaw — both files carry the near-duplicate generic hook, per #15: Manager overlay
   `channel.py:528-535`; leads/workers standalone `copaw_worker/matrix_channel.py` — registered for
   Image/File/Audio/Video at `:395-396`, handler at `:1210`. Today both download to `WORKING_DIR/media`;
   redirect to MinIO. Hermes: ⚠️ **no equivalent hook exists** — `overlay_adapter.py:179-239` only
   intercepts `m.image` (vision downgrade); generic `m.file`/`m.audio`/`m.video` pass straight through
   to the **un-vendored** native adapter → **spike S9**. Fallback: route attachments through the CoPaw
   lead — its standalone channel has the hook (`copaw_worker/matrix_channel.py:395-396,1210`) — into
   MinIO, and Hermes workers read from there.)*
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
  - ⚠️ **ORPHANED 4th patch (found in v2.2 pass):** `copaw/scripts/patch_copaw_stream_errors.py`
    (reclassifies stream disconnects as `ModelTimeoutException` instead of `UnknownAgentException` and
    extends the 60s stream timeout to 900s) **exists but is never COPY'd/RUN by `copaw/Dockerfile`** —
    the commit that added it (#847) touched the Dockerfile without wiring it in. **Wire it into
    `copaw/Dockerfile` NOW (v2.3 — Phase 0/1 hardening),** next to the sed patches at `:93-102` (both
    target the standard venv's copaw package); at bump time re-verify it still applies against the new
    exceptions.py shape, and delete it only if upstream fixed the misclassification.
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
  `hermes/src/hermes_matrix/overlay_adapter.py` subclasses the native `MatrixAdapter` (`_shim.py` only
  renames/re-exports). **The coupling surface is wider than `__init__` — re-diff all four overridden
  touchpoints against v0.17.0** (folded into spike S3): `connect()`/`self._client` + the monkey-patched
  `send_message_event` (`overlay_adapter.py:103-116`), `self._user_id` (`:127`),
  `_resolve_message_context`'s implicit 6-tuple return `(body, is_dm, chat_type, thread_id,
  display_name, source)` (`:134-177`) incl. `_is_dm_room`/`_get_display_name` (`:144,158`), and
  `_handle_media_message`/`_handle_text_message` (`:179-239`). Rebuild
  `hiclaw-hermes-worker`.

### Where image tags live
`helm/hiclaw/values.yaml` (`worker.defaultImage` per runtime, manager image) and the `Makefile`
image version vars. Bumping the runtime means rebuilding the corresponding `hiclaw-*-worker` /
`hiclaw-manager-copaw` images and updating tags.

---

## 7. Cross-cutting

**Hermes-interop spike (do early, during Phase 0/1):** stand up 1 CoPaw lead + 1 Hermes worker in a
throwaway team; confirm mention→task→result over Matrix and that Hermes consumes the team-worker
skills. This de-risks Phase 3 before the full restructure. **Fold two Phase 5b checks into this same
spike** (both flagged uncertain): (a) does CoPaw `show_tool_details=false` actually silence the stream
(it lives in the external `BaseChannel`), and (b) does Hermes stream steps as new events or `m.replace`
edits, and does the native adapter route them through `send_message_event`? **Plus the Phase 6 check
(S9):** drop an `m.file` on the Hermes worker and find where (if anywhere) the native adapter lands it —
the overlay has no generic inbound-media hook.

**→ Full spike table with PASS/FAIL exit-criteria for every ⚠️ item: see §10.3.**

**Hardening:**
- **Rotate MinIO creds** (currently `admin` / weak default) and the gateway/LLM keys; keep them in
  the install env file, not in image layers.
- **Per-worker Gitea PATs (post-#4):** give the #12 helper a `--rotate` mode and set a rotation cadence.
  Note the **Higress config store now holds every worker's PAT** (as `mcp-gitea-<worker>`
  `defaultCredential`s) — confirm whether the daily `hiclaw-snapshot` archives capture it, and
  encrypt/exclude those archives accordingly, or every PAT lands in plaintext backups.
- **Embedded-container health (v2.3):** add a Docker `HEALTHCHECK` to `Dockerfile.embedded` — one
  composite CMD hitting the controller `/healthz`, MinIO `/minio/health/live`, and Tuwunel
  `/_matrix/client/versions` — so `docker ps` reflects inner-subsystem crash-loops instead of only
  supervisord/PID-1 liveness. Plus a startup pre-flight `PRAGMA integrity_check` on the kine SQLite DB
  before `StartKine` (log loudly and refuse a silent fallback-to-empty on corruption — that file is the
  source of truth for all CRs).
- **Capacity budget (v2.3):** worst-case hot-burst RAM — Manager + 4 CoPaw leads at ~150–500 MB each,
  14 Hermes workers at ~150–500 MB each (`windows-deploy.md:50,294-305`) ≈ **3–9.5 GB for agents alone**,
  before the embedded stack (Higress+Tuwunel+MinIO+Element+controller, unmeasured) and the box's other
  tenants (Gitea, 3× act_runner, filebrowser, …) — against 16 GB shared. Mitigations: the Phase 0 step-6
  docker-limits fix (hard caps), `workerIdleTimeout` guidance of **30–60m** (the CRD default is 720m,
  `types.go:562` — far too lazy for this box; and note idle-sleep is *advisory*: the lead decides,
  `coordination.go:87-90`, the controller enforces nothing), and running heavy teams on the local
  satellite. Spot-check real tenant usage with `docker stats` / `free -h` on the VPS before Phase 3.
- Keep `console.pawcommit.com` and any MinIO console behind Traefik auth.
- Continue the daily `hiclaw-snapshot` backups; snapshot before each phase's deploy.

**Execution:** the **Claude Code on the VPS** can run the build/deploy/debug steps directly on-box.
Prefer building images there (or pull from a registry the box can reach — note the default registry is
the Aliyun CN one; mirror or build locally).

---

## 8. Decisions log (v2 — resolved 2026-06-30)

*All v1 + v2 open questions are now resolved except #9, deferred with the Option A choice. ✓ = decided, ⏸ = deferred. v2.2 adds #14–#15; v2.3 adds #16–#18.*

1. ✓ **Dashboard domain — `hq.pawcommit.com`** (dedicated subdomain behind Traefik).
2. ✓ **Project scope — always team-scoped** (every Project belongs to one functional Team).
3. ✓ **Dashboard auth — Traefik forward/basic-auth** (same pattern as `git-mcp.pawcommit.com`; no app-level auth).
4. ✓ **Per-worker Gitea identity through the gitea-mcp (revised 2026-06-30).** Each worker gets its **own Gitea user + scoped PAT** — so its PRs are attributable and it fixes its own review feedback. **Mechanism (verified, gitea-mcp source-cloned):** register **one gitea-mcp Higress MCP-server per worker** (`mcp-gitea-<worker>`, reusing `setup-mcp-proxy.sh`) carrying that worker's PAT as the upstream `defaultCredential`, gated via `allowedConsumers` to that worker's consumer. **Higress holds the PATs** (credential firewall preserved); gitea-mcp honors the per-server Bearer over its own env token (header > flag > env). Still through the gitea-mcp — **no native-git-as-primary, no per-repo deploy keys, no parallel REST client, no Sudo** (gitea-mcp strips it). **Provisioning (#12): an operator-run helper script** (`scripts/provision-worker-gitea.sh`) creates the user+PAT and registers the per-worker server — *not* controller code. **RW/RO (#13): enforced via each worker-user's Gitea repo-collaborator role** (ro→read, rw→write), not advisory. Hermes native checkout reuses the worker's own PAT via a credential helper (spike S-GIT).
5. ✓ **Manager proactivity — proactive nudges** (heartbeat-driven, event-level: stall / finish / needs-input).
   Coarse Manager→you status — coexists with quiet rooms (Phase 5b).
6. ✓ **Project storage — build a `Project` CRD now** (declarative, controller-reconciled), with a MinIO
   projection for flat reads (Phase 4).
7. ✓ **Multi-provider authorization (Phase 2b) — DECIDED 2026-06-30: pin per agent/team** via
   `spec.modelProvider`; no agent left on the authorize-all default (`higress.go:192-323`).
8. ✓ **Local satellite & Matrix (Phase 0b) — DECIDED 2026-06-30: Option A** — Matrix stays per-instance;
   the dashboard is the cross-instance pane.
9. ⏸ **Instance discriminator (Phase 0b) — deferred** with the Option A decision; not needed unless you
   later share one homeserver (Option B). `HICLAW_INSTANCE_ID` prefixing stays unbuilt for now.
10. ✓ **Skills source-of-truth (Phase 4.5) — DECIDED 2026-06-30: allow worker self-install** — stop
    `pull_all` pruning local skills absent from MinIO (`sync.py:709-716`) so `find-skills install` persists.
11. ✓ **Default worker runtime (Phase 4.5) — DECIDED 2026-06-30: standardize on `hermes`** — update the
    installer (`copaw`) and `helm/hiclaw/values.yaml:282` (`openclaw`) to match.
12. ✓ **Per-worker Gitea provisioning (#4/Phase 4) — DECIDED 2026-06-30: operator-run helper script.**
    `scripts/provision-worker-gitea.sh` (operator-run, not the controller) creates each worker's Gitea user +
    scoped PAT, registers the per-worker `mcp-gitea-<worker>` Higress server, and sets repo-collaborator
    membership. Keeps the controller free of any Gitea API client / PAT handling.
13. ✓ **Per-repo RW/RO (#4/Phase 4) — DECIDED 2026-06-30: enforced via Gitea repo-collaborator roles**
    (ro → read, rw → write), driven by the Project manifest — not advisory.
14. ✓ **mcp-gitea consumer isolation — DECIDED 2026-07-02.** `provision-worker-gitea.sh` (#12) must
    **not** run `setup-mcp-proxy.sh`'s Step 5 as-is: Step 5 (`setup-mcp-proxy.sh:326-339`)
    REPLACE-broadcasts **all** registry workers' consumers onto whatever server it registers and rewrites
    every worker's `mcporter.json` — voiding the per-worker isolation decision #4 depends on. The helper
    runs steps 1–4, then PUTs `/v1/mcpServer/consumers` = `[<that worker>]` itself and updates only that
    worker's `mcporter.json`. S-GIT gains an isolation assertion (worker A's key → 401/403 on worker B's
    server). Outside-in, nothing changes: workers just get their git tools.
15. ✓ **CoPaw channel split — RESOLVED STATICALLY 2026-07-02 (supersedes S1's open framing).** The
    Manager runs the overlay `copaw/src/matrix/channel.py` (baked in by `manager/Dockerfile.copaw:86-96`);
    CoPaw leads/workers run the standalone near-duplicate `copaw_worker/matrix_channel.py` (installed
    into `custom_channels/` by `worker.py:570-583`; the worker image applies only a one-line sed fix to
    the built-in, no overlay). Phase 1 + 5b fixes land in **both** files; S1 is demoted to a quick
    live-confirm. Leads stay CoPaw forever (leader runtime forced), so the standalone file matters even
    after the Hermes migration.
16. ✓ **Project CRD ↔ projectflow federation — DECIDED 2026-07-02 (v2.3).** The CoPaw lead runtime
    already ships a complete project-execution system: the `projectflow`/`taskflow` MCP tools with
    `create_project`/`plan_dag`/`pause_project`/`resume_project`/`complete_project`
    (`copaw/src/copaw_worker/hooks/tools/projectflow.py`, `task.py:191-214,496-541`) and their own
    `meta.json` schema. The Phase 4 CRD is a **second, federated concept** — CRD = repo/access
    provisioning; projectflow = work execution; linked by project id, **no schema merge**. §10.2 table
    F's "shape unchanged" claim was false and is corrected. The reconciler notifies the lead's Team Room
    on new Projects (step 8b) since nothing watches `shared/projects/` otherwise.
17. ✓ **Dashboard control tiers — DECIDED 2026-07-02 (v2.3).** v1 read-only → v1.1 wake/sleep
    passthrough (existing `POST /api/v1/workers/{name}/wake|sleep`, `lifecycle_handler.go:35-160`) →
    v1.5 message-injection endpoints (`POST /api/v1/managers/{name}/message` +
    `/api/v1/teams/{name}/message` via the existing `SendMessageAsAdmin`) → Phase 4+
    `POST /api/v1/projects`. Consciously amends #3's "no app-level auth code": still Traefik-only login,
    but the proxy grows a scoped write allowlist + request log as the audit trail. `/docker/` is never
    exposed to the UI beyond read-only logs.
18. ✓ **Project completion lifecycle — DECIDED 2026-07-02 (v2.3).** `status.phase` gains
    **Completed/Archived** as operator-set states, signalled by the lead's
    `projectflow(complete_project)`; on Completed the reconciler raises `DeprovisionPending=True` and
    the dashboard surfaces "run `provision-worker-gitea.sh --deprovision <id>`" (#12 gains the flag;
    controller still makes no Gitea calls). CR delete remains hard cleanup.

---

## 9. Appendix — key references

**Code hot-spots:** `manager/agent/AGENTS.md` · `manager/agent/copaw-manager-agent/` ·
`manager/agent/skills/` + `worker-skills/` · `copaw/src/matrix/channel.py` +
`copaw/src/copaw_worker/matrix_channel.py` (✓ #15 — Manager vs leads/workers) ·
`hermes/src/hermes_matrix/{_shim.py,overlay_adapter.py}` ·
`hiclaw-controller/internal/controller/{member_reconcile,team_controller,manager_controller}.go` ·
`hiclaw-controller/internal/service/provisioner.go` · `hiclaw-controller/internal/gateway/higress.go` ·
`hiclaw-controller/internal/server/http.go` (REST :8090) · `install/hiclaw-install.sh` ·
`hiclaw-controller/supervisord.embedded.conf`.

**v2.1 net-new (from §10.1 Project CRD):** `hiclaw-controller/internal/controller/project_controller.go` ·
`hiclaw-controller/internal/service/project_provisioner.go` (MinIO projection **only** — **no** Gitea client,
**no** gateway calls) · `config/crd/projects.hiclaw.io.yaml` +
the Helm copy `helm/hiclaw/crds/projects.hiclaw.io.yaml` · `api/v1beta1/types.go` (append `Project*` types) +
`internal/config/config.go` (additions).

**v2.1 net-new (operator-side, NOT controller — decisions #12/#13):**
`scripts/provision-worker-gitea.sh` (operator helper: creates each worker's Gitea user + scoped PAT via the
Gitea admin API, registers the per-worker `mcp-gitea-<worker>` Higress server, sets repo-collaborator
membership from the Project manifest — **the controller has no Gitea API client and never handles PATs**) ·
the per-worker `mcp-gitea-<worker>` Higress MCP-server registrations (via `setup-mcp-proxy.sh`, one per worker,
each carrying that worker's PAT as the upstream `defaultCredential`).

**v2 hot-spots (new tracks):**
- *Multi-provider (2b):* `hiclaw-controller/internal/gateway/higress.go` (`ResolveModelProvider:564-643`,
  `AuthorizeAIRoutes:181-323`) · `internal/initializer/initializer.go:299-438` ·
  `manager/scripts/init/setup-higress.sh` · `internal/agentconfig/generator.go:126-144,451-494` ·
  `manager/configs/{known-models.json,manager-openclaw.json.tmpl}` ·
  `manager/agent/skills/{model-switch,worker-model-switch}/` · `docs/faq.md:540-566` (multi-vendor pattern).
- *Harness enrichment (4.5):* `hiclaw-controller/internal/service/deployer.go:486-567,833-898`
  (`pushBuiltinSkills`/`pushRemoteSkills`/`builtinAgentDir`) · `internal/controller/worker_controller.go:29,151,171`
  (reconcile cadence) · `manager/agent/skills/worker-management/scripts/push-worker-skills.sh` ·
  `copaw/src/copaw_worker/sync.py:709-749` · the worker Dockerfiles + `helm/hiclaw/values.yaml:268-282`.
- *Local satellite (0b):* `hiclaw-controller/internal/backend/docker.go:85-95,491-599` (no resource caps) ·
  `internal/controller/member_reconcile.go:414-448` · `internal/config/config.go:319,379-385` ·
  `internal/initializer`/`appservice.go` (exclusive `@.*` namespace) · `internal/service/provisioner.go:211-213,308-311`
  (Matrix naming) · `internal/minio/minio_admin.go:124-162` (bucket) · `install/hiclaw-install.ps1` ·
  `Makefile:632-633` · `manager/scripts/init/start-tuwunel.sh:26-27` · `docs/windows-deploy.md`.
- *Matrix verbosity (5b):* `copaw/src/matrix/channel.py` + `copaw/src/copaw_worker/matrix_channel.py`
  (✓ #15) + CoPaw `config.py:1164` (`show_tool_details`) ·
  `copaw/src/copaw_worker/bridge.py:226-239` · `hermes/src/hermes_matrix/{overlay_adapter.py:109-132,policies.py}` ·
  `hermes/src/hermes_worker/bridge.py:249-253` (dead `MATRIX_FILTER_*`).

**Gitea MCP integration target:** `http://gitea-mcp:8080` on the `proxy` net → register **one Higress
MCP server per worker** (`mcp-gitea-<worker>` via `setup-mcp-proxy.sh`), each carrying that worker's own
scoped PAT as the upstream `defaultCredential` and gated by `allowedConsumers` to that worker's consumer
(per-worker Gitea identity; Higress holds the PATs); public mirror at `git-mcp.pawcommit.com`.

**Friction catalog (condensed, from analysis):** dead-air cold start; one-shot onboarding;
bare-`@mention` silent drop; CoPaw heartbeat-deferred replies; push-before-notify race; first-boot
message suppression; no status visibility without `docker exec`; agent-only file sync; GitHub-only git.

---

## 10. Build specs (v2.1 spec-readiness pass)

These are the implementation-ready artifacts that raise the four weakest spots — the Project CRD, the
dashboard ↔ controller data contracts, the spike exit-criteria, and the worker → team migration mapping
— to the bar where an unfamiliar engineer could build them. Each was drafted against the code and
adversarially verified; verifier corrections are folded in below, and any claim a verifier refuted or
left uncertain is fixed or clearly marked ⚠️ rather than asserted as fact.

### 10.1 Project CRD — implementation-ready design

> **Verifier verdict:** *mostly-solid*. **Revised 2026-06-30:** the controller has **no** Gitea client —
> no REST client, no per-repo deploy keys, no scoped tokens minted in the controller. Per-worker Gitea
> identity (own user + scoped PAT), the `mcp-gitea-<worker>` registration, and the repo-collaborator role
> are provisioned by an **operator-run helper** (`scripts/provision-worker-gitea.sh`, #12) from the manifest —
> out-of-band, not controller code. Remaining precision fixes (`addKnownTypes` casing, a line cite, the
> `.length` jsonPath) are folded in.

> Builds decision #6 (real CRD, not a JSON manifest) + #2 (team-scoped) + #4 (per-worker Gitea identity via
> the existing gitea-mcp — no controller Gitea client) + #12/#13 (operator helper, enforced RW/RO).
> Mirrors the Worker/Team controllers. New files:
> `api/v1beta1/types.go` (append), `config/crd/projects.hiclaw.io.yaml` **+ `helm/hiclaw/crds/projects.hiclaw.io.yaml` (keep in sync)**,
> `internal/controller/project_controller.go`,
> `internal/service/project_provisioner.go` (MinIO projection only — **no** Gitea client, **no** gateway calls; the per-worker Gitea identity / `mcp-gitea-<worker>` registration / collaborator role are applied by the operator helper #12).
> Register in `api/v1beta1/register.go:19-29` (the unexported `addKnownTypes`, which calls `scheme.AddKnownTypes`) and `internal/app/app.go:530-597`.

#### 0. Critical context found in the code (read before building)

- **Git access reuses the existing gitea-mcp — no controller Gitea client, per-worker identity.** Each worker reaches Gitea as its **own Gitea user + scoped PAT** through a per-worker `mcp-gitea-<worker>` Higress server (Higress holds the PAT; the worker presents only its consumer key — plan §3, Git MCP row). The Project controller does **only** two things: project the CR → MinIO manifest, and record assigned workers + per-repo `access`. **No Gitea REST client, no per-repo deploy keys, no scoped tokens, no PAT handling, no project credential storage** — the per-worker Gitea user/PAT, `mcp-gitea-<worker>` registration, and collaborator role are applied by the operator helper (#12) from the manifest. (high confidence)
- **Workers are stateless; gateway/FS secrets reach them via container env**, built in `service/worker_env.go:21-37` (`Build`) — keys like `HICLAW_WORKER_GATEWAY_KEY`, `HICLAW_FS_SECRET_KEY`. The worker presents its gateway consumer key to its own `mcp-gitea-<worker>` server (which carries the worker's PAT upstream) — no per-project secret and no PAT is injected into the worker. (high confidence)
- **OSS interface** (`oss/client.go:7-33`) gives `PutObject`, `GetObject`, `DeleteObject`, `DeletePrefix`, `Mirror` — exactly what the MinIO projection + cleanup need. Deployer already uses `d.oss.PutObject` / `d.oss.Mirror` (`deployer.go:826,852`). (high confidence)
- **Finalizer constant is shared**: `finalizerName = "hiclaw.io/cleanup"` (`worker_controller.go:28`), reconcile cadence `reconcileInterval = 5*time.Minute`, retry `30*time.Second` (`:29-30`). Reuse all three. (high confidence)
- **Two finalizer idioms exist** — Worker uses `client.MergeFrom` patch + deferred status patch (`worker_controller.go:95-110`); Team/Human use `r.Update` (`team_controller.go:96-118`). Mirror the **Worker** idiom (status subresource + deferred merge-patch) since Project has a rich status. (high confidence)
- **Per-worker gitea-mcp registration reuses the Higress proxy path** — `setup-mcp-proxy.sh` registers one `mcp-gitea-<worker>` per worker, each carrying that worker's PAT as the upstream `defaultCredential` and gated by `allowedConsumers` to that worker's consumer (`internal/gateway/higress.go` for the consumer/route mechanics). Higress holds the PATs; the operator helper (#12) runs the per-worker registration, not the controller. (high confidence)
- **The runtime already has its own Project system (✓ #16 — v2.3).** The CoPaw lead's `projectflow`/`taskflow` MCP tools (`copaw/src/copaw_worker/hooks/tools/projectflow.py`, `copaw/src/copaw_worker/task.py:36-44,191-214,496-541`) implement `create_project`/`plan_dag`/`pause_project`/`resume_project`/`complete_project` with their **own** `meta.json` schema (`source`/`requester`/`parent_task_id` — no `team`, no `repos`). The CRD is a **second, federated concept**: CRD = repo/access provisioning ("infra project"); projectflow = work execution — linked by project id, **not** schema-merged. Mid-flight steering (pause/resume/reprioritize) therefore needs **no CRD field**: it's a message to the lead (#17 tier v1.5), and the lead's existing tool actions do the rest. (high confidence)

---

#### 1. `config/crd/projects.hiclaw.io.yaml`

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: projects.hiclaw.io
spec:
  group: hiclaw.io
  versions:
    - name: v1beta1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              required: [team, repos]
              properties:
                team:
                  type: string
                  description: Name of the Team this Project is scoped to (required — team-scoped, decision #2). Must reference an existing Team CR.
                description:
                  type: string
                projectName:
                  type: string
                  description: Runtime/storage identity (MinIO path key shared/projects/<id>/). Defaults to metadata.name.
                repos:
                  type: array
                  minItems: 1
                  description: Repos this Project binds. Exactly one repo SHOULD have access=rw (the working repo); the rest are ro source repos.
                  items:
                    type: object
                    required: [url, access]
                    properties:
                      url:
                        type: string
                        description: "Full clone URL, e.g. https://git.pawcommit.com/owner/repo.git or ssh://git@git.pawcommit.com:4443/owner/repo.git"
                      access:
                        type: string
                        enum: [rw, ro]
                        description: "rw|ro is enforced via the assigned worker-user's Gitea repo-collaborator role (#13), applied by the provisioning helper; carried in the manifest as the source of that mapping."
                      name:
                        type: string
                        description: Optional friendly label surfaced in the dashboard. Defaults to the owner/repo slug parsed from url.
                workers:
                  type: array
                  description: "Optional. Worker runtime-names (Matrix localpart / OSS path key) assigned to this Project. When empty the controller assigns the creds to all current members of spec.team."
                  items:
                    type: string
            status:
              type: object
              properties:
                observedGeneration:
                  type: integer
                  format: int64
                phase:
                  type: string
                  enum: [Pending, Provisioning, Ready, Degraded, Failed, Completed, Archived]
                  description: "Pending→Provisioning→Ready are reconciler-computed; Completed/Archived are operator-set (✓ #18), signalled by the lead's projectflow(complete_project)."
                message:
                  type: string
                repoCount:
                  type: integer
                  description: "Number of repos bound by this Project; backs the Repos printer column."
                recordedWorkers:
                  type: array
                  description: "Workers recorded in the manifest (with their per-repo access) on the last successful reconcile; the operator helper (#12) provisions each one's Gitea user / mcp-gitea-<worker> / collaborator role from it."
                  items:
                    type: string
                conditions:
                  type: array
                  items:
                    type: object
                    required: [type, status]
                    properties:
                      type:
                        type: string
                        description: "ReposResolved | WorkersRecorded | MinIOProjected"
                      status:
                        type: string
                        enum: ["True", "False", "Unknown"]
                      reason:
                        type: string
                      message:
                        type: string
                      lastTransitionTime:
                        type: string
                        format: date-time
      subresources:
        status: {}
      additionalPrinterColumns:
        - name: Team
          type: string
          jsonPath: .spec.team
        - name: Repos
          type: integer
          jsonPath: .status.repoCount   # status.repoCount is an int the reconciler populates (k8s additionalPrinterColumns jsonPath does not support a .length function on arrays).
        - name: Phase
          type: string
          jsonPath: .status.phase
        - name: Age
          type: date
          jsonPath: .metadata.creationTimestamp
  scope: Namespaced
  names:
    plural: projects
    singular: project
    kind: Project
    shortNames: [proj]
```

Shape mirrors `workers.hiclaw.io.yaml` exactly: `subresources.status: {}` (`workers…yaml:260-261`), `additionalPrinterColumns` (`:262-274`), `scope: Namespaced` + `names` block (`:275-281`). The `enum`/`required`/`pattern` validation idiom is copied from the Worker spec. **The Helm copy is automatic:** `make generate` already runs `cp config/crd/*.yaml ../helm/hiclaw/crds/` after deepcopy (`hiclaw-controller/Makefile:50-53`) — so write the CRD to `config/crd/` and run `make generate`; no manual copy to forget. (The four existing CRDs are duplicated at `helm/hiclaw/crds/{workers,teams,managers,humans}.hiclaw.io.yaml` by exactly this mechanism.)

---

#### 2. `api/v1beta1/types.go` — `Project` Go structs (append)

```go
// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Project binds a Team to a working repo (RW) plus optional source repos (RO).
// Git access is via the existing gitea-mcp with a per-worker Gitea identity (no
// controller Gitea client): the controller only projects a flat manifest to MinIO
// at shared/projects/<id>/ and records the assigned workers + per-repo access.
// RW/RO is enforced via the assigned worker-user's Gitea repo-collaborator role
// (#13), applied by the provisioning helper; the manifest carries the access as
// the source of that mapping. The CR is source of truth; MinIO is cache.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectSpec   `json:"spec"`
	Status            ProjectStatus `json:"status,omitempty"`
}

type ProjectSpec struct {
	Team        string        `json:"team"`                  // required — team-scoped (decision #2)
	Description string        `json:"description,omitempty"`
	ProjectName string        `json:"projectName,omitempty"` // runtime/storage identity; defaults to metadata.name
	Repos       []ProjectRepo `json:"repos"`                 // >=1; exactly one SHOULD be access=rw
	Workers     []string      `json:"workers,omitempty"`     // runtime-names; empty = all members of spec.team
}

// ProjectRepo binds one repo at a given access level.
// access=rw|ro is enforced via the assigned worker-user's Gitea repo-collaborator
// role (#13: ro→read, rw→write), applied by the provisioning helper; carried in
// the manifest as the source of that mapping.
type ProjectRepo struct {
	URL    string `json:"url"`
	Access string `json:"access"`         // rw | ro — enforced via the worker-user's Gitea collaborator role (#13)
	Name   string `json:"name,omitempty"` // friendly label; defaults to owner/repo slug
}

// EffectiveProjectName mirrors WorkerSpec.EffectiveWorkerName /
// TeamSpec.EffectiveTeamName (types.go:176-181, 247-252).
func (s ProjectSpec) EffectiveProjectName(metadataName string) string {
	if s.ProjectName != "" {
		return s.ProjectName
	}
	return metadataName
}

type ProjectStatus struct {
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	Phase              string                 `json:"phase,omitempty"` // Pending/Provisioning/Ready/Degraded/Failed + operator-set Completed/Archived (#18)
	Message            string                 `json:"message,omitempty"`
	RepoCount          int                    `json:"repoCount,omitempty"`          // backs the Repos printer column
	RecordedWorkers    []string               `json:"recordedWorkers,omitempty"`    // workers recorded in the manifest; operator helper (#12) provisions Gitea user/mcp-gitea-<worker>/collaborator role from it
	Conditions         []ProjectCondition     `json:"conditions,omitempty"`
}

type ProjectCondition struct {
	Type               string      `json:"type"`   // ReposResolved|WorkersRecorded|MinIOProjected
	Status             string      `json:"status"` // True|False|Unknown
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}
```

Conventions copied: `+genclient` / `+kubebuilder:subresource:status` / deepcopy markers (`types.go:92-94`), pointer-free required fields, `omitempty` on optionals, `Effective*` helper (`:176-181`), `ObservedGeneration int64` + string `Phase` in status (`:200-201`), the `*List` type. **Then add `&Project{}, &ProjectList{}` to the `scheme.AddKnownTypes(...)` call inside the unexported `addKnownTypes` function (`register.go:19-29` — note: the function is `addKnownTypes`, lowercase; there is no exported `AddKnownTypes` to grep for) and run `make generate` (deepcopy)** — `zz_generated.deepcopy.go` is code-genned.

---

#### 3. `internal/controller/project_controller.go` — reconcile design

Mirror `WorkerReconciler` (`worker_controller.go:61-214`): deferred status merge-patch, finalizer add on create, `reconcileDelete` on `DeletionTimestamp`, `RequeueAfter: reconcileInterval` (5 min) on success, `30s` on retry.

**Struct** (registered in `app.go` alongside the others, `:530-597`):
```go
type ProjectReconciler struct {
	client.Client
	OSS            oss.StorageClient          // for MinIO projection (a.oss in app.go)
	Provisioner    service.ProjectProvisioner // MinIO projection only — no Gitea client, no gateway calls
	ControllerName string
}
```

**Reconcile (idempotent step list):**
1. `Get` Project; `client.IgnoreNotFound`. Set up deferred status patch (copy `worker_controller.go:72-93`): on success write `ObservedGeneration = Generation`, clear `Message`; on error keep prior phase + set `Message`.
2. If `DeletionTimestamp != 0` and finalizer present → `reconcileDelete` (see below). If absent → return.
3. Add `finalizerName` if missing (`client.MergeFrom` patch, copy `:102-108`).
4. **Resolve team** — `Get` Team `spec.team`; if missing → Phase `Degraded`, condition `ReposResolved=False reason=TeamNotFound`, requeue `30s`. (Team-scoped invariant, decision #2.)
5. **Resolve assigned workers** — if `spec.workers` set, use it; else enumerate the Team's members from `Team.Status.Members[].RuntimeName` (`types.go:399-441`). Record into `status.recordedWorkers`. Set `status.repoCount = len(spec.repos)`.
6. **Record assigned workers + per-repo access (idempotent)** — for each assigned worker, record its name + the Project's `repos[].access` into `status.recordedWorkers` and the MinIO manifest (step 8). The **per-worker Gitea identity, the `mcp-gitea-<worker>` Higress registration, and the repo-collaborator membership are applied by the operator helper (#12) from that manifest — the controller makes NO Gitea calls and registers no shared route.** Set condition `WorkersRecorded`. The `access: rw|ro` from `spec.repos` is the **source of the collaborator-role mapping (#13)** the helper applies — enforced, not advisory.
7. *(no credential injection step)* — the worker presents its own consumer key to its own `mcp-gitea-<worker>` server (Higress holds the worker's PAT); nothing per-project is injected into the worker by the controller.
8. **MinIO projection** (§5) — `PutObject(shared/projects/<id>/manifest.json, …)` including each repo's `access` (the source of the worker-user collaborator-role mapping #13 the helper applies). Set condition `MinIOProjected`.
8b. **Notify the lead (✓ #16 — v2.3):** the first time `MinIOProjected` goes True, post a message into the team's existing `Team.Status.TeamRoomID` via the provisioner's `SendMessageAsAdmin` (`internal/matrix/client.go:645-658` — the controller's existing Matrix-out primitive, already used for onboarding at `provisioner.go:1534`): project id + manifest path + "run `projectflow(create_project)` to start planning". Track condition `LeaderNotified` for idempotence (fire once). **Without this the manifest lands silently** — nothing watches `shared/projects/`: the lead's `pull_all` never pulls `shared/` (`sync.py:654-718`), and the lead heartbeat only iterates *local* project dirs (`projectflow.py:137-145`). Optional belt-and-suspenders: a lead-heartbeat `mc ls` diff of remote vs local project ids surfacing `new_project_manifest_pending`.
9. Phase `Ready` when all conditions True; `Provisioning` while any in flight. **`Completed`/`Archived` (✓ #18) are operator-set, never reconciler-computed** — see the completion-lifecycle note below. Return `RequeueAfter: reconcileInterval`.

**`reconcileDelete` (finalizer cleanup):**
```
OSS.DeletePrefix(ctx, "shared/projects/<id>/")                                  (non-fatal)
RemoveFinalizer; r.Patch
```
The controller only deletes the MinIO projection — **no Gitea-side calls, no gateway calls.** The operator helper (#12) de-provisions the per-worker Gitea users / `mcp-gitea-<worker>` registrations / repo-collaborator membership out-of-band. The projection delete is **best-effort/non-fatal so a transient MinIO error never wedges finalizer removal** — exactly the Human delete contract (`human_reconcile_delete.go:31-44`) and the Worker delete (`worker_controller.go:192-210`, all `_ =`/logged-non-fatal).

**Status transitions:** `"" → Pending` (created, finalizer added) → `Provisioning` (recording/projection in flight) → `Ready` (all conditions True) → `Degraded` (team missing or partial recording failure) → `Failed` (only when nothing could be recorded/projected and MinIO is unreachable). Mirror `computeWorkerPhase` semantics: on transient error keep the prior non-empty Phase rather than flapping to Failed (`worker_controller.go:325-338`).

**Completion lifecycle (✓ #18 — v2.3):** `Completed`/`Archived` are **operator-set** phases (dashboard button or `hiclaw` patch), never reconciler-computed — the *signal* that work is done is the lead running `projectflow(complete_project)` (`task.py:496-541`, already implemented today). On `Completed` the reconciler raises condition `DeprovisionPending=True`, which the dashboard surfaces as "run `provision-worker-gitea.sh --deprovision <id>`" — #12's helper gains that flag; **the controller still makes no Gitea calls.** `Archived` additionally moves the MinIO projection to a cold prefix. CR delete stays the hard-cleanup path, unchanged. Without this lifecycle, finished projects hold live Gitea collaborator grants indefinitely — a real hygiene problem for a solo operator who won't remember manual cleanup.

**SetupWithManager:** `ctrl.NewControllerManagedBy(mgr).For(&v1beta1.Project{}).Complete(r)`. **Add a `Watches` on Team** (enqueue Projects whose `spec.team` changed membership) so adding a worker to a team re-records it in the manifest (the operator helper then provisions its Gitea user / `mcp-gitea-<worker>` / collaborator role) — mirror the Pod-watch wiring in `worker_controller.go:340-371` but keyed on Team. (TODO-CRD-4: a `spec.team` field indexer like `TeamLeaderNameField` at `team_controller.go:34-37`.)

---

#### 4. Git access — per-worker identity through the gitea-mcp (decisions #4/#12/#13)

**Each worker has its own Gitea user + scoped PAT; all access flows through the existing gitea-mcp.** Verified mechanism (gitea-mcp source-cloned, HEAD bbde7ee):
- gitea-mcp (HTTP mode) reads a per-request `Authorization: Bearer <PAT>` and builds the upstream Gitea client from it, **overriding** its own `GITEA_ACCESS_TOKEN` env (`operation.go:72-84` → `pkg/gitea/gitea.go:76-81` → `rest.go`; precedence header > flag > env). One gitea-mcp process serves N Gitea identities.
- **Registration:** one Higress MCP-server **per worker** — `mcp-gitea-<worker>` via `setup-mcp-proxy.sh`, proxying the existing gitea-mcp URL, with that worker's PAT as the upstream `defaultCredential`/`defaultUpstreamSecurity`, `allowedConsumers` limited to that worker's consumer. **Higress holds each PAT**; the worker presents only its existing consumer key — credential firewall preserved. (Today's shared `mcp-github` pattern broadcasts one server to all workers — `setup-mcp-server.sh` Step 5 — so the registration loop becomes per-worker; config only, no controller Go change for the gateway leg.
⚠️ **#14:** skip the script's Step-5 all-workers broadcast — the helper sets the single-consumer list
itself.)
- **Provisioning (✓ #12 — operator helper):** a standalone `scripts/provision-worker-gitea.sh` (operator-run, **not** the controller) creates the Gitea user + scoped PAT (`POST /users`, `POST /users/:name/tokens`), registers the per-worker mcp server, and sets repo-collaborator membership. **No Gitea admin client in the controller; the controller never handles PATs.**
- **RW/RO enforcement (✓ #13 — real):** the Project manifest's `repos[].access` maps to each assigned worker-user's **Gitea repo-collaborator role** (ro → read, rw → write), applied by the helper — a `ro` repo genuinely cannot be pushed. (PAT scopes are global per scope-block; the per-repo wall is the collaborator role.)
- **Hermes native checkout:** reuses the worker's **own PAT** via a git credential helper when a working tree is needed — spike **S-GIT**.

⚠️ Bench-test before relying on it (S-GIT): confirm Higress per-server `defaultCredential` attaches the Bearer to the upstream gitea-mcp so it overrides the env token, on the VPS Higress version — two workers with two PATs must yield PRs attributed to two different Gitea users.

**Cleanup:** on Project delete → `reconcileDelete` removes the MinIO projection and de-authorizes any consumer recorded **solely** for this Project (§3). The controller makes no Gitea calls; the operator helper (#12) de-provisions Gitea users / per-worker `mcp-gitea-<worker>` registrations / collaborator membership out-of-band.

---

#### 5. MinIO projection — CR → `shared/projects/<id>/`

CR is **source of truth, MinIO is cache** (decision #6). On each reconcile (step 8), write a **flat manifest** so the dashboard (Phase 5, which already reads `shared/projects/{id}/`, plan `:478,484`) and workers get a no-apiserver read path. The `access` field is the **source of the RW/RO mapping** the operator helper (#12) applies as each assigned worker-user's Gitea repo-collaborator role (#13) — there is no credential material here:

```go
manifest := map[string]any{
    "id":          projectID,            // EffectiveProjectName(metadata.name)
    "team":        proj.Spec.Team,
    "description": proj.Spec.Description,
    "repos": []map[string]string{        // repo URLs + access (source of the #13 collaborator-role mapping) — no creds
        {"url": r.URL, "access": r.Access, "name": r.Name},
    },
    "recordedWorkers": proj.Status.RecordedWorkers, // workers the operator helper (#12) provisions Gitea identity for
    "updatedAt":         metav1.Now().UTC().Format(time.RFC3339),
}
b, _ := json.Marshal(manifest)
_ = r.OSS.PutObject(ctx, "shared/projects/"+projectID+"/manifest.json", b)
```

Uses the exact `oss.StorageClient.PutObject(ctx, key, []byte)` signature (`oss/client.go:10`) the Deployer already calls (`deployer.go:826`). Cleanup on delete = `OSS.DeletePrefix(ctx, "shared/projects/"+projectID+"/")` (`oss/client.go:32`). **The manifest carries repo URLs + `access` (the source of the #13 collaborator-role mapping the operator helper applies) only — there are no credentials to leak** (git access is via each worker's own `mcp-gitea-<worker>`; Higress holds the PAT).

---

#### Open TODOs (decisions / live-VPS confirmation)

- **✓ RW/RO ENFORCED (#13)**: `spec.repos[].access` is **enforced** via each assigned worker-user's Gitea repo-collaborator role (ro → read, rw → write), applied by the operator helper (#12) from the manifest — a `ro` repo genuinely can't be pushed. Not advisory. (PAT scopes are global per scope-block; the per-repo wall is the collaborator role.)
- **SPIKE S-GIT**: per-worker-PAT native checkout — does a worker's **own scoped PAT** via a git credential helper support clone+push against `git.pawcommit.com` from a Hermes sandbox? (And does Higress per-server `defaultCredential` attach the Bearer so gitea-mcp uses the worker's PAT over its env token — distinct attribution for two workers.) (see §10.3 S-GIT.)
- **✓ NET-NEW OPERATOR HELPER (#12)**: `scripts/provision-worker-gitea.sh` is net-new and **owns all Gitea-admin + per-worker-mcp-registration work** — it creates each worker's Gitea user + scoped PAT (`POST /users`, `POST /users/:name/tokens`), registers the per-worker `mcp-gitea-<worker>` Higress server (via `setup-mcp-proxy.sh`), and sets repo-collaborator membership from the manifest. **Keeps the controller Gitea-free** (no Gitea client, no PAT handling, no gateway calls for the Gitea leg).
- **BUILD**: construct `ProjectReconciler` in `initReconcilers` (`app.go:530-597`); the reconciler needs no gitea-mcp route config (it makes no gateway calls — the operator helper owns the `mcp-gitea-<worker>` registrations).
- **BUILD**: add `&Project{}/&ProjectList{}` to the `scheme.AddKnownTypes` call inside `addKnownTypes` (`register.go:19-29`) and run `make generate` for `zz_generated.deepcopy.go`.
- **BUILD**: write `projects.hiclaw.io.yaml` to `config/crd/` and run `make generate` — it deepcopies AND syncs `config/crd/*.yaml` → `helm/hiclaw/crds/` (`Makefile:50-53`), preventing drift by construction; add the `status.repoCount` field that backs the Repos printer column (the `.length` jsonPath is invalid).
- **BUILD**: `project_provisioner.go` does **only** MinIO projection — no Gitea client, no gateway calls. Update the `gitea-operations` skill to read repo URLs + access from the Project manifest (not inline paths) per Phase 4 step 4.

---

### 10.2 Dashboard ↔ controller data contracts

> **Verifier verdict:** *solid*. The one refuted item (Option 2 — proxy reads `state.json` from MinIO)
> is corrected: `state.json` is documented as **never** synced to MinIO, so Option 1 (controller-side
> endpoint) is now the default and Option 2 is marked future-work-requiring-new-code. Anchor nits
> (`manage-state.sh:28`, bucket override `plan:204` / `config.go:287`) and the CORS "zero hits" wording
> are fixed below.

> All shapes below are read from **actual code**, not designed. `file:line` anchors inline.
> Three are existing (REST `:8090`, `state.json`, MinIO trees); **one endpoint is NET-NEW**
> (`GET /api/v1/manager-tasks`) and the **same-origin proxy is NET-NEW glue**.
> ⚠️ **Auth is ON at `:8090` even in embedded mode** (see proxy contract) — the SPA can't call it directly.

#### (1) Source table — what the v1 dashboard consumes

| # | Source | Endpoint / path | Shape | Anchor |
|---|---|---|---|---|
| A | Controller REST | `GET :8090/api/v1/managers` | `ManagerListResponse` | `internal/server/http.go:90`, struct `internal/server/types.go:239-260` |
| B | Controller REST | `GET :8090/api/v1/teams` | `TeamListResponse` | `http.go:77`, struct `types.go:154-177` |
| C | Controller REST | `GET :8090/api/v1/workers[?team=NAME]` | `WorkerListResponse` (aggregated: standalone CRs **+** synthesized team members) | `http.go:70`, list logic `resource_handler.go:181-222`, struct `types.go:61-86` |
| D | Manager `state.json` | **NET-NEW** `GET :8090/api/v1/manager-tasks` (exposes `~/state.json`) | active-task registry — see (2) | file at `manage-state.sh:18` (`${HOME}/state.json`); endpoint does not exist yet |
| E | MinIO task store | bucket `hiclaw-storage`, prefix `shared/tasks/{id}/` (`meta.json`, `spec.md`, `result.md`, `plan.md`, `progress/YYYY-MM-DD.md`, `base/`) | see (1.E) | layout `task-lifecycle.md:99-109` + `worker-agent/AGENTS.md:138-144`; `meta.json` `task-lifecycle.md:34-45`; `progress/` `task-progress/SKILL.md:18-20` |
| F | MinIO project store | prefix `shared/projects/{id}/` (`meta.json`, `plan.md`) | see (1.F) | `create-project.sh:57,64-74` |
| G | Gitea API | `git.pawcommit.com` (repo/PR/issue context per Project) — **v2, out of v1 scope** | upstream Gitea REST | plan §3 |

Bucket name `hiclaw-storage` is the install default — it is **env-driven**: `OSSBucket = envOrDefault(HICLAW_FS_BUCKET, "hiclaw-storage")` (`config.go:287`; also worker `FSBucket :354`, propagated `:707`). The local satellite sets `HICLAW_FS_BUCKET=hiclaw-storage-home` (plan line 204) — the proxy/dashboard must read `HICLAW_FS_BUCKET` per-instance, not hard-code it.

##### A. `GET /api/v1/managers` → `ManagerListResponse` (`types.go:257-260` wrapping `ManagerResponse` `:239-255`)
```jsonc
{
  "managers": [{
    "name":         "string",   // CR name
    "phase":        "string",   // Status.Phase; defaults "Pending" when empty (resource_handler.go:957-959)
    "state":        "string",   // desired lifecycle: Running|Sleeping|Stopped (Spec.DesiredState())
    "model":        "string",   // Spec.Model
    "runtime":      "string",   // copaw|openclaw|hermes…
    "image":        "string",
    "matrixUserID": "string",   // Status.MatrixUserID, e.g. @manager:matrix.pawcommit.com
    "roomID":       "string",   // Status.RoomID (admin DM)
    "version":      "string",   // Status.Version
    "message":      "string",   // Status.Message (human-readable last condition)
    "welcomeSent":  false       // bool, always present — first-boot onboarding done (types.go:250-254)
  }],
  "total": 0
}
```

##### B. `GET /api/v1/teams` → `TeamListResponse` (`types.go:174-177` wrapping `TeamResponse` `:154-172`)
```jsonc
{
  "teams": [{
    "name":              "string",
    "teamName":          "string",   // display name (EffectiveTeamName)
    "phase":             "string",   // default "Pending"
    "description":       "string",
    "admin":             {/* TeamAdminSpec, omitempty */},
    "humanMembers":      [/* TeamMemberSpec */],
    "leaderName":        "string",   // Spec.Leader.Name (CoPaw lead)
    "leaderHeartbeat":   {"enabled": true, "every": "30m"}, // omitempty
    "workerIdleTimeout": "string",   // e.g. "30m", omitempty
    "teamRoomID":        "string",   // Status.TeamRoomID
    "leaderDMRoomID":    "string",
    "leaderReady":       false,      // bool
    "readyWorkers":      0,          // int  (Status.ReadyWorkers)
    "totalWorkers":      0,          // int  (Status.TotalWorkers)
    "message":           "string",
    "workerNames":       ["string"], // Spec.Workers[].Name
    "workerExposedPorts":{"worker": [{"port": 0, "domain": "string"}]} // omitempty
  }],
  "total": 0
}
```
Builder: `teamToResponse` `resource_handler.go:888-924`. `readyWorkers`/`totalWorkers` are the **only progress-ish counters that exist today** — there is no `%`-complete.

##### C. `GET /api/v1/workers` → `WorkerListResponse` (`types.go:83-86` wrapping `WorkerResponse` `:61-76`)
```jsonc
{
  "workers": [{
    "name":             "string",
    "phase":            "string",   // Pending|Running|Stopped, default "Pending"
    "containerManaged": true,       // omitempty
    "state":            "string",   // desired: Running|Sleeping|Stopped
    "model":            "string",
    "runtime":          "string",   // copaw|hermes|openclaw — leader hardcoded "copaw" (resource_handler.go:1038)
    "image":            "string",
    "containerState":   "string",   // live backend status: running|ready|starting|stopped (resource_handler.go:1067)
    "matrixUserID":     "string",
    "roomID":           "string",
    "message":          "string",
    "exposedPorts":     [{"port": 0, "domain": "string"}],
    "team":             "string",   // "" for standalone; team name for members
    "role":             "string"    // "team_leader" | "worker" | "" (standalone)
  }],
  "total": 0
}
```
Aggregation contract (`resource_handler.go:181-222`): standalone Worker CRs **plus** a synthesized entry per Team leader + each Team worker (no child Worker CR exists for team members — `teamMemberToResponse` `:1022-1083`). For team members `phase`/`containerState` come from a **live backend `Status()` call per member** (`:1064-1078`) — so this endpoint does real Docker/k8s I/O; the SPA must tolerate latency and partial data (`containerState` is `""` if the backend can't be reached).

##### E. MinIO `shared/tasks/{id}/meta.json`  (`task-lifecycle.md:34-45`)
```jsonc
{
  "task_id":      "task-YYYYMMDD-HHMMSS",
  "project_id":   "proj-…",          // optional
  "task_title":   "string",
  "assigned_to":  "worker-name",
  "room_id":      "!room:domain",
  "status":       "assigned",        // assigned → completed (task-lifecycle.md:41,100). ⚠️ only 'assigned'/'completed' are confirmed meta.json status literals; revision/blocked are result.md Outcomes / workflow states, not confirmed status strings (see Open TODOs)
  "depends_on":   ["task-id"],       // DAG edges
  "assigned_at":  "ISO-8601",
  "completed_at": "ISO-8601",        // added on completion (task-lifecycle.md:100)
  "is_revision_for": "task-id",      // optional, revision tasks (task-lifecycle.md:89)
  "triggered_by":    "task-id"       // optional
}
```
Sibling files in the same prefix: `spec.md` (Manager-written), `plan.md` (worker), `result.md` (worker, finite only — its **Outcome** line is `SUCCESS|SUCCESS_WITH_NOTES|REVISION_NEEDED|BLOCKED`, parsed by Manager at `task-lifecycle.md:83`), `progress/YYYY-MM-DD.md` (append-only, latest-first by filename — `task-progress/SKILL.md:11-20`), `base/` (Manager-owned, read-only to workers).

##### F. MinIO `shared/projects/{id}/meta.json`  (`create-project.sh:64-74`)
```jsonc
{
  "project_id":      "proj-YYYYMMDD-HHMMSS",
  "title":           "string",
  "project_room_id": "!room:domain", // null until Matrix room created (create-project.sh:68,142)
  "status":          "planning",     // planning → active (create-project.md:61); also "completed"
  "workers":         ["worker-name"],
  "created_at":      "ISO-8601",
  "confirmed_at":    "ISO-8601"      // null until confirmed (create-project.sh:73)
}
```
Sibling: `plan.md` (the DAG / phase plan; `[ ]`/`[~]`/`[x]` checkboxes, parsed for the v2 DAG render). **Phase 4 note (CORRECTED v2.3 — the earlier "shape unchanged" claim was false):** once the `Project` CRD lands, the CRD's projection is `shared/projects/<id>/manifest.json` with the **§10.1 shape** (`id/team/description/repos/recordedWorkers/updatedAt`) — which shares almost nothing with this chat-flow `meta.json` (`project_room_id/status/workers/confirmed_at`, written by the lead's `projectflow` system, decision #16). The dashboard must **join both**: the CRD manifest for repos/access/provisioning state, and the `projectflow` `meta.json` + `plan.md` for execution status and the DAG render — linked by project id.

#### (2) NET-NEW: `GET /api/v1/manager-tasks` — expose `state.json`

**Status: does not exist.** `state.json` lives only on the Manager container's home dir (`~/state.json`, `manage-state.sh:18`), written by the Manager skill — the controller never reads it. ⚠️ **It is documented as `local only … never synced to MinIO`** (`manager/agent/AGENTS.md:3`, `copaw-manager-agent/AGENTS.md:3`), and no `mc cp`/`mirror` of `state.json` exists in the repo. **Default to Option 1.**

- **Option 1 (default — controller-side endpoint).** Add to `internal/server/` mirroring `resource_handler.go`, reading the Manager's **local/host-mounted** `state.json` (the controller mounts the Manager workspace; see §10.1/§0b mount chain). Register in `http.go` next to the manager routes (`http.go:88-93`), gated by the same `RequireAuthz(ActionGet, "manager", nil)`.
- **Option 2 (future work, NOT a current toss-up).** Have the same-origin proxy read `state.json` from MinIO **only if** new mirroring code is added to sync `~/state.json` into the bucket — which contradicts the current "never synced to MinIO" design. Treat as a deliberate future change, not a v1 alternative.

**Request:** `GET /api/v1/manager-tasks` — no params. (Optional `?manager=NAME` if multi-manager; today there is one.)

**Response** (exact `state.json` shape, from `manage-state.sh`):
```jsonc
{
  "admin_dm_room_id": "!room:domain",  // or null (manage-state.sh:28,175)
  "active_tasks": [
    { // finite task (manage-state.sh:72-79)
      "task_id":        "string",
      "title":          "string",
      "type":           "finite",
      "assigned_to":    "worker-name",
      "room_id":        "!room:domain",
      "project_room_id":"!room:domain",  // present only if set (manage-state.sh:78)
      "delegated_to_team":"team-name"    // present only if set (manage-state.sh:79)
    },
    { // infinite task (manage-state.sh:107-117)
      "task_id":          "string",
      "title":            "string",
      "type":             "infinite",
      "assigned_to":      "worker-name",
      "room_id":          "!room:domain",
      "schedule":         "CRON",
      "timezone":         "string",
      "last_executed_at": "ISO-8601",    // null until first run (manage-state.sh:115)
      "next_scheduled_at":"ISO-8601"
    }
  ],
  "updated_at": "ISO-8601"               // _ts() = UTC %Y-%m-%dT%H:%M:%SZ (manage-state.sh:20-22)
}
```
This is the **task board's primary feed** (it's the registry the Manager heartbeat itself reads instead of scanning `meta.json` — `state-management.md:5`). Join `active_tasks[].task_id` → MinIO `shared/tasks/{id}/` (E) for detail. **Caveat:** `state.json` holds only *active* tasks — completed tasks are removed (`manage-state.sh:138`); for history the dashboard must list MinIO `shared/tasks/` and read each `meta.json`.

#### (3) Same-origin backend-proxy contract (NET-NEW)

**Rationale (verified):** `:8090` does **no CORS/OPTIONS handling** — there is no `Access-Control-*` header set and no `OPTIONS` handler or CORS middleware in the mux (`http.go:44-130`). (A bare `grep Options` substring-matches Go's `metav1.*Options` types — that's noise, not CORS.) A browser SPA on `hq.pawcommit.com` therefore cannot `fetch()` `:8090` cross-origin. **Second reason, stronger:** `:8090` **requires a Bearer SA token** in *both* modes — embedded mode runs an embedded apiserver (`app.go:367,636`) → `restCfg != nil` → TokenReview auth enabled (`app.go:432-443`); missing/invalid token → `401 "invalid or missing bearer token"` (`internal/auth/middleware.go:62,137-141` — note the package: `auth`, not `server`; `extractBearerToken` `:158-168`). The browser must never hold that token. → the proxy is mandatory, not just a CORS workaround.

**Shape:** a thin server (nginx or a small Go/Node service) co-located with the SPA behind Traefik at `hq.pawcommit.com`, same origin as the static assets. It holds **one credential**: a controller admin SA token (the bootstrapped CLI token, `app.go:211-220`).

| SPA path (same-origin) | Proxies to | Injects | Notes |
|---|---|---|---|
| `/api/managers` | `:8090/api/v1/managers` | `Authorization: Bearer <admin-SA-token>` | strip the token before responding |
| `/api/teams` | `:8090/api/v1/teams` | same | |
| `/api/workers` | `:8090/api/v1/workers[?team=]` | same | passthrough query string |
| `/api/manager-tasks` | `:8090/api/v1/manager-tasks` (2) | same | NET-NEW; controller-side (Option 1) |
| `/api/tasks`, `/api/tasks/{id}/*` | MinIO `s3://$HICLAW_FS_BUCKET/shared/tasks/...` | MinIO creds | list + read `meta.json`/`result.md`/`progress/*` |
| `/api/projects`, `/api/projects/{id}/*` | MinIO `s3://$HICLAW_FS_BUCKET/shared/projects/...` | MinIO creds | |
| `/api/files/*` | MinIO browse/download | MinIO creds | the file browser (Phase 6 overlap) |

**Auth boundary:** the **only** external auth is Traefik forward/basic-auth on `hq.pawcommit.com` (✓ decided #1/#3, Phase 5 build step 3) — same pattern as `git-mcp.pawcommit.com`. No app-level auth code. The proxy is read-only for v1 (GET only); reject all other methods — **v1.1+ grows a scoped write allowlist** (wake/sleep, then message-injection) per decision #17 / Phase 5 step 4.

**Cross-instance (Phase 0b Option A):** the dashboard aggregates **both** controllers' `:8090` (§0b Option A). The proxy fans out to `vps-controller:8090` and `home-controller:8090` (over the tailnet), tagging each response with an `instance` field client-side. **TODO (live-VPS):** confirm the home controller's `:8090` is reachable from the proxy host (tailnet/`host.docker.internal`), and that the home MinIO bucket is `hiclaw-storage-home`.

#### (4) SPA polling model

No websockets/SSE exist server-side — the v1 SPA **polls** (Phase 5 build step 1, "Poll every ~15s").

| Feed | Source via proxy | Interval | Notes |
|---|---|---|---|
| Manager/Team/Worker cards | `/api/managers`, `/api/teams`, `/api/workers` | **15s** | `/api/workers` is the heaviest (live per-member backend `Status()` calls, `resource_handler.go:1064-1078`) — consider 30s for it, or only when the Workers view is open |
| Task board | `/api/manager-tasks` | **15s** | small JSON; cheap |
| Open task detail panel | `/api/tasks/{id}/meta.json` + `result.md` + latest `progress/*` | **on open + 15s while open** | latest progress = highest `YYYY-MM-DD.md` (`task-progress/SKILL.md:12`) |
| Project list | `/api/projects` | **30–60s** | changes rarely |

**ETag/caching: none today.** No handler sets `ETag`/`Last-Modified`/`Cache-Control` (the writers are `httputil.WriteJSON`, no cache headers; MinIO objects do carry native `ETag`/`Last-Modified`). Recommendation: the **proxy** adds conditional-GET support for the MinIO routes (pass through MinIO `ETag`, honor `If-None-Match` → `304`) so polling task/progress files is cheap; the controller REST feeds are small enough to fetch fresh each poll. Client-side: diff by `updated_at` (`manager-tasks`) and by `phase`/`containerState`/`message` (cards) to avoid re-render churn.

**Known gaps to design around (unchanged from Phase 5's known-gaps note):** no `%`-complete, no per-subtask status, no live stream. v1 infers progress from `phase` + `meta.json` timestamps + `result.md` Outcome + counting `[x]`/`[~]`/`[ ]` in `plan.md`. Add `shared/events/*.jsonl` only if the inferred view proves insufficient.

#### Open TODOs

- **DECISION**: implement `GET /api/v1/manager-tasks` controller-side (Option 1, reads the local/host-mounted `state.json`) — this is now the default; Option 2 (proxy-reads-MinIO) requires new mirroring code and is future work.
- **LIVE-VPS CONFIRM**: the bootstrapped admin SA token location/lifetime the proxy will use (`app.go:211-220`) and whether it auto-renews across controller restarts.
- **LIVE-VPS CONFIRM (Phase 0b)**: home controller `:8090` reachable from the proxy host (tailnet / `host.docker.internal`) and home MinIO bucket = `hiclaw-storage-home`, for cross-instance fan-out.
- **DECISION**: per-feed poll intervals — confirm 15s for cards/board is acceptable given `/api/v1/workers` does live backend `Status()` calls; consider 30s for the workers feed.
- **CONFIRM** the full `meta.json` `status` enum for tasks beyond `assigned`/`completed` (revision/blocked are referenced in `task-lifecycle.md:83-94` but are **result.md Outcomes / workflow states, not confirmed `meta.json` status literals**).

---

### 10.3 Spike exit-criteria

> **Verifier verdict:** *solid*. The track was relabelled from `spike-definitions` to `spike-criteria`;
> the misleading `bridge.py:249-250` comment is flagged in S3; and minor line-cite tightenings
> (S1 `283-298`, S2 `300,307,352,367`, S5 `docs/faq.md:548-563`, S7 `worker_env.go:146-148`, S4 log
> span) are folded in.

> Every ⚠️/"verify"/"validate on a live worker" item in the plan, converted to a runnable spike. Run all on the VPS Claude Code box (or the throwaway interop team it stands up) unless a row says "local box". A spike is **DONE** only when its PASS or FAIL branch is recorded in the decisions log. "throwaway team" = the 1 CoPaw-lead + 1 Hermes-worker team from the §7 Hermes-interop spike.

| ID | Uncertainty (plan §, file:line) | Exact experiment — commands / where / what to observe | PASS criterion | FAIL → fallback | Gates | Priority |
|---|---|---|---|---|---|---|
| **S1** | **CoPaw two-channel-impl: which registers at runtime** (§0b/§5b; overlay `copaw/src/matrix/channel.py` vs standalone `copaw/src/copaw_worker/matrix_channel.py`, both `CHANNEL_KEY="matrix"`). | On a running CoPaw **worker**: `docker exec <copaw-worker> sh -c 'ls -la $COPAW_WORKING_DIR/custom_channels/ && cat $COPAW_WORKING_DIR/custom_channels/matrix_channel.py \| head -5'`. Confirm against `worker.py:570-583` (`_install_matrix_channel` copies `copaw_worker/matrix_channel.py` → `custom_channels/`) and `worker.py:283-298` (`clear_builtin_channel_cache()` at :286, then `ChannelManager.from_config` at :293-297). For the **manager**, grep its image/agent dir for which channel module loads. | The runtime channel is the **standalone `copaw_worker/matrix_channel.py`** (workers); manager path identified. The overlay `matrix/channel.py` is confirmed a *separate* code path, not the worker runtime one. | If a CoPaw worker actually loads `matrix/channel.py` (overlay), all §5b CoPaw fixes (S2) must target that file instead — re-anchor before editing. | 5b, 3 (interop) | **✓ STATICALLY RESOLVED 2026-07-02 (#15)** — manager = overlay (baked by `Dockerfile.copaw:86-96`), leads/workers = standalone (`worker.py:570-583`). Phase 1/5b re-anchored to both files; this row is now a quick live-confirm, no longer P0-blocking. |
| **S2** | **CoPaw `show_tool_details=false` efficacy** (§5b step 1; plan claims field lives in external `BaseChannel`). **Correction found:** `show_tool_details` is a **top-level `Config` field** (`copaw/src/matrix/config.py:1164`), NOT in the `channels.matrix` dict; both fork channels only accept it as a default `=True` param and **never set it** (`matrix_channel.py:193,200,274,289`; `channel.py:300,307,352,367`). The open question is whether external `copaw.app.channels.ChannelManager.from_config` threads root `Config.show_tool_details` into the channel's `from_config(..., show_tool_details=...)`. **(Grouped under the §7 Hermes-interop spike.)** | In the throwaway team, run a multi-tool task against the CoPaw lead and **count messages/turn** in the room (Element or `mc cat` the room timeline). Then set `show_tool_details: false` **at the config.json root** (not under `channels.matrix`) via the bridge — `docker exec` patch `$COPAW_WORKING_DIR/config.json` `{"show_tool_details": false, ...}`, restart, rerun the same task, re-count. Also `grep -rn show_tool_details` inside the installed `copaw` package (`python -c "import copaw, os; print(os.path.dirname(copaw.__file__))"`) to see if `ChannelManager`/`BaseChannel` reads `cfg.show_tool_details` and forwards it. | Per-tool-call/reasoning messages drop (e.g. 1 final `m.text` reply instead of N) with the **root-level** flag set, while typing indicators + final reply remain. | (a) If only the channel-dict flag is read, set it there instead and re-test. (b) If neither silences the stream (external `BaseChannel` ignores it), fall back to **post-send suppression** in the standalone `matrix_channel.py` (filter outbound events by type before `room_send`), reusing `filter_tool_messages`/`filter_thinking` (already read at `matrix_channel.py:166-167,290-291`) — verify *those* are actually honored by `BaseChannel` first. | 5b | **P1** |
| **S3** | **Hermes streaming shape + adapter chokepoint** (§5b step 2 / "⚠️ Confirm in spike"; `overlay_adapter.py:109-132` wraps `send_message_event`). Two sub-questions: (a) new events vs `m.replace` edits; (b) does the native adapter route streaming steps through `send_message_event` (the wrapped chokepoint) or a lower-level method. ⚠️ **Do NOT trust the `bridge.py:249-250` comment** claiming `MATRIX_FILTER_*` are "consumed by `hermes_matrix.adapter` directly" — that comment is stale/false; the vars are written but **read nowhere** in `hermes/` (`bridge.py:252-253`), so suppression must be added explicitly in the overlay. Version-dependent: pin **v0.10.0** → target **v0.17.0**. **(Grouped under the §7 Hermes-interop spike.)** | In the throwaway team, run a multi-tool task against the Hermes worker. Capture the raw room timeline (Element devtools, or `mc`/Matrix CS `/messages` API). Inspect each event for `content["m.relates_to"]["rel_type"] == "m.replace"`. Add a temporary `logger.warning` at the `wrapped` send in `overlay_adapter.py:118-128` printing `event_type`+`msgtype`+whether `m.replace`; rebuild Hermes image, rerun, read worker logs to confirm **every** outbound step passes through that wrapper. | You can state, with evidence: (a) steps stream as **new events** OR as **`m.replace` edits of one message**; and (b) **all** outbound events traverse the `send_message_event` wrapper (the log fires for each). | If streaming **bypasses** `send_message_event` (no log lines for steps), find the lower-level send the native `_matrix_native.MatrixAdapter` uses and override *that* instead in the overlay. If steps are `m.replace` edits, suppression policy must keep only the final edit, not drop all. | 5b, 3 | **P1** |
| **S4** | **Hermes FileSync pruning parity** (§4.5 / decision #10; gates worker self-install — plan asks "⚠️ Verify Hermes FileSync has no equivalent pruning"). **Answer found in-repo (no live spike needed to confirm existence):** Hermes prunes in **TWO** places — periodic sync `hermes/src/hermes_worker/sync.py:448-452` (`minio_skill_set` / `rmtree`, identical to CoPaw `sync.py:709-716`) **and** at startup-install `hermes/src/hermes_worker/worker.py:368-375` (`keep = installed ∪ {file-sync}`, `rmtree` the rest). | Static: already verified — both sites prune local skills absent from MinIO/the manager pool. Live confirm: `find-skills install <x>` inside a Hermes worker, wait one ~5-min reconcile, `docker exec <hermes-worker> ls skills/` — observe `<x>` removed and a log line `"Removed local skill no longer in MinIO"` (`sync.py:453-455`, multi-line) or `"Removed stale hermes skill"` (`worker.py:373`). | Live behavior matches static reading: a self-installed skill is pruned on the next sync (and at next startup) **before** the fix. After patching **both** loops (skip dirs not in MinIO instead of `rmtree`; mirror the CoPaw `sync.py:709-716` patch), `<x>` **survives** a reconcile and a worker restart. | If a third prune path exists (e.g. the `_install_skills` copy at `worker.py:350-361` overwriting), patch it too. Keep the builtin set as the floor either way. | 4.5 | **P1** (decided #10 can't ship correctly without both sites patched) |
| **S5** | **Higress route ordering — model-prefix route vs path-only `default-ai-route`** (§2b step 1; `default-ai-route` uses `pathPredicate matchType=PRE matchValue="/"`, `setup-higress.sh:248`, which prefix-matches every path). Risk: a new `^ollama/.*$` / `^mimo` model-name route is shadowed by the catch-all "/". | On the VPS Higress: add `mimo` + `ollama` providers + their own AI routes with model-name match rules (per `docs/faq.md:548-563`, service name no `/`), route name **≠ `default-ai-route`**. Then from a worker's gateway key, fire 3 chat-completions with `model` = a DeepSeek id, `ollama/<model>`, and `mimo/<model>`. Inspect Higress access logs / response `x-higress-*` headers (or upstream-served model) to see **which provider** served each. Confirm `AuthorizeAIRoutes` empty-`providerFilter` semantics at `higress.go:181-194` and that `default-ai-route` rewrite each boot (`setup-higress.sh:253-281`) doesn't clobber the new routes. | Each request lands on its **model-matched** provider (DeepSeek→openai-compat, `ollama/*`→ollama, `mimo*`→mimo); the path-only `default-ai-route` does **not** shadow the prefix routes. | If "/" shadows: model-name routing in Higress AI is route-config ordering/priority-sensitive — set explicit route priority, or constrain `default-ai-route`'s match so it's the genuine fallback (lowest priority / most-specific-wins). Document the required priority field in §2b step 1. | 2b | **P1** |
| **S6** | **Xiaomi MiMo hosted base URL + current model IDs** (§2b "Provider compatibility"; "⚠️ confirm the exact hosted base URL at provision time", models **MiMo-V2.5 / V2.5-Pro**, route by `^mimo`). | Web: confirm the live OpenAI-compatible base URL (candidates: `platform.xiaomimimo.com`, WaveSpeedAI, OpenRouter, or self-host vLLM) and the **exact current model IDs**. Then live probe: `curl -s <BASE>/v1/chat/completions -H "Authorization: Bearer $MIMO_KEY" -H 'Content-Type: application/json' -d '{"model":"<MIMO_MODEL_ID>","messages":[{"role":"user","content":"ping"}]}'`. | A 200 with a valid completion from a known model ID over an OpenAI-compatible `/v1/chat/completions`; base URL + model IDs recorded for `setup-higress.sh`/catalog (`generator.go:451-494`, `known-models.json`, `manager-openclaw.json.tmpl`, `update-manager-model.sh`). | If no stable hosted OpenAI-compatible endpoint exists for the operator's account, route MiMo via OpenRouter (`openrouter/...` prefix) or self-host vLLM and point `openaiCustomUrl` at it. **TODO (human/live):** which MiMo hosting the operator actually has access to. | 2b | **P2** |
| **S7** | **market.hiclaw.io reachability** (§4.5 step 2; default `find-skills` registry = `nacos://market.hiclaw.io:80/public`, `install/hiclaw-install.sh:3059` → `config.go:371` → `worker_env.go:146-148` sets `SKILLS_API_URL`). | From the VPS **and** the local box: `curl -sv -m 10 http://market.hiclaw.io:80/ ; echo exit=$?` and a real `@nacos-group/cli skill-get` / `find-skills` list against `nacos://market.hiclaw.io:80/public` from inside a worker container. | Both boxes resolve + connect and a skill list/get returns from the nacos registry. | If unreachable: self-host Nacos (mirror the public skill set) or switch to a `skills.sh`-style HTTP registry; set `HICLAW_SKILLS_API_URL` to it. Decide before pre-fetching market skills at build time (§4.5 step 2). | 4.5 | **P2** |
| **S8** | **manager-workspace mount-target "mismatch"** (§0b Caveats; controller binds host ws → `/root/hiclaw-fs/agents/manager` `ps1:3168`, spawned manager binds → `/root/manager-workspace` `manager_reconcile_container.go:209-213`). **Static finding:** likely **benign** — both derive from the same host `HICLAW_WORKSPACE_DIR` (`config.go:306` → `EmbeddedConfig.WorkspaceDir` `app.go:587` → HostPath of the spawned-manager mount). `/root/hiclaw-fs/agents/manager` is only the *controller's own* view; the spawned manager (host docker.sock) mounts the host path at `/root/manager-workspace` and `HOME=/root/manager-workspace` (`worker_env.go:61`). | Local box, embedded bring-up: after the manager spawns, `docker inspect <manager-container> --format '{{json .Mounts}}'` and confirm its `/root/manager-workspace` HostPath == the host `HICLAW_WORKSPACE_DIR`. `docker exec <manager> sh -c 'ls -la /root/manager-workspace && cat /root/manager-workspace/AGENTS.md \| head -3'` to confirm persona/state are present. | Spawned manager's `/root/manager-workspace` is backed by the real host workspace dir and contains its persona/`state.json`/registry — i.e. no actual mismatch. | If the manager comes up with an empty workspace, the two paths are genuinely crossed: align `EmbeddedConfig.WorkspaceDir` (host path) with whatever the spawned-manager HostPath should be, or fix the ps1 controller-bind target. | 0b | **P2** |
| **S-GIT** | **Per-worker Gitea identity (source-verified) + own-PAT native checkout** (§2 step 4 / §4 decision #4 / §10.1 #4 / decisions #12,#13). The per-request `Authorization: Bearer <PAT>` override of gitea-mcp's env token is **source-verified** (gitea-mcp cloned, `operation.go:72-84` → `pkg/gitea/gitea.go:76-81` → `rest.go`; header > flag > env) — no longer the uncertainty. The **live** unknown is whether **Higress per-server `defaultCredential`** attaches that Bearer to the upstream gitea-mcp on the VPS Higress version. Sub-test: does a worker's **own scoped PAT** via a git credential helper work for clone+push (gitea-mcp is API-only, no working tree)? | Register **two** `mcp-gitea-<worker>` servers with **two different PATs** (two Gitea users) via `setup-mcp-proxy.sh`; from each worker's consumer, open a PR. Inspect the PR author in Gitea. **Sub-test:** from a Hermes sandbox, configure the **worker's own PAT** via a git credential helper (`store`/`cache` or `https://<user>:<token>@git.pawcommit.com`), `git clone` a test repo, edit, `git push`. **Also (v2.3): enumerate the live tool list/names/schemas exposed through `mcp-gitea-<worker>`** — the input to authoring the `gitea-operations` SKILL (Phase 2 step 3); don't assume GitHub parity. | PRs are attributed to **two different Gitea users** (Higress per-server `defaultCredential` → upstream Bearer overrides gitea-mcp's env token); the own-PAT clone+edit+push succeeds; **AND isolation holds (✓ #14):** worker A's consumer key gets 401/403 on worker B's `mcp-gitea-<worker>` after the helper has provisioned both (i.e. Step-5's all-workers broadcast was successfully avoided). | Higress **per-consumer request-transform** to inject the Bearer, or **per-worker gitea-mcp instances** (one process per PAT). ⚠️ The **Sudo-impersonation** variant is **non-viable** — gitea-mcp strips Sudo headers (zero source matches). | 2, 4 | **P1** |
| **S9** | **Hermes inbound media path** (Phase 6 step 1, added v2.2; `overlay_adapter.py:179-239` `_handle_media_message` only intercepts `msgtype == "m.image"` — generic `m.file`/`m.audio`/`m.video` pass through to the **un-vendored** native adapter (`_matrix_native.py`, created only at Docker build), so there is no in-repo hook for the drag-drop-a-file case). | In the throwaway team, send an `m.file` attachment to the Hermes worker's room. Read worker logs + `docker exec` the container FS to see where (if anywhere) the native adapter downloads it. Then locate the native-adapter method that handles media (mirror the S3 chokepoint hunt) and prototype an overlay override that diverts the bytes / mxc URL into MinIO `shared/tasks/<id>/`. | A concrete overridable native-adapter method is identified and a prototype diverts one attached file into the MinIO task dir. | Route attachments through the **CoPaw lead** instead — its standalone channel's `_on_room_media_event` (`copaw_worker/matrix_channel.py:395-396` registration, `:1210` handler; generic for Image/File/Audio/Video) pushes to MinIO, and Hermes workers read from there. | 6 | **P2** |
| **S10** | **Hermes Matrix sync-token persistence across recreate** (v2.3; the native mautrix adapter `_matrix_native.py` is created at Docker build time, not vendored — where does it persist its `next_batch`/sync token, and does that path fall inside `hermes_worker/sync.py`'s MinIO mirror scope?). | Locate the token path inside a running Hermes worker (`docker exec`, grep for next_batch / sync-token files under the agent home); check whether the sync mirror covers it. Then destroy+recreate the worker container while a message is sent to its room; observe whether the message is processed after restart or silently skipped. | Token survives recreate (or is safely re-derived) and the in-between message reaches the agent. | Add the token path to the MinIO mirror set (persist under `agents/<name>/`); if the adapter can't point at a persisted path, document that recreate = missed-message window and add a Hermes catch-up equivalent to Phase 1 step 4. | 3, 0b | **P1** |
| **S-BACKUP** | **Snapshot restore path is unvalidated** (v2.3; `/root/backups/hiclaw-snapshot-*` exist, but no restore script or doc exists in-repo, and Tuwunel+MinIO+kine+Higress state co-mingle in one `hiclaw-data` volume). | On the VPS: inspect one snapshot's contents/format; hand-restore into a THROWAWAY embedded instance (fresh container, restored volume); verify Manager identity, rooms, MinIO task/project data, and Higress config (incl. worker PATs, #14) survive. Time it; write it down as `docs/restore.md`. | A documented, tested restore procedure exists; the snapshot demonstrably contains (or explicitly excludes) the Higress PAT config. | If snapshots are partial/unrestorable: fix the snapshot script scope (whole `hiclaw-data` volume + env file) **before** Phase 0 step 4's selective data import relies on them. | 0 | **P1** |

#### Open TODOs

- **S-GIT (live worker)**: confirm two `mcp-gitea-<worker>` servers with two different PATs yield PRs attributed to two different Gitea users (Higress per-server `defaultCredential` → upstream Bearer overrides gitea-mcp's env token) on the VPS Higress version; and that a Hermes sandbox can clone+push against `git.pawcommit.com` using the **worker's own PAT** via a git credential helper. If the per-server credential doesn't attach, fall back to per-consumer request-transform or per-worker gitea-mcp instances (Sudo impersonation is non-viable — gitea-mcp strips Sudo headers).
- **S6 (human/live)**: confirm which MiMo hosting the operator's account actually has — `platform.xiaomimimo.com` vs OpenRouter vs WaveSpeedAI vs self-host vLLM — plus exact current model IDs (MiMo-V2.5 / V2.5-Pro candidates).
- **S7 (live, both boxes)**: confirm `market.hiclaw.io:80` reachability from VPS and local before committing to nacos-based market-skill pre-fetch.
- **S2/S3 (live worker)**: both require the throwaway CoPaw-lead+Hermes-worker team standing up first; they cannot be pre-decided from this repo because the streaming lives in external packages.
- **S9 (live worker)**: same throwaway team — the Hermes inbound-media hook hunt; if no clean override exists, fall back to lead-mediated attachment ingest (see row).
- **Decision-log update**: record PASS/FAIL of each spike in §8 once run; S4's two-site finding should be folded into decision #10's implementation note.

---

### 10.4 Worker → team migration mapping

> **Verifier verdict:** *mostly-solid*. The one refuted item — the line numbers for the infra/container
> reconcile split — is corrected below: `ReconcileMemberInfra` is `member_reconcile.go:152-197` (there
> is **no** `ensureMemberInfra`); the `:268-293` container citation was already right. The
> `create-team.md` cite is fixed to `:21,62`, and the registry-stub shape is quoted in full.

> Turns Phase 3 step 3 ("map godot-* / game-* / narrative-writer / engineer-backend-architect / … into the right teams as Hermes workers") into a fill-in-the-blanks procedure. The live persona list is in **`workers-registry.json` on the VPS** (the in-repo copy `manager/agent/workers-registry.json` is an empty stub — `{"version":1,"updated_at":"","workers":{}}`); rows for unknown personas are marked **TEMPLATE — complete from the VPS registry**.

#### (1) Functional-team taxonomy

Four functional Teams (the plan's set; the code implies no others — there is no built-in team taxonomy, a Team is just a CR). For **every** Team: Leader runtime is **hard-coded `copaw`** by the controller (`team_controller.go:972`, `leaderWorkerSpec` always sets `Runtime: "copaw"` regardless of spec) — exactly the desired CoPaw-lead model; member-worker runtime is **`hermes`** (decision #11), set per-member as `runtime: hermes` in the Team `workers[]` (passed through verbatim by `team_controller.go:1016`).

| Team (`metadata.name`) | Leader runtime | Member runtime | One-line purpose |
|---|---|---|---|
| `engineering` | `copaw` (forced) | `hermes` | Backend/infra/app code: architecture, services, APIs, CI on `git.pawcommit.com`. |
| `design` | `copaw` (forced) | `hermes` | UX/UI, visual assets, design systems, asset pipelines. |
| `marketing-social` | `copaw` (forced) | `hermes` | Copy, social content, campaign assets, scheduling/publishing. |
| `gamedev` | `copaw` (forced) | `hermes` | Game implementation + narrative: Godot projects, gameplay, level/asset work, story/dialogue. |

Per Phase 2b decision #7, set `spec.modelProvider` on the leader **and** every member (no team-wide field exists — `teams.hiclaw.io.yaml:85-87` leader, `:262-264` worker only). Suggested pins from the plan: `engineering` → `deepseek`; `gamedev` → `ollama`; `design`/`marketing-social` → **TODO (operator decision)**.

#### (2) Migration table (existing persona → target team)

Columns: persona (current standalone CoPaw worker) → target Team → role → runtime → skills to attach → notes. **All migrated workers run `hermes`** (decision #11); leaders stay `copaw`.

| Persona (current) | Target team | Role | Runtime | Skills to attach (on-demand, beyond image built-ins) | Notes |
|---|---|---|---|---|---|
| `engineer-backend-architect` | `engineering` | worker | `hermes` | `gitea-operations` (Phase 2; mirrors `github-operations`); Hermes git via its **own** `mcp-gitea-<worker>` (own Gitea user/PAT); native checkout via its **own PAT** helper if a working tree is needed (S-GIT) | Backend lead-IC. Reaches Gitea as its **own Gitea user** via `mcp-gitea-<worker>`; for a native `git clone/push` against `git.pawcommit.com` it reuses its **own scoped PAT** helper — **not** a shared token, **not** the CoPaw `git-delegation` flow. |
| `godot-*` (e.g. `godot-engineer`, `godot-tools`) | `gamedev` | worker | `hermes` | `gitea-operations`; native checkout via its **own PAT** helper if a working tree is needed (S-GIT) | One Team member per `godot-*` persona. **TEMPLATE — enumerate exact `godot-*` names from the VPS registry.** |
| `game-*` (e.g. `game-designer`, `game-systems`) | `gamedev` | worker | `hermes` | `gitea-operations`; native checkout via its **own PAT** helper if a working tree is needed (S-GIT); (design-leaning ones may add `file-sharing` analogue) | **TEMPLATE — enumerate exact `game-*` names from the VPS registry.** Split design-vs-code intent if the registry distinguishes. |
| `narrative-writer` | `gamedev` | worker | `hermes` | `gitea-operations` (commit story files); native checkout via its **own PAT** helper if a working tree is needed (S-GIT) | Narrative/dialogue. Could alternatively seed `marketing-social` if it writes campaign copy — **TODO confirm intent from persona soul/agents.** |
| `<persona-5>` | `engineering` \| `design` \| `marketing-social` \| `gamedev` | worker | `hermes` | `gitea-operations` + any persona-specific on-demand skill | **TEMPLATE — complete from `workers-registry.json` on the VPS.** |
| … `<persona-6 … persona-14>` | … | worker | `hermes` | … | **TEMPLATE rows — the plan states 14 existing CoPaw workers; only 4 personas are named. Fill the remaining ~10 from the VPS registry: `cat ~/workers-registry.json` (manager workspace) lists each worker + its already-assigned on-demand skills.** |

Built-ins are runtime-fixed and need **no** row: a `hermes` worker auto-gets `file-sync`, `task-progress`, `project-participation`, `mcporter`, `find-skills` from its image (`manager/agent/hermes-worker-agent/skills/`; README at `worker-skills/README.md:55-67`). The "skills to attach" column is **only on-demand** skills pushed via `push-worker-skills.sh` (`manager/agent/skills/worker-management/scripts/push-worker-skills.sh`). ⚠️ The two on-demand worker-skills that exist today (`git-delegation`, `github-operations`) are **CoPaw-oriented** — `git-delegation` exists because "Workers don't have git credentials" (`worker-skills/git-delegation/SKILL.md:8-10`). Hermes workers reach Gitea as their **own Gitea user** via a per-worker `mcp-gitea-<worker>` (own scoped PAT held by Higress, Phase 2), with an **own-PAT** credential helper only for native checkouts (S-GIT), so attach `gitea-operations` (Phase 2 deliverable, not yet in repo) and skip `git-delegation`. **TODO:** confirm during the §7 spike that Hermes consumes `gitea-operations` SKILL.md the same way CoPaw consumes `github-operations`.

#### (3) Per-worker migration steps

For each persona row above:

1. **Add the member to its Team CR** (create the Team first if it doesn't exist). Use **YAML + `hiclaw apply -f`**, *not* the `hiclaw create team` CLI — the CLI forces `runtime: copaw` on all members (`create-team.md:21`) and assigns `copaw-worker-agent` skills (`create-team.md:62`); only YAML lets you set `runtime: hermes` per member:
   ```yaml
   apiVersion: hiclaw.io/v1beta1
   kind: Team
   metadata:
     name: gamedev
   spec:
     leader:
       name: gamedev-lead          # runtime forced to copaw by controller
       modelProvider: ollama        # decision #7 — pin every agent
     workers:
       - name: narrative-writer     # ← migrated persona
         runtime: hermes            # decision #11; passed through (team_controller.go:1016)
         modelProvider: ollama      # decision #7
         identity: "…"              # carry over from the old standalone worker's persona
         soul: "…"                  # carry over SOUL/role text
         agents: "…"                # carry over AGENTS rules
   ```
   Carry `identity`/`soul`/`agents` from the old standalone Worker CR (these generate IDENTITY.md/SOUL.md/AGENTS.md — `workers.hiclaw.io.yaml:38-46`). `runtime`, `modelProvider`, `model`, `image`, `skills` are all per-member fields on `Team.spec.workers[]` (`teams.hiclaw.io.yaml:255-282`).
2. **Map persona → skills.** Built-ins arrive via the `hermes` image automatically. Attach on-demand skills after the worker exists:
   ```bash
   bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh \
     --worker narrative-writer --add-skill gitea-operations
   ```
   (Skills can also be declared in `workers[].skills` for built-in HiClaw skills, or `workers[].remoteSkills` for `nacos://` registry skills — `teams.hiclaw.io.yaml:276-311`.)
3. **`hiclaw apply -f team.yaml`.** The Team reconciler provisions the Matrix Team Room + Leader DM, creates the member Worker CR, injects coordination context into the Leader's AGENTS.md, and sets up shared team MinIO storage (`create-team.md:58-66`).
4. **Validate interop (Phase 3 step 4 / §7 spike / §10.3 S2-S3).** Confirm the CoPaw lead ↔ Hermes worker round-trip over Matrix (`@mention` → task → result) and that the Hermes worker reads the `gitea-operations` skill. Run this on **one** throwaway team before migrating all 14.

#### ⚠️ Destructive runtime-switch caveat

Flipping an existing standalone CoPaw worker to `runtime: hermes` (or any spec change the controller deems a spec change) **deletes and recreates the container** — it is not an in-place upgrade. In `ensureMemberContainerPresent`, when `specChanged` is true the controller calls `wb.Delete(ctx, m.Name)` then `createMemberContainer(...)` (`member_reconcile.go:316-329`; same delete+recreate on Stopped/unexpected states at `:331-353`). **Anything inside the old container's local FS is lost** — but Hermes/CoPaw workers are stateless (no host volumes; FS is MinIO inside the controller, per plan §0b).

**Preserved across the switch** (owned by the *infra* reconcile, keyed by the stable member `name`, run **before** container reconcile — `ReconcileMemberInfra` at `member_reconcile.go:152-197` provisions Matrix/Gateway/MinIO into `state.ProvResult`, distinct from and run before `ReconcileMemberContainer` at `:268-293`; note `member_reconcile.go:233-265` is `ReconcileMemberConfig`, the config/skill push, **not** infra):
- **Matrix** user + Team Room + DM membership (`state.ProvResult.MatrixToken/MatrixPassword`),
- **Higress** gateway consumer/key (`state.ProvResult.GatewayKey`),
- **MinIO** prefix `agents/<name>/` + `shared/` + `teams/<team>/` (the worker's persistent FS).

(`WorkerProvisionResult` fields `MatrixToken`/`MatrixPassword`/`GatewayKey`/`MinIOPassword` are at `provisioner.go:32-39`.) So: migration interrupts the running container but keeps the worker's identity, room history, gateway authorization, and MinIO files. Do it when the worker is idle, or accept a brief interruption (mirrors the same caveat for resource changes — `create-team.md:54`).

#### Open TODOs

- **LIVE-VPS**: `cat ~/workers-registry.json` on the VPS to enumerate all 14 personas and their currently-assigned on-demand skills; fill the TEMPLATE rows.
- **DECISION**: assign each of the ~10 unnamed personas to engineering/design/marketing-social/gamedev (read each persona's soul/agents on the VPS to decide).
- **DECISION**: `modelProvider` pin for design and marketing-social teams (plan only specifies engineering→deepseek, gamedev→ollama).
- **DECISION**: does `narrative-writer` belong in gamedev or marketing-social? (depends on whether it writes game story vs campaign copy — confirm from persona text).
- **SPIKE (§7 / §10.3 S2-S3)**: confirm a Hermes worker reads/uses the `gitea-operations` SKILL.md, and that the CoPaw-lead ↔ Hermes-worker mention→task→result round-trip works before migrating all 14.
- **BLOCKER ON PHASE 2**: `gitea-operations` worker-skill must be authored (mirror `github-operations`) before it can be attached during migration.
