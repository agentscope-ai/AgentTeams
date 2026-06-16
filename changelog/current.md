# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `openclaw-base/` here before the next release.

---

- fix(agent): update file-sharing path guidance for CoPaw and Team Leader agents to use `/root/hiclaw-fs/agents/...` instead of the retired `/root/.hiclaw-worker/...` path.
- feat(controller): add per-agent `spec.resources` support for Manager, Worker, Team Leader, and Team Worker CRDs.

- **OpenHuman runtime**: OpenHuman added as the fourth Worker runtime with native Matrix support via `channel-matrix` feature flag; includes controller routing (K8s + Docker backends), Dockerfile, entrypoint script, agent template, Helm chart integration, and Makefile build targets.
- **Multi model providers**: Worker, Team, and Manager specs can now select a Higress model provider via `spec.modelProvider`; the controller resolves the provider, injects the matching gateway URL into runtime config, and authorizes consumers only on the selected AI route.

**Bug Fixes**

- **CoPaw Worker heartbeat**: CoPaw worker templates now seed heartbeat at a 10-minute interval so Team Leader agents created from the worker template can run heartbeat turns without requiring an explicit Team CR heartbeat spec.
- **Helm CRDs**: Removed unsupported `propertyNames` schema fields from Worker and Team CRDs so Kubernetes API servers accept the chart CRDs.
- **CoPaw local runtime paths**: CoPaw direct-run defaults now honor `COPAW_INSTALL_DIR` and `COPAW_WORKING_DIR` before falling back to local home-directory paths, while container entrypoints can continue to pass explicit directories.

**HiClaw → AgentTeams rename — Phase 0** (see [#861](https://github.com/agentscope-ai/HiClaw/issues/861))

Backwards-compatible groundwork for the upcoming project rename. Existing `HICLAW_*` env vars, the `hiclaw` CLI, the `hiclaw.io` CRD group, and the `hiclaw` Helm chart all keep working unchanged.

- **Shell `resolve_env` helper**: new `shared/lib/resolve-env.sh` reads `AGENTTEAMS_<KEY>` first and falls back to `HICLAW_<KEY>` with a one-time deprecation warning. `shared/lib/hiclaw-env.sh` now mirrors RUNTIME / MATRIX_URL / AI_GATEWAY_URL / FS_BUCKET / STORAGE_PREFIX / EMBEDDING_MODEL into both prefixes; the "empty string disables embedding" semantic is preserved.
- **Controller `envcompat` package**: Go counterpart of `resolve_env` (`internal/envcompat`). 77 `os.Getenv("HICLAW_*")` call sites across config, backend, proxy, executor, mail, and CLI now route through `envcompat.Lookup` so any `AGENTTEAMS_*` value transparently overrides the legacy variable. 100 % statement coverage; verified under `go test -race` with a 64-goroutine concurrency stress test that asserts the once-per-key warning guarantee.
- **Dual-name CLI**: the same Go binary now adapts its `Use` field to `os.Args[0]`, and every container that ships the CLI also installs an `agt` symlink alongside `hiclaw`. End-to-end test invokes the binary under both names via symlink and asserts the help text adapts.
- **READMEs**: top-of-file rename announcement added to `README.md`, `README.zh-CN.md`, and `README.ja-JP.md` linking to #861.
