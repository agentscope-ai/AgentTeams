# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/`, `hiclaw-controller/`, and release-facing `install/` / Helm chart changes here before the next release.

---

- fix(copaw): validate bridge profiles (`worker`|`manager`) and align CoPaw unit tests with current Worker/bridge/toolhelper APIs
- fix(ci): install shared `agentteams_*` Python packages only for the hermes remediation-gates matrix entry
- ci: skip embedded integration tests when `AGENTTEAMS_LLM_API_KEY` is unset (Makefile + workflow); forks without the secret no longer fail every shard at install-embedded
- fix(worker,manager): pass `--break-system-packages` when pip-installing shared merge/sync packages into OpenClaw images (PEP 668)
- fix(copaw): tolerate current PyPI Matrix `sync_loop()` shape in the legacy `_sync_loop` indentation patch so the worker image builds
- fix(controller): populate `QwenPawWorkerImage` across Docker/K8s/Sandbox config builders and wire `AGENTTEAMS_QWENPAW_WORKER_IMAGE` through the Helm chart (values/helpers/controller env)
- feat(install): wire `AGENTTEAMS_QWENPAW_WORKER_IMAGE` through embedded install (override/default/pull/env-file/controller `-e`), mirroring Hermes
- feat(manager): add `qwenpaw-worker-agent` template (incl. find-skills parity); embed `openhuman-worker-agent` and `qwenpaw-worker-agent` templates in the controller image and route their builtin agent dirs; sync shared worker skills into the qwenpaw runtime tree
- note(helm): `worker.defaultRuntime` changed from `hermes` → `openclaw` — Workers created without `spec.runtime` now default to openclaw on Helm upgrade
- fix(controller): add `openhuman` to the Worker CRD runtime enum and `qwenpaw` to the Team member CRD runtime enum (config/crd + helm/crds mirror)
- fix(hiclaw): align advertised `--runtime` help with CRD-accepted sets (managers `openclaw|copaw`; workers/team members `openclaw|copaw|hermes|openhuman|qwenpaw`) across create/update/apply and Helm values comment
- fix(controller): requeue (5s) instead of blocking in-goroutine for scoped worker storage config; single-shot non-blocking probe with clear waiting status; probe key matches deploy path (QwenPaw/Edge → `runtime/runtime.yaml`, legacy → `openclaw.json`) using RuntimeName
- fix(controller): refresh the status patch base after finalizer add so the deferred status patch diffs against in-sync metadata
- fix(controller): remove duplicate `AGENTTEAMS_WORKER_CR_NAME` dead-store in member reconcile
- fix(install): remove `eval` from installer prompt/config helpers (shell-injection vector); use `printf -v` and `${!var}` indirect expansion
- fix(shared): parse the STS credential file and validate the controller env file instead of dot-sourcing untrusted content
- fix(copaw): stop logging a partial Matrix access token on re-login
- fix(controller): remove the phantom `projects.hiclaw.io` CRD (reconciled group is `agentteams.io`); legacy clusters may run `kubectl delete crd projects.hiclaw.io`
- docs: fix broken design links in `AGENTS.md`; restore required `AGENTTEAMS_WORKER_NAME` in quickstart/worker guides (en + zh-cn); document `AGENTTEAMS_WORKER_CR_NAME` only as optional CR override
- test: add QwenPaw config-builder, `builtinAgentDir`, non-blocking scoped-config, OpenHuman image-routing, and shell (installer prompt / env-file / credential-file) coverage; hook `shared/tests/test-*.sh` via `make test-shared` and remediation-gates `shared-shell`

- fix(controller): parallel ListWorkers health probes with shared 5s list budget; GetWorker attaches healthChecks on CR and synthesized paths; teamMember responses copy lastHeartbeat/lastActiveAt from TeamMemberStatus
- fix(controller): worker `/ready` ignores negative llmCallsSinceLastHeartbeat; retries status update up to 2 times on conflict (still 204 when persistence fails)

- docs(manager): AGENTS/HEARTBEAT completion flow requires verify-output before meta/state updates; Leader completions use same verify gate as Step 2
- test(manager): extend test-verify-output.sh for ## Deliverables, task-relative paths, and exists claims
- refactor(dashboard): extract overview freshness/health helpers to overview-health.js; add node --test coverage

- feat(controller): five-point worker health probes (container/heartbeat/LLM/git/sync) on worker status + list API with 30s cache; `GET /v1/models` + Higress MCP inventory probes
- feat(dashboard): worker overview cards show 5-dot health strip alongside Phase 2/3 freshness + LLM call counts

- feat(copaw): count Higress gateway LLM HTTP calls and report `llmCallsSinceLastHeartbeat` on worker `/ready` heartbeat loop
- feat(controller): persist `llmCallsLastHeartbeat` / `llmCallsTotal` on Worker status; expose on worker API responses
- feat(dashboard): worker overview cards show last-window LLM call count alongside heartbeat freshness badges

- feat(controller): Project CR `spec.dependsOn` with reconciled `status.dependencies` for cross-project visibility; dashboard cross-project graph; Manager project-management skill + HEARTBEAT gate unsatisfied deps before assignment

- feat(controller): persist `LastHeartbeat` on worker `/ready`; expose `lastHeartbeat` and `lastActiveAt` on worker API responses
- feat(dashboard): worker overview cards show heartbeat/active freshness (healthy &lt;10m, degraded &lt;30m, down ≥30m)
- docs(copaw): remove stale Team Leader `ActionReady` 403 gotcha (authorizer now allows it)

- fix(controller): desiredPodRevision applies resolved runtime in standard pod hash path; dedupe team-member lookup via WorkerResourceService; hiclaw manager-state rejects unknown flags; create team accepts --leader alias
- fix(manager): create-team.sh removes duplicate hiclaw create call
- feat(manager): add verify-output.sh and manage-state verify action for task deliverable checks; update finite-tasks/HEARTBEAT completion flow and verifiable_claims schema

- fix(copaw): manager DM room bootstrap uses `AGENTTEAMS_MATRIX_URL`/bridged homeserver and env token fallback; deferred refresh after Matrix auto-join (`manager_bootstrap`, `run_manager_app`)
- fix(manager): CoPaw worker builtin notify waits for runtime via `--wait-runtime` in bootstrap `workers.sh`
- fix(manager): `create-team.sh` passthrough hiclaw flags, poll Running/room IDs, update `teams-registry.json` after controller create
- fix(manager): drop stale `PYTHONPATH=/opt/hiclaw/copaw/src` from `start-copaw-manager.sh` (copaw_worker installed via site-packages)

- refactor(manager): Phase 9 G9.6 — thin `start-copaw-manager.sh`; CoPaw Manager bootstrap via `copaw_worker.run_manager_app` + `manager_bootstrap` (WorkspaceLayout, skills symlink, DM auto-reply, CMS, openclaw.json watcher)
- refactor(manager): Phase 9 G9.7 — unified `manager/Dockerfile` multi-target build (`manager-agent-bundle`, `manager-copaw`, `manager-openclaw`); Makefile `build-manager-copaw` uses `--target manager-copaw`
- refactor(manager): Phase 9 G9.8 — shared AGENTS/HEARTBEAT fragments under `manager/agent/fragments/`; `render-manager-prompts.sh` + upgrade-builtins Step 0; CoPaw fast-reply / pending-workers rules preserved in copaw overlays

- refactor(hermes): Phase 6 Y6.3 — migrate Hermes sync onto `agentteams_sync` via `hermes_worker.sync` shim (300s pull, shared-in-pull_all, Hermes PushPolicy + inner→outer bridge); hermes-worker image installs agentteams-sync
- refactor(qwenpaw): Phase 6 Y6.6 — migrate QwenPaw push allowlist onto `agentteams_sync` via `qwenpaw_worker.sync` shim (`PushPolicy.qwenpaw`, byte-accurate compare ≤20 MiB); qwenpaw-worker image installs agentteams-sync
- fix(openhuman): Phase 6 Y6.5 — align shared/ push excludes with SyncContract (`*/spec.md`, `*/base/*`); document bash sync semantics in entrypoint
- refactor(worker): Phase 6 Y6.4 — thin OpenClaw background sync via `python -m agentteams_sync daemon --contract=openclaw`; preserve startup mirror, Matrix re-login, and E2EE crypto wipe in `worker-entrypoint.sh`; worker image installs agentteams-sync
- refactor(worker): O13.5 — extract OpenClaw Matrix crypto wipe + re-login from `worker-entrypoint.sh` into `python -m agentteams_sync openclaw-matrix --contract=openclaw`; add focused unit tests
- refactor(agentteams_sync): add OpenClaw daemon (`openclaw.py`, `PushPolicy.openclaw`, PULL_MARKER push guard, 300s fallback pull with shared `--newer-than 5m`)
- refactor(agentteams_sync): add `team_resolver=agents_md`, `pull_includes_shared`, Hermes byte-accurate push, QwenPaw `_cat_bytes`/`max_compare_bytes` push hooks, and `PushPolicy.openhuman()` preset

- feat(openhuman): Phase 13 — add Python `openhuman_worker` bridge (openclaw.json → config.toml) with unit tests; thin entrypoint delegates to `openhuman-worker bridge`; fix mc alias to use `AGENTTEAMS_STORAGE_ALIAS` from hiclaw-env
- docs(openhuman): document OpenHuman as Kubernetes-only in quickstart and install script header (not in embedded install worker-runtime menu)
- fix(worker): correct README directory layout — agent templates live under `manager/agent/worker-agent/`, not `worker/agent/`
- refactor(hermes): extract shared `agentteams_matrix_policies` package; Hermes re-exports via compatibility shim (O13.4)
- docs(sync): Phase 6 Y6.1 — `design/sync-contract.md` + `agentteams_sync.contract` presets document per-runtime sync semantics (preserve Hermes 300s vs CoPaw 60s, shared-in-pull_all divergence)
- refactor(copaw): Phase 8 bridge/workspace — add WorkspaceLayout (materialize/rebridge/persist_edits), split bridge_config vs bootstrap_copaw_runtime, extract MatrixBootstrapClient, ensure skills symlink, wire quiet_rooms to MatrixChannel, replace Dockerfile sed with fail-fast matrix sync_loop patch script

- feat(copaw): taskflow submit_task verifies deliverable artifacts locally and stats each path on storage after push; TeamHarness check_task runs artifact verification and returns failedClaims
- fix(protocol): align claim path resolution with verify-output.sh; parse `## Deliverables` in result.md; fail-closed verify when filesystem unavailable; CoPaw submit_task verifies before marking submitted
- refactor(controller): Phase 10 soft refactors — split team reconciler by concern; unify `buildMemberRuntimeConfigReq`, channel policy builder, and `NewMemberDeps`; extract `ProvisionWorker` phases and `RoomMembershipOptions` room-membership API
- refactor(controller): Phase 10 continued — split `gateway.Client` into focused interfaces; extract deployer openclaw merge helpers to `deployer_merge.go`; move worker-deps manifest builders to `internal/workerdeps`; add `WorkerResourceService` for standalone worker create domain rules; golden spec-hash vector test (DesiredPodRevision deferred); clarify Manager `RefreshManagerCredentials` vs legacy `RefreshCredentials`
- refactor(controller): Phase 10 soft items — consolidate pod-recreate hashing behind `desiredPodRevision` with locked golden vectors; mechanical splits for `provisioner`/`deployer`/`matrix` client, `config` load/types, and `api/v1beta1` types by kind; document LegacyCompat deprecation path and member reconcile non-fatal/Observed semantics (C10.10–12 docs only)
- refactor(copaw): Phase 7 matrix unification — move `matrix_relations` to `matrix/relations.py`; extract `OutboundFilterPolicy` to `matrix/outbound_policy.py`; Worker loads unified overlay via custom_channels shim (threads/ledger preserved); delete divergent `copaw_worker/matrix_channel.py` implementation; freeze vendored `matrix/config.py` (X7.4) and QwenPaw overlay growth (X7.5)
- refactor(copaw): share Team Leader DM preamble detection between `message_filter` hook and `matrix/outbound_policy` (dedupe regex/runtime.yaml parsing)

- refactor(protocol): Phase 5 P5.3/P5.4 — move TeamHarness MCP projectflow/taskflow into `plugins/teamharness/mcp/tools/*.py` with shared `mcp_common.py`; server.py registry-only for those tools; `AGENTTEAMS_REFACTOR_PROTOCOL_CORE=1` enables extra DAG validation dual-run
- test(protocol): add `snapshots/teamharness-mcp/` characterization for overlapping dag-delegate-flow actions; document P5.7 filesync deferral to Phase 6 SyncContract
- refactor(teamharness): Phase 5 P5.7 — MCP `filesync` delegates pull/push/stat/list to `agentteams_sync.FileSync` with explicit `storage.sharedPrefix` / `globalSharedPrefix` roots; preserve workspaceDir layout and global-shared read-only push; add `TEAMHARNESS_MCP` SyncContract preset

- refactor(protocol): extract agentteams_protocol shared domain from copaw_worker.task; CoPaw re-exports via copaw_worker.task shim; TeamHarness MCP uses protocol_bridge for DAG validation and shared runtime_config with WorkerFlow; MCP matrix formatting extracted to tools/matrix_format.py; tool dispatch modules for projectflow/taskflow/filesync/artifact; copaw-worker image installs agentteams-protocol package

- refactor(shared): extract agentteams_openclaw_merge as canonical openclaw.json merge library (MERGE_RULES docstring); merge-openclaw-config.sh becomes python3 -m wrapper; copaw_worker and hermes_worker sync import shared module; worker/manager/copaw/hermes images install agentteams-openclaw-merge package


- fix(manager): remove unreachable k8s Higress inner block from start-manager-agent.sh (k8s leaves console URL empty; controller owns Higress setup)
- fix(manager): drop redundant shared/builtins/worker/ MinIO publish from upgrade-builtins.sh; per-agent Step 3 sync remains the sole worker builtin path
- fix(manager): correct create-worker.md post-creation paths to openclaw vs copaw Manager runtimes only (Hermes is Worker-only)

- refactor(shared): add is_cloud_runtime/is_local_runtime helpers to hiclaw-env.sh; use them in start-manager-agent.sh
- refactor(manager): add resolve-model-params.sh sourced from known-models.json; migrate update-manager-model.sh
- refactor(manager): dedupe find-skills script via shared-worker-skills/ and sync-shared-worker-skills.sh (upgrade-builtins + image build)
- refactor(qwenpaw): Phase 12 — split monolithic `update.py` into `qwenpaw_worker/update/` package (runtime_config, agent_package, model_sync, channel_writers, teams_prompt, runtime_updater); re-export via `qwenpaw_worker.update`
- refactor(qwenpaw): extract worker orchestration helpers (`plugin_bootstrap.py`, `runtime_configurator.py`, `security_bootstrap.py`); reuse `plugin_install.py` for zip install/digest
- refactor(teamharness): move QwenPaw Matrix trigger/task-room helpers to `plugins/teamharness/adapters/qwenpaw/matrix_channel.py`; overlay delegates via runtime loader
- feat(qwenpaw): Phase 12 Q12.4 — pin `qwenpaw==1.1.11`, add `qwenpaw_site_packages_gate.py` (version gate, upstream marker/checksum manifest, Python overlay apply); Dockerfile uses gate instead of raw `cp`
- refactor(qwenpaw): Phase 12 Q12.5 — extract shared `agentteams_matrix_format` package; QwenPaw Matrix overlay and TeamHarness MCP `matrix_format` delegate markdown-it rendering
- test(qwenpaw): Phase 12 Q12.6 — replace Matrix overlay AST/source-lock tests with behavior tests (streaming config, ready marker, mentions); extract overlay harness; add gate unit tests
- chore(qwenpaw): defer full thin Matrix overlay rewrite, upstream sha256 manifest population in CI, and runtime monkeypatch replacement for defer-MCP site-packages patch (still build-time Python patch)
- refactor(plugins): shared plugins/adapters/qwenpaw/install-plugin.sh for TeamHarness and WorkerFlow adapters

- refactor(install): extract shared defaults to install/defaults.env; bash and PowerShell installers source/read it (ports, image names, version gates)
- refactor(manager): move MCP YAML templates to manager/configs/mcp-templates/; setup-mcp-server.sh accepts --template; setup-higress.sh uses configs path
- refactor(design): move higress-api-doc.json out of agent skills-alpha to design/ (developer reference, not agent workspace)
- refactor(manager): consolidate MCP aliyun guard in gateway-api.sh (gateway_require_local_mcp_management)
- docs(install): document embedded vs legacy Docker/supervisord layout (docs/embedded-docker-layout.md, manager/docker-legacy/README.md)
- docs(manager): defer hiclaw create team CLI thickening and shared state module; document CLI-first team create and manage-state.sh-only OpenClaw state
- feat(hiclaw): thicken `hiclaw create team` with team-admin, peer-mentions, channel-policy, and per-worker model/skills/MCP flags (Phase 14 I14.3)
- feat(hiclaw): add `hiclaw manager-state` shared state CLI; manage-state.sh delegates to it with shell fallback (Phase 14 I14.4)
- refactor(manager): thin `create-team.sh` to `hiclaw create team` wrapper; legacy shell path in `create-team-legacy.sh` (`HICLAW_TEAM_CREATE_IMPL=shell`)
- feat(hiclaw): add `--worker-runtimes` to `hiclaw create team` (CreateTeamRequest already supports per-worker runtime)
- docs(manager): document create-team wrapper vs Manager-only human backfill; mention `hiclaw manager-state` in task-management and controller-api skill docs

- refactor(manager): Phase 9 bootstrap — split start-manager-agent.sh into lib/bootstrap/* modules; thin entrypoint sources secrets/matrix/higress/openclaw-config/workers/cloud-sync helpers
- refactor(manager): unify aliyun/k8s workspace sync in bootstrap/cloud-sync.sh with runtime-aware .openclaw/.copaw excludes
- feat(manager): add send-manager-message.sh for bootstrap Matrix sends (CoPaw channels send vs OpenClaw curl); wire welcome DM and worker builtin notify
- feat(manager): add send-task-message.sh; task-management finite/infinite docs use helper instead of hiclaw runtime lookup
- fix(manager): skip openclaw-cms-plugin and config-health cleanup on CoPaw Manager runtime path

- fix(integration): integrate fork code-review remediation (Tier 0–2C) onto the renamed AgentTeams upstream baseline. Replays the `manager/` / `copaw/` / `hermes/` / `hiclaw-controller/` / `install/` / `plugins/` / `dashboard/` remediation (security/data-loss, correctness, cleanup/dedup) onto `upstream/main` @ `06d75c6` including the `a7b707e` hard-cut AgentTeams contracts rename. Reconciles the `AGENTTEAMS_*` env-var rename end-to-end: fixes the dead Manager heartbeat override (controller emitted `AGENTTEAMS_MANAGER_HEARTBEAT_INTERVAL` but CoPaw/OpenClaw read `HICLAW_`), the dead `CMS_SERVICE_NAME` worker propagation, and the unwired `AGENTTEAMS_SOLO_OPERATOR` config field; renames the fork-internal `MANAGER_HEARTBEAT_INTERVAL`/`CMS_SERVICE_NAME`/`MANAGER_STATE_FILE`/`QUIET_ROOMS`/`CHAT_ACK`/`SOLO_OPERATOR` stragglers. Ports the Tier 0 #8 `oss-credentials.sh` cached-cred-before-refresh fallback onto the dynamic `MC_HOST_*` alias machinery, the Tier 1D openhuman `find-skill` test coverage, and the `buildDesiredMembers` team-controller member builder + helpers. Adds `.gitattributes` (LF hygiene), `.github/workflows/remediation-gates.yml` (CI: go test + pytest + dashboard npm + helm lint), and the `docs/upstream-integration-migration.md` operator cutover runbook. Also captures previously-unrecorded image-affecting changes from `faa8874` (provider-management skill) and `915653d` (11 Kilo review fixes on PR #2).
- feat(qwenpaw): add the QwenPaw worker runtime Python package baseline with runtime config sync, storage sync, heartbeat reporting, Matrix channel overlay, and focused unit tests.
- fix(controller): surface Kubernetes Pod container failures in Worker backend status and status API responses.
- feat(controller): expose low-cardinality AgentTeams controller metrics and optional Helm ServiceMonitor.
- feat(controller): add Matrix AppService Human SSO identity provisioning with hash-derived Matrix IDs, AppService login, deletion deactivation, and Team admin/member identity resolution from Human status.
- fix(agent): update file-sharing path guidance for CoPaw and Team Leader agents to use `/root/hiclaw-fs/agents/...` instead of the retired `/root/.hiclaw-worker/...` path.
- feat(helm): add a Helm LLM preflight hook and reusable `hiclaw llm-preflight` command to validate API key/base URL/model before controller startup.

- fix(copaw): harden Matrix channel control-command handling, task-thread routing, NO_REPLY suppression, and cancellation noise handling.
- feat(controller): add OpenKruise Sandbox backend support for Workers via `spec.backendRuntime=sandbox`, including SandboxClaim lifecycle, status watches, CRD schema, and Helm RBAC/env wiring.
- fix(controller): materialize sandbox Worker runtime env/auth material into worker-deps storage before creating SandboxClaim deps and block legacy pod-to-sandbox runtime switches until the Worker is stopped.
- feat(controller): add per-agent `spec.resources` support for Manager, Worker, Team Leader, and Team Worker CRDs.
- fix(worker): pass `X-HiClaw-Cluster-ID` when remote Workers refresh controller-issued STS credentials for OSS and Nacos AI registry access.
- feat(hiclaw-controller): support a separate agent-pod-template for `deployMode: Remote` Workers via an optional `pod-template-remote.yaml` key on the controller-scoped pod-template ConfigMap; remote-mode Pod creation prefers this key and transparently falls back to `pod-template.yaml` when it is absent or empty, while non-remote/Sandbox paths keep ignoring it.
- feat(controller): move Kubernetes resources to the `agentteams.io/v1beta1` API group and decouple Team membership through `spec.workerMembers` references to standalone Worker CRs.
- feat(runtime): let Worker runtimes consume terminal `AGENTTEAMS_*` storage, controller, and auth environment variables while preserving existing `HICLAW_*` deployment compatibility.

- **OpenHuman runtime**: OpenHuman added as the fourth Worker runtime with native Matrix support via `channel-matrix` feature flag; includes controller routing (K8s + Docker backends), Dockerfile, entrypoint script, agent template, Helm chart integration, and Makefile build targets.
- **Multi model providers**: Worker, Team, and Manager specs can now select a Higress model provider via `spec.modelProvider`; the controller resolves the provider, injects the matching gateway URL into runtime config, and authorizes consumers only on the selected AI route.
- **modelProvider authorization boundary**: Controller reconcilers now own provider-specific AI route authorization, while provisioning keeps using the default gateway authorization path to avoid duplicate provider coupling.
- **Matrix AppService mode**: The controller can register as a Matrix Application Service and provision/log in users with the `as_token` instead of per-user passwords (legacy password auth is preserved when disabled). Enabled by default via `HICLAW_MATRIX_APPSERVICE_ENABLED`; the install script and the Helm `runtime-env` Secret generate and persist `HICLAW_MATRIX_APPSERVICE_AS_TOKEN` / `HICLAW_MATRIX_APPSERVICE_HS_TOKEN`. Set `HICLAW_MATRIX_APPSERVICE_USER_NAMESPACE_REGEX` to narrow the exclusive user namespace when running against a shared / pre-existing homeserver.

- fix(controller): bind Docker worker console port to 127.0.0.1 instead of all interfaces when AGENTTEAMS_CONSOLE_PORT is set ([859869a](https://github.com/nattiini45/AgentTeams/commit/859869a))

**Bug Fixes**

- **Legacy Team channel policy**: Legacy Team reconciliation now writes final Matrix channel allow-lists to member runtime config and re-adds the Team Leader to the Manager allow-list so controller integration tests observe durable policy state.
- **Sandbox worker-deps hardening**: Sandbox-backed Workers now prepare controller-owned worker-deps env/token/data material before claim creation, recycle stale SandboxClaims and bound Sandboxes on runtime-affecting changes, and use bounded ServiceAccount token projection for built-in SandboxClaim mounts.
- **CLI AgentTeams auth env**: The `hiclaw` CLI now discovers `AGENTTEAMS_CONTROLLER_URL`, `AGENTTEAMS_AUTH_TOKEN`, `AGENTTEAMS_AUTH_TOKEN_FILE`, and `AGENTTEAMS_CLUSTER_ID` while preserving legacy `HICLAW_*` fallbacks, so Manager and Worker containers can use the terminal env names for controller calls.
- **CoPaw worker runtime environment**: CoPaw workers now prefer AgentTeams storage/runtime environment variables while preserving legacy HiClaw fallbacks, and Qwen-style model health preflights disable thinking for lightweight readiness checks.
- **CoPaw Worker heartbeat**: CoPaw worker templates now seed heartbeat at a 10-minute interval so Team Leader agents created from the worker template can run heartbeat turns without requiring an explicit Team CR heartbeat spec.
- **Helm CRDs**: Removed unsupported `propertyNames` schema fields from Worker and Team CRDs so Kubernetes API servers accept the chart CRDs.
- **CoPaw local runtime paths**: CoPaw direct-run defaults now honor `COPAW_INSTALL_DIR` and `COPAW_WORKING_DIR` before falling back to local home-directory paths, while container entrypoints can continue to pass explicit directories.
- **CoPaw worker replies**: CoPaw's no-text debounce only recognized copaw `Content` objects, but the custom Matrix channel builds plain-dict content parts, so every message was buffered and the agent never replied. The channel now also recognizes dict-based text/audio parts; workers and team leaders reply as expected.
- **Worker Matrix token refresh**: `POST /api/v1/credentials/matrix-token` was registered against the `credentials` resource kind, but the worker credentials branch only permitted `ActionSTS`, so the route was dead and workers could not self-refresh on a homeserver 401. The self-scoped credentials branch now also permits `ActionRefreshMatrixToken` (and the misplaced dead entry was removed).
- **Helm AppService tokens**: The Helm `runtime-env` Secret now generates and preserves the AppService `as_token` / `hs_token` (values override -> existing Secret -> generated) so a default Helm/in-cluster install no longer crash-loops the controller, and a template comment trim-marker that broke `helm lint` YAML parsing was fixed.
- **Remote Worker authentication boundary**: Remote Worker tokens are now bound to the matching local Worker CR's `deployMode: Remote`, target cluster ID, target namespace, and ServiceAccount name before authorization.
- **Remote Worker applied target auth**: Remote Worker authentication now prefers the status-pinned deployment target and falls back to spec only before first provisioning, so spec target edits do not immediately break the running remote Worker or trust a target before it is applied.
- **Remote Worker lifecycle boundary**: Workers now record the applied deployment target in status, reject running target changes until the Worker is Stopped, clean up using the applied target, and register remote Pod watches for Worker/Team status updates.
- **Team Worker CR decoupling**: Worker identity enrichment and Worker REST APIs now resolve `spec.workerMembers` references, and Teams reject sharing the same referenced Worker CR before injecting coordination context.
- **Matrix AppService integration**: SSO Human Team admins now resolve through the Human identity source, Matrix AppService transaction push routes are wired into the controller registration path, and registration keeps the homeserver-facing controller URL as the endpoint base.

- fix(copaw): install vendored `matrix` overlay as top-level site-packages package (alongside `copaw.app.channels.matrix`) so `from matrix.*` imports resolve in Worker and Manager CoPaw images
- fix(manager): install `agentteams-protocol`, `agentteams-openclaw-merge`, and `agentteams-sync` in manager-copaw images; pass shared-package build contexts from `make build-manager-copaw`
- fix(hermes): install unpublished `agentteams-*` shared wheels before `hermes-worker` (`--no-deps`) so clean PyPI builds do not fail
- fix(qwenpaw): install `agentteams-sync` and `agentteams-matrix-format` before `qwenpaw-worker` (`--no-deps`); add Makefile build contexts for matrix-format/policies packages
