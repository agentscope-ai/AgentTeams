# Milestone 2 — Implementation Plan (Project CRD, dashboard v1, operator tooling, quiet-rooms plumbing)

> **Status:** Approved-pending · **Date:** 2026-07-03 · **Owner:** @nattiini45
> Companion to [ai-coworking-reshape-plan.md](ai-coworking-reshape-plan.md) **v2.3** (decisions #1–#18, spikes S1–S10/S-GIT/S-BACKUP).
> Successor to [implementation-milestone-1.md](implementation-milestone-1.md) — **M1 is fully committed** on `impl/m1-hardening` (10 commits, `35a4412..ba44fb1`).
>
> **How to resume:** open a Claude Code session in this repo and say
> *"Implement Milestone 2 from docs/implementation-milestone-2.md, starting at Step 1"* (or any step).
> **First action on resume: this doc is committed on `impl/m1-hardening`; branch
> `impl/m2-projects-dashboard` from `impl/m1-hardening` before implementing.**
>
> Every seam below was re-verified against THIS checkout on 2026-07-03 (post-M1 — several of the
> v2.3 plan's line numbers have drifted since M1 landed; the corrected numbers are what's cited here).
> The full draft was then validated by 9 per-step seam validators + 1 doc-level consistency validator
> (2026-07-03); their corrections are folded in below (notably: Step 1 gained the Project REST-CRUD
> layer that Step 8's `hiclaw apply` requires). Anything that could NOT be verified from this
> checkout is flagged ⚠️ and listed in the ledger at the end.

## Orchestration model (MANDATORY, every step)

- **Fable** (main session) = orchestrator — writes the step brief, dispatches, integrates, runs tests, commits. Does not implement directly.
- **Sonnet subagent** (`model: 'sonnet'`, `isolation: worktree`) = implementor — implements the brief + writes/updates tests, reports diff (git patch) + test results.
- **Opus subagent** (`model: 'opus'`) = reviewer — reviews the diff against the brief AND the locked decisions (#12 controller Gitea-free — NEVER a parallel Gitea client or deploy-keys management, reuse the existing VPS gitea-mcp; #14 single-consumer isolation; #15 two-channel split; #16 CRD↔projectflow federation; #17 tier ladder; #18 operator-set project completion lifecycle). Verdict approve / request-changes; max 2 rounds, then Fable arbitrates.
- A step lands only after: Opus approval → Fable runs the step's checks → **one commit per step** (conventional message referencing the plan section).

## Scope

Everything REMAINING from plan v2.3 that is buildable/authorable/testable **from this checkout alone** — no VPS, no live Matrix/Gitea/MinIO. Live-only verification for authored artifacts (Steps 3, 4, 5, 7) is done statically (`bash -n`, stub-based unit tests, docker builds) with the live checks explicitly listed as **deferred-to-deploy**.

**In:** Project CRD + reconciler + REST CRUD (Phase 4/§10.1, #16/#18) · `GET /api/v1/manager-tasks` (§10.2) · dashboard **v1 read-only + v1.1 wake/sleep** (#17 tier ladder) · `provision-worker-gitea.sh` + per-worker Higress registration authoring (Phase 2, #12/#13/#14) · Phase 2b model-catalog + provider/route authoring (steps 1–2) · Phase 4.5 CoPaw CLI parity + default-runtime=hermes (#11) + stale-docs fix · Phase 5b quiet-rooms **mechanism, env-gated default-OFF** (defaults stay gated on S2/S3) · HEARTBEAT blocked-age nudge + orphan sweep (Phase 1b item 2 — the one M1 gap).

**Deferred to M3 (with reasons):** Phase 0/0b deploys (live-only) · all live spikes S1–S10/S-GIT/S-BACKUP (live-only) · `gitea-operations` skill (still blocked on S-GIT tool enumeration) · §6 runtime bumps (QwenPaw v1.1.12.post2 / hermes v0.17.0 — needs live re-validation of version-sensitive patches) · dashboard v1.5 message-UI + v2 kanban/DAG/detail + cross-instance fan-out + observability kit (tier ladder #17; M1 already shipped the v1.5 endpoints) · Phase 5b defaults-ON (gated S2/S3) · Phase 6 attachment→MinIO bridge (Hermes side gated S9; the CoPaw side is deferred with it so the design lands once — the M2 dashboard file browser covers the read path meanwhile) · Phase 3 team CRs + 14-persona migration (needs the live VPS registry, §10.4) · optional `Team.spec.modelProvider` team-wide field + Phase 2b step 4 chat-driven provider onboarding.

## Toolchain (verified on this box)

Go 1.26.3 (module targets go 1.25) + GNU Make 3.81 native. Controller tests: `cd hiclaw-controller && go test ./internal/... ./cmd/...` (= `make test-unit`).
**System python has NO pytest** — python tests run in the M1-built docker images `agentteams-copaw-test` / `agentteams-hermes-test`:
`docker run --rm -v <abs-path>:/w -w /w -e PYTHONPATH=src <image> pytest tests/ -q` — **run docker from PowerShell, never Git Bash** (msys path mangling).
**copaw suite baseline is PRE-EXISTING RED:** `test_bridge.py` uncollectable + 17 failures (scratchpad `copaw-baseline-failures.txt`); acceptance bar = **no NEW failures vs baseline**. Never add tests to `test_bridge.py`.
Shell tests run in `alpine` + `apk add jq bash`, **CRLF stripped** (checkout is `autocrlf=true`) — precedent `manager/tests/test-manage-state.sh`.
Node 24 + Docker 29.5.3 for the dashboard build. Root `tests/` stays a live-instance e2e suite — not runnable locally.

## Step order

**Wave 1 (parallel, disjoint files):** Step 1 (Go: CRD+CRUD) · Step 4 (bash: gitea helper) · Step 5 (Go+shell+json: 2b catalog) · Step 6 (Dockerfile/helm/installers/docs) · Step 7 (python: quiet rooms) · Step 9 (markdown).
**Wave 2 (after Step 1):** Step 2 (Go: manager-tasks — **file-collision serialization only**: both touch `internal/app/app.go` and `internal/server/http.go`; no correctness dependency, mirroring M1's "diff hygiene only" note) · Step 8 (skill federation — **true schema dependency**: needs Step 1's ProjectSpec + REST routes; no file collision with Step 1).
**Wave 3 (after Step 2):** Step 3 (dashboard — consumes the Step-2 endpoint).

---

### Step 1 — `Project` CRD + reconciler + REST CRUD + MinIO projection + lead notification *(plan Phase 4 steps 1–3/5, §10.1; decisions #2/#6/#16/#18; #12 guard)* — largest step

**Files:** `hiclaw-controller/api/v1beta1/types.go` (append), `api/v1beta1/register.go`, `config/crd/projects.hiclaw.io.yaml` (Helm copy is automatic), `internal/controller/project_controller.go` (+`_test.go`), `internal/app/app.go` (`initReconcilers`), **`internal/server/resource_handler.go` + `internal/server/http.go` (+ `resource_handler_test.go`) — Project REST CRUD (validator-added: Step 8's `hiclaw apply` calls hardcoded REST endpoints, `cmd/hiclaw/apply.go:88-115`, so without routes it 404s)**. Optional thin `internal/service/project_provisioner.go` — or use `oss.StorageClient` + `Provisioner.SendAdminMessage` directly; implementor's call, reviewer checks it stays MinIO-projection-only.
**Seams (all verified in this checkout):**
- `register.go:19-32` — unexported `addKnownTypes`; append `&Project{}, &ProjectList{}` to the `scheme.AddKnownTypes` call (`:20-29`).
- `types.go:92-94` marker idiom (`+genclient` / `+kubebuilder:subresource:status` / deepcopy); `EffectiveWorkerName` helper `:174-181` (mirror as `EffectiveProjectName`); `TeamStatus` `:362` (`TeamRoomID :364`, `Members []TeamMemberStatus :379`); `TeamMemberStatus.RuntimeName :406` — the source for default worker resolution when `spec.workers` is empty.
- `worker_controller.go:28-30` — reuse `finalizerName "hiclaw.io/cleanup"` / `reconcileInterval 5m` / `reconcileRetryDelay 30s`; `:61-111` the deferred status merge-patch + finalizer-add idiom to copy (`patchBase :72`, deferred patch `:78-93`, finalizer add `:102-108`); `:177-214` `reconcileDelete` with non-fatal cleanup + `RemoveFinalizer` merge-patch (`:206-210`); `:325-338` `computeWorkerPhase` keep-prior-phase-on-transient-error semantics; `:340-371` `SetupWithManager` + watch precedent for the **Team watch** (enqueue Projects whose `spec.team` membership changed); field-indexer precedent `team_controller.go:35` (`TeamLeaderNameField`).
- `human_reconcile_delete.go:22-51` — the non-fatal delete contract (log, never wedge the finalizer).
- `oss/client.go:7-33` — `PutObject :10`, `DeletePrefix :32`; wired from `app.go` (`OSS: a.oss` appears in `initHTTPServer :607`; reconcilers registered `app.go:530-600`).
- `provisioner.go:198-204` — **M1 already exported `SendAdminMessage`**; step 8b (notify the lead's Team Room once, condition `LeaderNotified`) needs NO new matrix plumbing.
- REST CRUD precedents: `resource_handler.go` `CreateWorker :93`, `ListWorkers :200`, `CreateTeam :377` (one handler method per kind); routes wired in `http.go:71-95` (`/api/v1/workers`, `/api/v1/teams`, …). Add `POST/GET /api/v1/projects`, `GET/PUT/DELETE /api/v1/projects/{name}` with `RequireAuthz` on resource `"project"` following the sibling idiom — enough surface for `hiclaw apply`'s existence-check-GET → create/update flow plus dashboard reads.
- CRD YAML shape: mirror `config/crd/workers.hiclaw.io.yaml:260-281` (`subresources.status`, `additionalPrinterColumns`, `scope: Namespaced`, `shortNames`). `hiclaw-controller/Makefile:50-53` — `make generate` runs controller-gen deepcopy AND `cp config/crd/*.yaml ../helm/hiclaw/crds/` — write the CRD to `config/crd/` only. **Validator note: the CRD YAML condition-type description must list all five condition types (ReposResolved|WorkersRecorded|MinIOProjected|LeaderNotified|DeprovisionPending) — the §10.1 skeleton shows only the first three; it predates the #16/#18 additions.**
**Spec:** exactly §10.1 — `ProjectSpec{Team(required), Description, ProjectName, Repos[]{URL,Access(rw|ro),Name}, Workers[]}`; `ProjectStatus{ObservedGeneration, Phase, Message, RepoCount, RecordedWorkers, Conditions[ReposResolved|WorkersRecorded|MinIOProjected|LeaderNotified|DeprovisionPending]}`. `status.repoCount` int backs the Repos printer column (no `.length` jsonPath). Manifest to `shared/projects/<id>/manifest.json` = `{id, team, description, repos[{url,access,name}], recordedWorkers, updatedAt}` — **no credential material**. `Completed`/`Archived` operator-set only (#18); on `Completed` raise `DeprovisionPending=True`; `Archived` moves the projection to a cold prefix. `reconcileDelete` = `DeletePrefix("shared/projects/<id>/")` non-fatal + finalizer removal.
**Hard guards (Opus checks):** the reconciler makes **NO Gitea calls, NO gateway calls, holds NO PATs** (#12/#13 — the operator helper, Step 4, owns those). CRD manifest and the lead's `projectflow` `meta.json` are **federated, never schema-merged** (#16) — both live under `shared/projects/<id>/` keyed by id.
**Tests:** `fake.NewClientBuilder().WithScheme(...).WithStatusSubresource(&v1beta1.Project{}, &v1beta1.Team{})` (precedent `team_controller_test.go:618-621` — the exact pattern — and `message_handler_test.go:134-136`); stub `oss.StorageClient` recording keys/payloads; stub admin-messenger. Cases: happy → `Ready` + manifest shape; missing team → `Degraded/TeamNotFound` + 30s requeue; empty `spec.workers` → recorded from `Team.Status.Members[].RuntimeName`; delete → `DeletePrefix` + finalizer removed even when OSS errors; `LeaderNotified` fires exactly once; `Completed` → `DeprovisionPending`. REST CRUD: handler tests mirroring `resource_handler_test.go` siblings (create/get/list/update/delete + 404/409 paths).
**Acceptance:** `make test-unit` green; `make generate` clean (deepcopy + Helm CRD copy both regenerate with no manual edits); printer columns Team/Repos/Phase/Age present; `hiclaw apply -f` of a fixture Project YAML succeeds against an httptest server wired with the new routes.

### Step 2 — `GET /api/v1/manager-tasks` — expose `state.json` *(plan §10.2 (2), Option 1 controller-side; feeds #17)* — after Step 1 (file-collision serialization only)

**Files:** new `internal/server/managertasks_handler.go` (+`_test.go`), `internal/server/http.go`, `internal/app/app.go` (thread the path into `ServerDeps`), `internal/config/config.go` (optional `HICLAW_MANAGER_STATE_FILE` override).
**Seams:**
- `http.go:90-95` manager-route + `RequireAuthz` idiom; `ServerDeps` `:21-35`. Gate: `RequireAuthz(ActionGet, "manager", nil)` (list-style, no `{name}`).
- Path resolution (verified end-to-end): `manage-state.sh:24` `STATE_FILE="${HOME}/state.json"` *(the plan's `:18` cite is stale post-M1)*; the spawned manager runs with `HOME=/root/manager-workspace` bound to host `HICLAW_WORKSPACE_DIR` (`install/hiclaw-install.sh:3153`); the **embedded controller container has the same host dir bound at `/root/hiclaw-fs/agents/manager`** (`install/hiclaw-install.sh:3535`, `install/hiclaw-install.ps1:3168`) = `config.AgentFSDir()` default `/root/hiclaw-fs/agents` (`config.go:444-446`) + `/manager`. So the handler reads `<AgentFSDir>/manager/state.json`.
- Response = the raw `state.json` document passed through (shape per §10.2, **plus M1's fields**: `cancelled_tasks[]` (`manage-state.sh:36,46`), `blocked` status/`blocked_since`, `last_digest_sent_at`) — do not re-model it; the dashboard treats it as opaque-ish JSON.
**Behavior:** file missing → `404 {"error":"manager state not available"}` (covers incluster mode — no mount — and a Manager that hasn't run `init` yet); unreadable/malformed → `502`; success → `200` + `Content-Type: application/json`. Doc-comment carries a curl example.
**Tests:** temp-dir state file + `httptest.NewRecorder()` (precedent `lifecycle_handler_test.go` / `message_handler_test.go`): happy, missing→404, malformed→502.
**Acceptance:** `make test-unit` green; embedded-only availability documented in the handler comment (k8s mode 404s by design — Option 2 stays future work per §10.2).

### Step 3 — Dashboard v1 (read-only) + v1.1 wake/sleep + same-origin proxy *(plan Phase 5 build step 1 + #17 tiers v1/v1.1; §10.2 contracts)* — after Step 2

**Files:** net-new top-level `dashboard/`: `dashboard/server/` (small Node proxy, `node:test` tests), `dashboard/web/` (Vite SPA), `dashboard/Dockerfile`, `dashboard/README.md`. No controller changes.
**Scope — what lands in M2 (v1 + v1.1 of the #17 ladder):**
1. **Proxy (mandatory, verified):** `:8090` sets **no CORS headers anywhere in the mux** (`http.go:44-137`) AND **requires a Bearer SA token in both modes** — the browser must never hold it. The proxy injects the admin token (embedded controller mints it at startup — `app.go:753-762` `bootstrapAdminCLIToken`, written to `HICLAW_AUTH_TOKEN_FILE=/var/run/hiclaw/cli-token`, `hiclaw-controller/Dockerfile:57-63`; the proxy reads a token-file path from env). Routes: `/api/managers|teams|workers|manager-tasks|projects` → controller GETs; `/api/tasks/*`, `/api/files/*` → MinIO (env `HICLAW_FS_BUCKET`, default `hiclaw-storage`, `config.go:298` — never hard-code). **Scoped allowlist:** GET-only, plus exactly `POST /api/workers/{name}/wake|sleep|ensure-ready` (v1.1) proxying `http.go:108-110` / `lifecycle_handler.go:35,74,113` — with a confirm dialog client-side and a request log server-side (the #17 audit trail). Path-traversal guard on the MinIO routes. `/docker/` is **never** proxied.
2. **SPA v1:** cards for Managers/Teams/Workers (`internal/server/types.go:239-260`, `:154-177`, `:61-86` response shapes); task table from `/api/manager-tasks` joined to MinIO `shared/tasks/{id}/meta.json`; Project browser **joining both** `manifest.json` (Step-1 CRD projection) and the chat-flow `meta.json`+`plan.md` by project id (#16 — render `[ ]/[~]/[x]` counts only, no DAG yet); file browser (list/download under `shared/` + `agents/`). Poll 15s; **30s for `/api/workers`** — it does a live backend `Status()` call per team member (`resource_handler.go:200` ListWorkers, `teamMemberToResponse :1049`, live status `:1093-1100`).
**Deferred to M3:** v1.5 message-injection UI (the endpoints exist since M1 — `http.go:98-100`), kanban/DAG/task-detail (v2), cross-instance fan-out, ETag conditional-GET, observability kit, Gitea API context panels.
**Tests:** `node --test` on the proxy: allowlist enforcement (unknown path/method → 405/404; the three lifecycle POSTs pass; everything else GET-only), token injected upstream and **stripped from responses**, traversal guard, request-log line per write. `npm run build` green; `docker build dashboard/` green. Live serve behind Traefik + real-controller e2e = deferred-to-deploy (README documents Traefik labels + env: `HICLAW_CONTROLLER_URL`, `HICLAW_AUTH_TOKEN_FILE`, `MINIO_ENDPOINT/ACCESS/SECRET`, `HICLAW_FS_BUCKET`).
**Acceptance:** proxy tests green; production build + image build; README deploy notes complete; Opus checks the allowlist against #17 (scoped writes only, logged; no `/docker/`).

### Step 4 — `provision-worker-gitea.sh` operator helper + per-worker registration authoring *(plan Phase 2 steps 2/5, §10.1 (4); decisions #4/#12/#13/#14/#18)* — authorable now, live-testable only

**Files:** net-new `scripts/provision-worker-gitea.sh`, `manager/agent/skills/mcp-server-management/scripts/setup-mcp-proxy.sh` (small backward-compatible flag), net-new `manager/tests/test-provision-worker-gitea.sh`.
**Seams:**
- `setup-mcp-proxy.sh:10` usage `<server-name> <url> <transport> [--header "K: V"]…`; `--header "Authorization: Bearer <PAT>"` becomes `securitySchemes[].defaultCredential` + `defaultUpstreamSecurity` in the mcp-proxy YAML (`:205-258`); server PUT `/v1/mcpServer` with `consumerAuthInfo.allowedConsumers ["manager"]` (`:262-278`); Step 3 manager-authorize (`:281+`).
- **The #14 hazard, verified:** Step 5 (`:326-340`) REPLACE-broadcasts **every** registry worker (`CONSUMER_LIST` built from `~/workers-registry.json`, consumer names `worker-<name>` `:334-335`, `PUT /v1/mcpServer/consumers` `:338-340`) and rewrites **all** workers' `config/mcporter.json` + MinIO push (`:341-383`).
**Change:** (a) give `setup-mcp-proxy.sh` an opt-out (`--consumers <csv>` or `--skip-worker-broadcast`) that skips Step 5 — default behavior byte-identical. (b) `provision-worker-gitea.sh <worker> --project <id>` (operator-run, **not** controller): creates the Gitea user + scoped PAT via `GITEA_URL`/`GITEA_ADMIN_TOKEN` (`POST /api/v1/admin/users`, `POST /api/v1/users/{u}/tokens`); registers `mcp-gitea-<worker>` via `setup-mcp-proxy.sh … --header "Authorization: Bearer <PAT>" --skip-worker-broadcast`; itself PUTs `/v1/mcpServer/consumers` = `["worker-<name>"]` (#14); updates **only that worker's** `mcporter.json` (+MinIO push, reusing the Step-5 per-worker block shape); reads `shared/projects/<id>/manifest.json` (Step-1 shape) and sets repo-collaborator roles ro→read / rw→write (#13). `--rotate` (PAT re-mint, §7 hardening) and `--deprovision <id>` (#18: remove collaborator grants, delete the per-worker server registration, optionally the Gitea user) round it out.
**Hard guards (Opus):** all Gitea/gateway calls live in THIS script — zero controller involvement (#12); no Step-5 broadcast path reachable from the helper (#14); PATs never written to the repo/manifest/logs.
**Tests (static only):** `bash -n` both scripts; alpine+jq+bash harness (CRLF-stripped, `test-manage-state.sh` precedent) with PATH-shimmed `curl`/`mc` recording every call: asserts (1) no all-workers consumer PUT ever fires, (2) the single-consumer PUT payload is exactly `["worker-<name>"]`, (3) ro→read / rw→write mapping from a fixture manifest, (4) `--deprovision` reverses grants/registration, (5) `setup-mcp-proxy.sh` without the new flag emits today's call sequence unchanged.
**Acceptance:** harness green. **Deferred-to-deploy (S-GIT):** PR attribution to two distinct Gitea users, worker-A-key → 401/403 on worker B's server, ⚠️ Higress `defaultCredential`→upstream-Bearer attach on the VPS Higress version, own-PAT native checkout.

### Step 5 — Phase 2b: model catalog + provider/route registration authoring *(plan Phase 2b steps 1–2, decision #7; S5/S6 live-gated)*

**Files:** `hiclaw-controller/internal/agentconfig/generator.go` (+test), `manager/configs/known-models.json`, `manager/configs/manager-openclaw.json.tmpl`, `manager/agent/skills/model-switch/scripts/update-manager-model.sh`, `manager/scripts/init/setup-higress.sh`.
**Seams (the 4 catalog spots + the installer, all verified):**
- `generator.go:375-418` `defaultModelSpec` presets (unknown → `{150000,128000}` `:400-403`); `allModelSpecs :451-472`; `allModelAliases :475-494`.
- `manager/configs/known-models.json` — flat 16-entry array (matches the 16-name `allModels` slice in `generator.go:452`/`:476`).
- `manager-openclaw.json.tmpl:53-70` provider models list; `:82-98` `agents.defaults.models` alias map.
- `update-manager-model.sh:87-106` ctx/max case table (+ input modalities `:114-119`).
- `setup-higress.sh:198-246` provider-registration idiom (openai-compat `:220-227`); `:248` `default-ai-route` body — `pathPredicate matchType=PRE matchValue="/"` (the S5 shadowing risk); `:253-281` boot-time rewrite of exactly that route name — **new routes must use a different name**. `initializer.go:432-438` (`EnsureAIRoute` skeleton) is left untouched.
**Change:** add `ollama/<model>` and `mimo/<model>` prefixed ids to all 4 catalog spots (same ctx/max in each — the reviewer greps for drift); extend `setup-higress.sh` with an **optional, env-gated extra-provider loop** (e.g. `HICLAW_EXTRA_LLM_PROVIDERS="ollama=https://ollama.com/v1;mimo=${MIMO_BASE_URL}"` + per-provider key envs): each gets a DNS service-source (name without `/`, `docs/faq.md:550-552`), an `openai`-type provider, and its **own AI route** (`hiclaw-<provider>-route`) with a model-prefix match — never touching `default-ai-route`. Env unset → the script's behavior is byte-identical.
**⚠️ Unverifiable here:** MiMo hosted base URL + exact current model ids (S6 — env-required, catalog entries marked provisional in a comment) and route-priority/shadowing vs the PRE-"/" default route (S5) — the route bodies carry a documented priority note; live confirm at deploy.
**Tests:** go test asserting the new ids resolve with the intended ctx/max and appear in specs+aliases; `bash -n`; a small consistency check (test or script) asserting the 4 catalogs agree on the id set; grep-assert `default-ai-route` is not referenced by the new loop.
**Acceptance:** `make test-unit` green; catalogs agree; unset-env no-op proven by the harness; S5/S6 live checks listed as deferred.

### Step 6 — Phase 4.5: CoPaw CLI parity, default worker runtime → hermes, stale-docs fix *(plan §4.5 CLI baking + decision #11 + §4.5 step-3 doc fix)*

**Files:** `copaw/Dockerfile`, `helm/hiclaw/values.yaml`, `install/hiclaw-install.sh`, **`install/hiclaw-install.ps1` (validator-added — it holds its own hardcoded default)**, `manager/agent/skills/worker-management/references/skills-management.md`, `manager/agent/worker-skills/README.md`.
**Seams:**
- `copaw/Dockerfile:65-71` — node layer installs only `mcporter skills`; the parity gap vs `hermes/Dockerfile:57-58` (`ripgrep`, `ffmpeg`) and `:79` (`@nacos-group/cli`).
- `helm/hiclaw/values.yaml:282` `worker.defaultRuntime: "openclaw"` → `"hermes"`. **Do NOT touch `:224`** — that is `manager.runtime` (Manager stays CoPaw per the locked-in table; set by env at deploy).
- `install/hiclaw-install.sh:2450` non-interactive default `copaw`; upgrade case `:2458-2464`; interactive case `:2473-2479` — note the version guard: hermes is only offered when `! _ver_lt "${HICLAW_VERSION}" "v1.1.0"` (`:2445,2459,2474`) — the new hermes default must keep the `<v1.1.0 → copaw` fallback.
- **`install/hiclaw-install.ps1:2216` and `:2624`** — two independent hardcoded `"copaw"` non-interactive defaults for `DEFAULT_WORKER_RUNTIME`; change both to `hermes`. ⚠️ Pre-existing parity gap: the ps1 `Step-Runtime` function (`:2207-2243`) unconditionally lists "3) hermes" with NO `HICLAW_VERSION` guard (unlike bash `_ver_lt`) — Step 6 adds the equivalent version guard to the ps1 default path (small, clearly right) OR, if it balloons, documents the gap in the commit message and defers the interactive-menu guard.
- `config.go:308` has **no** hardcoded default (env passthrough) — the sources that must agree (#11) are: bash installer, ps1 installer, helm values.yaml.
- Stale docs contradicting decision #10 / M1 step 9: `skills-management.md` Key-facts bullet "Workers cannot modify their own skills" (~`:38`) and `worker-skills/README.md` zh bullet "Worker 不能修改自己的 skills" (~`:55`) → rewrite to the self-install-with-builtin-floor semantics (builtin names re-overwritten each ~5-min reconcile; non-builtin names persist).
**Tests:** `docker build copaw/` (PowerShell) + smoke `docker run --rm <img> sh -c "rg --version && ffmpeg -version | head -1 && command -v nacos"`; grep-asserts on the **three** default-runtime sources (bash, ps1, helm); Opus reviews doc wording against plan §4.5.
**Acceptance:** image builds with the three tools present; all three sources agree on `hermes` (with the bash version-guard fallback intact and the ps1 guard added or explicitly deferred); docs no longer contradict #10.

### Step 7 — Phase 5b quiet-rooms plumbing, env-gated default-OFF *(plan Phase 5b steps 1–2; the DEFAULT stays gated on live S2/S3)*

**Files:** `hermes/src/hermes_matrix/policies.py`, `hermes/src/hermes_matrix/overlay_adapter.py`, `hermes/src/hermes_worker/bridge.py`, `copaw/src/copaw_worker/bridge.py`; tests `hermes/tests/test_policies.py` (extend) + **new** `copaw/tests/test_quiet_rooms.py` (never touch the uncollectable `test_bridge.py`).
**Seams:**
- `hermes/src/hermes_worker/bridge.py:249-253` — the stale comment ("consumed by hermes_matrix.adapter directly" — **false**, grep shows zero readers) + hardcoded `MATRIX_FILTER_TOOL_MESSAGES/THINKING: "true"` dead knobs.
- `overlay_adapter.py:103-107` `connect()` wrap hook; `:109-132` `_wrap_send_message_event` — the one in-repo outbound chokepoint (`wrapped` `:118-128`, `self._user_id :127`). ⚠️ The native adapter (`_matrix_native.py`) is created only at image build — whether streamed steps traverse this wrapper is exactly S3's live question; unit tests stub the client.
- `policies.py:95` `apply_outbound_mentions` — the pure-function precedent to mirror as `should_suppress_outbound(content, *, filter_tool, filter_thinking)`.
- `copaw/src/copaw_worker/bridge.py:225-238` `matrix_channel_cfg` (hardcoded `filter_tool_messages/filter_thinking: True` `:235-236`), config.json write `:242+`. Root flag: `copaw/src/matrix/config.py:1164` `show_tool_details: bool = True`. Both fork channels read the filter flags but never enforce them (`matrix_channel.py:188-189`, `channel.py:282-286`) — enforcement lives (if anywhere) in the external `BaseChannel` ⚠️ (not vendored; S2's live question). *(Plan's `:166-167,290-291` cites drifted post-M1 — corrected here.)*
**Change (mechanism only, default-off):**
- **Hermes:** implement `should_suppress_outbound` (drop tool-call/intermediate/thinking-shaped events; always pass plain final `m.text`, start/finish/heartbeat; pass `m.relates_to.rel_type=="m.replace"` final edits — policy documented in the docstring); enforce it inside the wrapped `send_message_event`, reading `MATRIX_FILTER_*` env in the overlay; `bridge.py` derives `MATRIX_FILTER_*` from **`HICLAW_QUIET_ROOMS` (default false)** instead of hardcoded `"true"` — net behavior today: unchanged (the strings were dead; making them live requires the gate). Fix the stale comment.
- **CoPaw:** when `HICLAW_QUIET_ROOMS` is truthy, `bridge.py` additionally writes root-level `"show_tool_details": false` into config.json (S2's best-guess tap) and keeps the channel-dict flags; env unset → **byte-identical config.json output to today** (regression-tested).
**Tests:** hermes — pure-function cases + stubbed-client wrapper test proving suppression + always-pass classes; bridge env-derivation cases. copaw — new test file asserting config.json with/without the env. Run both suites in the docker test images; copaw bar = no NEW failures vs `copaw-baseline-failures.txt`.
**Acceptance:** hermes suite green; copaw no-new-failures; env-unset output identity proven; defaults-ON + live efficacy explicitly deferred (S2/S3), including the `m.replace`-vs-new-events question.

### Step 8 — `project-management` federation updates *(plan Phase 4 steps 4/6; decision #16)* — after Step 1 (true schema dependency: needs ProjectSpec + the Project REST routes)

**Files:** `manager/agent/skills/project-management/SKILL.md`, `references/create-project.md`, `scripts/create-project.sh`, `manager/agent/skills/git-delegation-management/SKILL.md`.
**Seams:** `create-project.sh:64-74` chat-flow `meta.json` heredoc, `:141-143` room-id patch, `:292-294` `mc mirror` to `shared/projects/<id>/`; curl calls to `$HICLAW_MATRIX_URL` at `:46,121,144,153,160,198,212` (harness must shim these); the manager image bundles the `hiclaw` CLI (`manager/Dockerfile.copaw:56-57`) so `hiclaw apply` works from the Manager — **against the Step-1 REST routes** (`cmd/hiclaw/apply.go:88-115` hits `/api/v1/projects`; without Step 1 this 404s).
**Change:** `create-project.sh` gains optional `--team <t>` + repeatable `--repo <url>:<rw|ro>` flags → emits a Project CR YAML (Step-1 schema) and `hiclaw apply -f`s it **in addition to** the untouched chat-flow `meta.json` (federation #16 — two documents, one id, no merge). `SKILL.md`/`create-project.md` document the two-layer model (CRD = repo/access provisioning; projectflow = execution) and when to create the CR. `git-delegation-management/SKILL.md` re-points repo-URL/credential guidance at the Project manifest (not inline paths) and references the per-worker identity path (#4) — with an explicit "the Manager/skill never handles PATs" note.
**Tests:** `bash -n`; alpine harness with PATH-shimmed **`hiclaw`, `curl`, AND `mc`** (validator correction — create-project.sh curls Matrix and runs `mc mirror` unconditionally, so all three must be stubbed for the harness to run offline) recording the applied YAML — asserts team required, access enum enforced, no-flags invocation byte-identical to today; Opus checks wording against §10.1/#16.
**Acceptance:** harness green; docs consistent with plan v2.3; zero behavior change without the new flags.

### Step 9 — HEARTBEAT robustness: blocked-age nudge + orphaned-task sweep *(plan Phase 1 step 8 tail + Phase 1b item 2)* — markdown-only, closes the one M1 doc gap

**Files:** `manager/agent/copaw-manager-agent/HEARTBEAT.md` (304 lines today).
**Seams:** `:39-65` finite-task iteration (stall ping `:61-64`, container `recreated/failed` branches `:51-53`); `:239+` Step-7 healthy branch; `:260-278` M1's daily-digest gate (the digest already *reports* blocked items `:271` — but no step **nudges on blocked age** and no step **detects orphans**). M1's `manage-state.sh` actions are in place (`:15-20` usage: `mark-blocked/unblock/cancel/reassign/last-digest`; `blocked_since` field). Container delete+recreate on any spec change is real (`member_reconcile.go:318-329` — "spec changed, recreating container" → `wb.Delete` → `createMemberContainer`), which is exactly what strands an in-flight task.
**Change:** (a) **blocked-age nudge** in the finite-task loop: entries with `status:"blocked"` and `blocked_since` older than ~24h → escalate in the Step-7 report using the `[task-id]` envelope (M1 Step-10 format); (b) **orphan sweep**: for each finite active task, verify the assigned worker's container status via the existing check-worker flow AND task-dir recency (`mc stat`/latest `progress/YYYY-MM-DD.md`); worker gone or no progress across N cycles → `mark-blocked --reason "orphaned: container recreated/stalled"` + Step-7 flag — never silently delete the entry (cancel stays a human/`cancel`-action decision).
**Tests:** none executable — review-heavy step; Opus checks consistency with M1's Step-8/Step-10 texts, the digest gate (nudges are findings → immediate, never digest-gated), and Phase 5b non-interference (Manager→admin channel only).
**Acceptance:** doc matches plan Phase 1b wording; no contradiction with the daily-digest gate or quiet rooms.

---

## Milestone verification

```
cd hiclaw-controller && go test ./internal/... ./cmd/...      (Steps 1, 2, 5)
cd hiclaw-controller && make generate                          (Step 1 — deepcopy + CRD→Helm sync, no manual drift)
# PowerShell, never Git Bash:
docker run --rm -v <abs>/copaw:/w  -w /w -e PYTHONPATH=src agentteams-copaw-test  pytest tests/ -q   (Step 7 — bar: no NEW failures vs scratchpad baseline)
docker run --rm -v <abs>/hermes:/w -w /w -e PYTHONPATH=src agentteams-hermes-test pytest tests/ -q   (Step 7)
docker build copaw/                                            (Step 6)
alpine+jq+bash (CRLF-stripped): manager/tests/test-manage-state.sh (regression) ·
  manager/tests/test-provision-worker-gitea.sh (Step 4) · Step-8 create-project harness
cd dashboard && npm test && npm run build && docker build .    (Step 3)
bash -n on every touched shell script                          (Steps 4, 5, 8)
grep-assert default-runtime agreement across bash/ps1/helm     (Step 6)
```

First live checkpoint (deploy milestone): Steps 3/4/5 live behavior + spikes S2/S3/S5/S6/S-GIT — plus the root `tests/` e2e suite, which only runs after Phase 0 deploys.

## ⚠️ Unverified-assumption ledger (all flagged inline above)

1. **MiMo hosted base URL + current model ids** (S6) — env-required in Step 5; catalog entries provisional.
2. **Higress per-server `defaultCredential` → upstream Bearer attach** on the VPS Higress version (S-GIT) — Step 4 authored against the source-verified gitea-mcp header-precedence; the Higress leg is live-only.
3. **Higress route ordering** — model-prefix routes vs the PRE-"/" `default-ai-route` (S5) — Step 5 documents a priority note; cannot be validated from this checkout.
4. **CoPaw `show_tool_details` efficacy** — the external `ChannelManager`/`BaseChannel` is not vendored here; whether root `Config.show_tool_details` (config.py:1164) threads into the channel is S2's live question. Step 7 lands the tap default-off.
5. **Hermes streaming chokepoint** — `_matrix_native.py` exists only in built images; whether streamed steps traverse the wrapped `send_message_event` (and whether they are `m.replace` edits) is S3's live question. Step 7 enforces at the wrapper with stubbed-client tests only.
6. **gitea-mcp live tool names/schemas** — still unknown (S-GIT); `gitea-operations` stays excluded from M2.
7. **Ollama Cloud base URL `https://ollama.com/v1`** — carried from the plan's 2026-06-29 web research; not re-verifiable from this checkout.
8. **`market.hiclaw.io` reachability** (S7) — untouched in M2 (no build-time market-skill pre-fetch attempted).
9. **Plan line-number drift corrected in this doc** (post-M1): `manage-state.sh:18→24`; copaw filter-flag cites (`matrix_channel.py:166-167,290-291` → `:188-189` etc.); `resource_handler.go:181-222/1022-1083` → `:200+/:1049+`. Trust the numbers in THIS doc, re-verified 2026-07-03.
