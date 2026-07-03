# Milestone 1 — Implementation Plan (repo-side hardening + chat fixes)

> **Status:** Approved-pending · **Date:** 2026-07-02 · **Owner:** @nattiini45
> Companion to [ai-coworking-reshape-plan.md](ai-coworking-reshape-plan.md) **v2.3** (decisions #1–#18, spikes S1–S10/S-GIT/S-BACKUP).
>
> **How to resume:** open a Claude Code session in this repo and say
> *"Implement Milestone 1 from docs/implementation-milestone-1.md, starting at Step 1"* (or any step).
> Nothing from the v2.3 doc pass or this plan is committed yet — **first action on resume: commit
> `docs/` on `plan/ai-coworking-reshape`, then branch `impl/m1-hardening`.**
>
> All 10 steps were validated against the source by a dedicated Plan agent (2026-07-02): every cited
> seam confirmed; its three scoping refinements (S2 seam simplification, S3 split, S6 dead-env-var
> reuse + pytest spike) are folded in below. No step needs to move out of this milestone.

## Orchestration model (MANDATORY, every step)

- **Fable** (main session) = orchestrator — writes the step brief, dispatches, integrates, runs tests, commits. Does not implement directly.
- **Sonnet subagent** (`model: 'sonnet'`, `isolation: worktree`) = implementor — implements the brief + writes/updates tests, reports diff + test results.
- **Opus subagent** (`model: 'opus'`) = reviewer — reviews the diff against the brief AND the locked decisions (#12 controller Gitea-free, #14 single-consumer isolation, #15 two-channel split, #16 CRD↔projectflow federation, #17 tier ladder). Verdict approve / request-changes; max 2 rounds, then Fable arbitrates.
- A step lands only after: Opus approval → Fable runs the step's checks → **one commit per step** (conventional message referencing the plan section).

## Scope

Everything buildable/testable **from this checkout alone** — no VPS. Deferred to later milestones: Phase 0 deploy, all live spike parts (S1–S10, S-GIT, S-BACKUP), Phase 2 per-worker Higress registration + `provision-worker-gitea.sh` (authorable later, only testable live), `gitea-operations` skill (gated on S-GIT tool enumeration), the dashboard build, Phase 5b suppression defaults (gated on S2/S3), and the §6 runtime version bumps.

## Toolchain (verified on this box)

Go 1.26.3 (module targets go 1.25) · Python 3.14.5 · GNU Make 3.81 · Docker 29.5.3 · Node 24.
Controller tests: `make test-unit` in `hiclaw-controller/` (= `go test ./internal/... ./cmd/...`). No lint config exists.
Python tests: `PYTHONPATH=src pytest tests/` from `copaw/` and `hermes/` respectively.
Root `tests/` is a **live-instance bash e2e suite — not runnable locally**; it runs after Phase 0 deploys.

## Step order

S1 → S2 (independent Go) → S3, S4 (Dockerfiles, independent) → S5 (largest, Python) → S6 → S7 → S8 → S9 → S10 (S10 depends on S8). S1–S4 can interleave if a step blocks on review.

---

### Step 1 — Docker backend: resource limits + member RestartPolicy *(plan Phase 0 step 6)*

**Files:** `hiclaw-controller/internal/backend/docker.go`, `internal/config/config.go`, `internal/controller/member_reconcile.go`, `internal/backend/docker_test.go`.
**Seams:**
- `dockerHostConfig` `docker.go:491-498` — add `Memory int64` + `NanoCpus int64` (Docker Engine API JSON names `Memory`, `NanoCpus`); `SecurityOpt` is the add-a-field precedent.
- `buildCreatePayload` `:517-599`; ⚠️ **the HostConfig is only attached if one of NetworkMode/ExtraHosts/PortBindings/Binds/RestartPolicy is set (`:584-587`) — new fields must join that condition or they silently drop.**
- `req.Resources` (`interface.go:70-75,119-121`) carries k8s-style strings (`"1000m"`, `"2Gi"`) — convert via `resource.ParseQuantity` (already a dependency): CPU → `MilliValue()*1e6` NanoCpus; memory → `Value()` bytes.
- Mirror `kubernetes.go` `buildDefaultResources` `:433-450` (defaults **1000m/2Gi**) + `mergeResourceOverrides` `:454-488` as docker-side helpers.
- Config: add `HICLAW_DOCKER_WORKER_CPU/_MEMORY` (+ Manager variants) mirroring the K8s envs (`config.go:290-291`, `K8sConfig()` `:500-512`); extend `DockerConfig` struct (`docker.go:21-28`) + `config.go DockerConfig()` `:459-468`.
- RestartPolicy: `createMemberContainer` (`member_reconcile.go:414-425`) sets none — add `RestartPolicy: "unless-stopped"` like the Manager gets at `manager_reconcile_container.go:222` (docker.go attaches when non-empty, `:550-553`).
**Tests:** `docker_test.go` uses `mockDockerAPI(t)` (`:14-70`) — a real `httptest.Server` faking the Docker Engine API. Capture the `POST /containers/create` body; assert `HostConfig.Memory`/`NanoCpus` (a) from `req.Resources`, (b) from defaults when nil; assert member RestartPolicy.
**Acceptance:** `make test-unit` green; k8s backend behavior unchanged.

### Step 2 — Controller message-injection endpoints *(plan #17 v1.5)*

**Files:** `internal/server/http.go`, new `internal/server/message_handler.go` (+`_test.go`), `internal/service/provisioner.go` (one exported method).
**Seams:**
- Route pattern `http.go:43-131` (`mux.Handle("POST /path", mw.RequireAuthz(action, resource, nameFn)(http.HandlerFunc(...)))`, `nameFn = authpkg.NameFromPath` `:66`). `ServerDeps.Provisioner *service.Provisioner` exists (`:33`).
- `Provisioner.matrix` is **unexported** (`provisioner.go:139`) — add exported `func (p *Provisioner) SendAdminMessage(ctx, roomID, body string) error` wrapping `matrix.Client.SendMessageAsAdmin` (`internal/matrix/client.go:649-658`). Precedent for handler→Provisioner wiring: the credentials handler at `http.go:117`.
- **Validator-confirmed seam:** `matrix.Client` is already an interface that includes `SendMessageAsAdmin` — the thin exported Provisioner wrapper is the whole change; tests stub `matrix.Client` directly (no extra narrow interface needed).
- Sequencing note: run Step 2 before Step 7 — both touch controller Go/`config.go`; ordering is for diff hygiene only, not correctness.
**Routes:** `POST /api/v1/managers/{name}/message` → Manager CR `Status.RoomID`; `POST /api/v1/teams/{name}/message` → Team CR `Status.LeaderDMRoomID` (`types.go` TeamStatus; TeamRoomID at `:364` is the fallback if LeaderDM is empty — reviewer should check plan #17 intent: instructions go to the **lead**). Body `{"body":"..."}`; 400 empty body; 404 unknown CR; 409 if the target room isn't provisioned yet. Auth: `ActionUpdate` on `"manager"`/`"team"`.
**Tests:** mirror `lifecycle_handler_test.go` (`TestLifecycleSleepSetsSleepingPhase` `:18-63`): `fake.NewClientBuilder().WithScheme(...).WithStatusSubresource(...)` + `httptest.NewRecorder()` + direct handler call + stub messenger recording (roomID, body).
**Acceptance:** `make test-unit` green; handler doc-comment includes a curl example.

### Step 3 — Embedded image health *(plan §7)* — split in two (validator recommendation)

**Files:** `hiclaw-controller/Dockerfile.embedded`, `hiclaw-controller/supervisord.embedded.conf` / entrypoint (locate the kine start path; add a small `healthcheck.sh` + `preflight.sh`).
**S3a — HEALTHCHECK (safe, do first):** `HEALTHCHECK CMD` = one composite check: controller `/healthz` (`:8090`) + MinIO `/minio/health/live` + Tuwunel `/_matrix/client/versions`.
**S3b — kine SQLite integrity pre-flight (gated):** **first verify the `sqlite3` binary exists in the built image** (unverified — the validator flags this as a real risk); install it in `Dockerfile.embedded` if absent, or fall back to a tiny Go/`python3 -c` check if installing is unreasonable. Then: `PRAGMA integrity_check` on the kine DB **before** kine starts; on failure log loudly and refuse to start (no silent fallback-to-empty).
**Tests:** `docker build` locally succeeds; `docker inspect` shows the Healthcheck; document a manual corruption check (copy DB, truncate, expect refusal).
**Acceptance:** image builds; HEALTHCHECK present; pre-flight refuses a corrupted DB with a clear log line.

### Step 4 — Wire `patch_copaw_stream_errors.py` into `copaw/Dockerfile` *(plan §6)*

**Files:** `copaw/Dockerfile`, `copaw/scripts/patch_copaw_stream_errors.py` (read-only).
**Change:** COPY + RUN the script against the **standard venv's** copaw package, next to the sed patches at `:93-102` (the three `patch_*_lazy.py` scripts show the copy/invoke idiom, but they target the lite venv — confirm the target path inside the script itself). Ensure the RUN fails the build if the patch markers aren't found (the script targets `"llm",\n    ]` and `"deadline exceeded",` markers in `copaw/exceptions.py`).
**Acceptance:** `docker build` green; built image's `exceptions.py` contains the `ModelTimeoutException` reclassification and the 900s timeout.

### Step 5 — CoPaw channel fixes in BOTH files *(plan Phase 1 steps 1/2/4; decision #15)* — largest step

**Files:** `copaw/src/matrix/channel.py` (Manager overlay, 2693 lines), `copaw/src/copaw_worker/matrix_channel.py` (leads/workers, 1613 lines — a **stale fork subset**), `copaw/tests/test_channel_mention.py`, new `copaw/tests/test_worker_channel.py`.
**Three sub-changes, each in BOTH files:**
1. **Bare-`@mention` resolution** — `_was_mentioned` (`channel.py:981-1014` / `matrix_channel.py:662-683`, near-verbatim twins; 3 tiers: m.mentions → matrix.to href regex → raw-MXID regex). Add tier 4: bare `@localpart` resolved against a localpart cache built from **room members** (works in both files without needing `workers-registry.json`; the manager may additionally consult `~/workers-registry.json`). Implementor decides the nio API for member listing; cache with TTL.
2. **Immediate ack** — on accepting a task/mention, send a short ack via direct `room_send` bypassing the queue. Precedent: the readiness-probe path (test `test_matrix_readiness_probe_replies_directly_without_enqueue`, `test_channel_mention.py:418-458`); primitives: `_send_plain_text` (`channel.py:958-978`) / `send()` (`matrix_channel.py:1501-1533`). Env-gate `HICLAW_CHAT_ACK` (default on).
3. **Catch-up replay** — today both files clear `event_callbacks` during first sync and drop everything (`channel.py:763-800` / `matrix_channel.py:548-569`). Buffer suppressed message events (cap ~50, skip own) and replay after ready. **Also port the drift fixes to the worker copy:** `timeout=0` first sync (`channel.py:773` vs stale `timeout=30000` at `matrix_channel.py:554`), the None-guard on token save (`channel.py:779-781`), and configurable sync timeout.
**Tests:** extend `test_channel_mention.py` (channel.py) for all three; **new mirrored test file for `copaw_worker.matrix_channel` — nothing tests it today.** Run: `cd copaw && PYTHONPATH=src pytest tests/`.
**Acceptance:** pytest green; bare `@alice` wakes the handler in both impls (test-proven); replay test proves no first-boot message loss.

### Step 6 — Heartbeat interval configurable *(plan Phase 1 step 3)*

**Validator finding (changes this step's shape):** `HICLAW_MANAGER_HEARTBEAT_INTERVAL` **already exists and already flows to the manager container** (`internal/service/worker_env.go:80`) — but it is a **dead env var with zero consumers**. Reuse that exact name; do NOT invent a new one. Second finding: `bridge.py` contains **no heartbeat-handling code**, yet `copaw/tests/test_bridge.py` asserts heartbeat-seeding behavior — the logic lives either in the vendored `copaw` package or those tests are currently red. **Mandatory first action: run the pytest spike (`cd copaw && PYTHONPATH=src pytest tests/test_bridge.py -k heartbeat -v`) to establish ground truth before writing any code.**
**Files:** `copaw/src/copaw_worker/` (wherever the spike shows heartbeat config is applied), `copaw/src/copaw_worker/templates/agent.manager.json` (`:11-14`, hardcoded `"every": "30m"`), `manager/configs/manager-openclaw.json.tmpl:104` (hardcoded `"1h"`), `manager/scripts/init/start-manager-agent.sh` (exports around `:632-667`; **two** jq branches `:696-736` upgrade + `:755-778` fresh — the upgrade branch bypasses the template, so a jq clause is required either way).
**Change:** consume `HICLAW_MANAGER_HEARTBEAT_INTERVAL` (default **10m**; plan wants 5–10m). CoPaw path: override `heartbeat.every` at the point the spike identifies. OpenClaw analogue: `${HICLAW_MANAGER_HEARTBEAT_INTERVAL:-10m}` in the tmpl + export + jq clause in **both** branches.
**Tests:** python unit for the CoPaw override; `bash -n`; grep-assert both jq branches set `.agents.defaults.heartbeat.every`.
**Acceptance:** setting the env changes the effective interval in both generated configs; unset → 10m; the formerly-dead env var now has a consumer.

### Step 7 — Solo mode *(plan Phase 1 step 5)*

**Files:** `internal/config/config.go` (`HICLAW_SOLO_OPERATOR`), `internal/service/provisioner.go` `renderManagerWelcomeBody` `:1540-1568` (non-interview welcome variant), `internal/controller/manager_reconcile_welcome.go` (thread the flag), `team_controller.go` (force `PeerMentions=true`; today default-true at `types.go:243`), `resource_handler.go:620` / `types.go:468` (`PermissionLevel` — treat the sole Human as admin).
**Scope guard:** keep to (a) welcome variant, (b) PeerMentions force, (c) config flag + admin default. If the PermissionLevel strip balloons across handlers, defer the deeper strip and note it — don't let this step grow.
**Tests:** unit: flag on → welcome body contains no 4-question interview; config parse; PeerMentions forced.
**Acceptance:** `make test-unit` green; flag documented alongside the other `HICLAW_*` envs.

### Step 8 — `manage-state.sh` new actions *(plan Phase 1 step 8 + Phase 1b)*

**Files:** `manager/agent/skills/task-management/scripts/manage-state.sh` (functions `:47-181`, dispatch `:276-306`), new `manager/tests/test-manage-state.sh`.
**Change:** add `mark-blocked --task-id T --reason` / `unblock` (set/clear `status:"blocked"` + `blocked_since` on the entry; `action_list` prefixes `[BLOCKED since <ts>]`), `cancel` (remove entry, record reason), `reassign` (swap `assigned_to`/`room_id`), `last-digest` get/set (`last_digest_sent_at`, for Step 10). **Id-collision fix:** `action_add_finite` currently SKIPs on exact-id match (`:55-61`) — same id but *different* title/assignee must suffix (`-2`, `-3`) and add; identical call stays SKIP.
**Tests:** no shell harness exists in-repo — add a plain-bash test script driving a temp `state.json` through every action; `bash -n` both scripts.
**Acceptance:** test script passes end-to-end.

### Step 9 — Skill-pruning fixes, three sites *(decision #10; spike S4 static finding)*

**Files:** `copaw/src/copaw_worker/sync.py:709-716`, `hermes/src/hermes_worker/sync.py:448-455`, `hermes/src/hermes_worker/worker.py:368-375`; tests `copaw/tests/test_worker_sync.py` + `hermes/tests/`.
**Change:** at all three prune sites, **skip** (don't `rmtree`) local skill dirs absent from MinIO; log once per dir. Builtin floor semantics unchanged — the controller's `Overwrite:true` push still wins name collisions (plan §4.5).
**Tests:** extend existing sync tests: a self-installed skill dir survives `pull_all` and Hermes startup `_sync_skills`.
**Acceptance:** `pytest` green in both packages.

### Step 10 — Doc-skills pack *(plan Phase 1 steps 7/9; Phase 3 step 5)* — after Step 8

**Files:** `manager/agent/skills/task-management/references/finite-tasks.md` (intake skeleton under "Assigning a finite task", `:10`), `manager/agent/copaw-manager-agent/HEARTBEAT.md` Step 7 (`:235` — daily-digest time-gate via `last_digest_sent_at`), `manager/agent/team-leader-agent/skills/communication/SKILL.md` (Escalation Report envelope), `manager/agent/team-leader-agent/skills/team-coordination/SKILL.md` (cross-team Manager-mediated pattern + PeerMentions scope note).
**Markdown-only** (+ relies on Step 8's state field). Review-heavy step — the Opus reviewer checks consistency with plan §5 texts and that the digest doesn't fight Phase 5b quiet-rooms.
**Acceptance:** docs match plan v2.3 wording; no new contradiction.

---

## Milestone verification

```
cd hiclaw-controller && make test-unit
cd copaw  && PYTHONPATH=src pytest tests/
cd hermes && PYTHONPATH=src pytest tests/
docker build copaw/   (Step 4)   ·   docker build -f hiclaw-controller/Dockerfile.embedded .  (Step 3)
bash manager/tests/test-manage-state.sh   (Step 8)
```

The root `tests/` e2e suite runs only after Phase 0 deploys on the VPS — first live checkpoint for Steps 1/2/5/6/7 together.
