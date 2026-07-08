# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/`, `hiclaw-controller/`, and release-facing `install/` / Helm chart changes here before the next release.

---

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
- **test(14) deterministic git-collab setup**: `tests/test-14-git-collab.sh` now pre-creates the three workers (alice, bob, charlie) via `hiclaw apply worker`, calls `create-project.sh` to materialise the shared Matrix room + meta.json, marks the project active and syncs it to MinIO before sending the task to Manager. Manager now focuses only on the 4-phase coordination logic; the worker + project setup steps that previously stalled on the Manager tool-guard approval prompt (`jq | mv | cat` to update meta.json) are now part of the test runner and never enter the LLM control loop.
