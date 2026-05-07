# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `openclaw-base/` here before the next release.

---

- feat(controller): Worker package resolve/extract no longer requires `SOUL.md` in the archive or directory; missing personality is filled later by `DeployWorkerConfig` defaults or inline `spec.soul`
- feat(controller): honor user-defined `spec.env` (`map[string]string`) on Worker, Manager, `Team.leader`, and `Team.workers[]`; variables merge with controller/backend-managed env at `createMemberContainer` / `createManagerContainer` with system keys winning on collision (dropped keys logged once at INFO); CRD exposes `env` as a string map (`additionalProperties.type=string`). `hiclaw` CLI / REST API surface for this field is deferred—use `kubectl apply` for now.
- fix(controller): add `+kubebuilder:subresource:status` on CR types; patch Worker finalizers instead of full `Update`; exponential backoff on REST update conflict retries
- fix(manager): document runtime-aware Worker dispatch (avoid @worker text in admin DM only); update task-management references, AGENTS.md, HEARTBEAT.md, channel-management skill
- fix(manager): separate runtime-specific AGENTS/HEARTBEAT for OpenClaw vs CoPaw; remove cross-runtime references from manager agent docs
- refactor(api)!: restructure `spec.mcpServers` on Worker/Manager/Team CRDs to `[]{name,url,transport}`; drop controller-side MCP gateway authorization; `mcporter-servers.json` is written from the CRD (see `docs/declarative-resource-management.md`)
- feat(controller): support `mcpServers` on Team Leader spec; leader can now declare MCP servers the same way team workers do
- chore(build): add `make generate` target to regenerate deepcopy functions via controller-gen and sync CRDs to Helm chart
- fix(controller): honor `TeamWorkerSpec.Runtime` and `HICLAW_DEFAULT_WORKER_RUNTIME` for team workers in `teamWorkerSpecToWorkerSpec()` and `teamMemberToResponse()` (regression from #666 that silently forced team workers to `copaw`). Leader runtime remains `copaw` by design. Existing team pods keep their current runtime until the user edits the Team spec or deletes the pod; new/edited teams pick up the fix immediately ([af52054](https://github.com/agentscope-ai/HiClaw/commit/af52054))

