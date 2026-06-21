# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/`, `hiclaw-controller/`, and release-facing `install/` / Helm chart changes here before the next release.

---

- fix(agent): update file-sharing path guidance for CoPaw and Team Leader agents to use `/root/hiclaw-fs/agents/...` instead of the retired `/root/.hiclaw-worker/...` path.
- feat(controller): add per-agent `spec.resources` support for Manager, Worker, Team Leader, and Team Worker CRDs.

- **OpenHuman runtime**: OpenHuman added as the fourth Worker runtime with native Matrix support via `channel-matrix` feature flag; includes controller routing (K8s + Docker backends), Dockerfile, entrypoint script, agent template, Helm chart integration, and Makefile build targets.
- **Multi model providers**: Worker, Team, and Manager specs can now select a Higress model provider via `spec.modelProvider`; the controller resolves the provider, injects the matching gateway URL into runtime config, and authorizes consumers only on the selected AI route.
- **Matrix AppService mode**: The controller can register as a Matrix Application Service and provision/log in users with the `as_token` instead of per-user passwords (legacy password auth is preserved when disabled). Enabled by default via `HICLAW_MATRIX_APPSERVICE_ENABLED`; the install script and the Helm `runtime-env` Secret generate and persist `HICLAW_MATRIX_APPSERVICE_AS_TOKEN` / `HICLAW_MATRIX_APPSERVICE_HS_TOKEN`. Set `HICLAW_MATRIX_APPSERVICE_USER_NAMESPACE_REGEX` to narrow the exclusive user namespace when running against a shared / pre-existing homeserver.

**Bug Fixes**

- **CoPaw Worker heartbeat**: CoPaw worker templates now seed heartbeat at a 10-minute interval so Team Leader agents created from the worker template can run heartbeat turns without requiring an explicit Team CR heartbeat spec.
- **Helm CRDs**: Removed unsupported `propertyNames` schema fields from Worker and Team CRDs so Kubernetes API servers accept the chart CRDs.
- **CoPaw local runtime paths**: CoPaw direct-run defaults now honor `COPAW_INSTALL_DIR` and `COPAW_WORKING_DIR` before falling back to local home-directory paths, while container entrypoints can continue to pass explicit directories.
- **CoPaw worker replies**: CoPaw's no-text debounce only recognized copaw `Content` objects, but the custom Matrix channel builds plain-dict content parts, so every message was buffered and the agent never replied. The channel now also recognizes dict-based text/audio parts; workers and team leaders reply as expected.
- **Worker Matrix token refresh**: `POST /api/v1/credentials/matrix-token` was registered against the `credentials` resource kind, but the worker credentials branch only permitted `ActionSTS`, so the route was dead and workers could not self-refresh on a homeserver 401. The self-scoped credentials branch now also permits `ActionRefreshMatrixToken` (and the misplaced dead entry was removed).
- **Helm AppService tokens**: The Helm `runtime-env` Secret now generates and preserves the AppService `as_token` / `hs_token` (values override -> existing Secret -> generated) so a default Helm/in-cluster install no longer crash-loops the controller, and a template comment trim-marker that broke `helm lint` YAML parsing was fixed.
