# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, `hiclaw-controller/` here before the next release.

---

- feat(team-leader): refresh Team Leader coordination prompts and built-in skills for project, task, file-sharing, communication, organization, and mcporter workflows.
- fix(team-leader): keep legacy Team Leader skill names and helper scripts available for existing workspaces and integration tests.

- feat(hiclaw-controller): propagate controller-level skills API and Nacos auth defaults to workers.
- fix(copaw): suppress noisy warnings when optional MinIO objects do not exist.
- fix(hiclaw-controller): preserve default object-storage access when custom non-storage entries are configured.
- fix(copaw): skip static mc alias setup for k8s workers that use wrapper-provided credentials.
- fix(copaw): exclude inbound Matrix thread messages from room-history context.
- fix(copaw): align the CoPaw worker install directory with the HOME-backed workspace path.
- fix(copaw): seed the CoPaw worker agent heartbeat interval at 10 minutes.
- feat(agent): add non-overridable credential file access prohibition to CoPaw worker and team leader agent prompts.
- fix(manager): agent docs and jq examples use `roomID` for `hiclaw get workers` / `hiclaw create worker` JSON (CLI field name), not `room_id`
- fix(controller): add `+kubebuilder:subresource:status` on CR types; patch Worker finalizers instead of full `Update`; exponential backoff on REST update conflict retries
- fix(manager): document runtime-aware Worker dispatch (avoid @worker text in admin DM only); update task-management references, AGENTS.md, HEARTBEAT.md, channel-management skill
- fix(manager): separate runtime-specific AGENTS/HEARTBEAT for OpenClaw vs CoPaw; remove cross-runtime references from manager agent docs
- refactor(api)!: restructure `spec.mcpServers` on Worker/Manager/Team CRDs to `[]{name,url,transport}`; drop controller-side MCP gateway authorization; `mcporter-servers.json` is written from the CRD (see `docs/declarative-resource-management.md`)
- feat(hiclaw-controller): support Nacos remote skills and per-URI Nacos package auth with `authType=nacos|sts-hiclaw|none`, including `ai-registry` STS access scope.
- fix(hiclaw-controller): use UUID STS session names for credential-provider requests while logging the original caller label for traceability.
- fix(copaw-worker): pin the bundled Nacos CLI package to `@nacos-group/cli@1.0.5-beta.1`.
- fix(hiclaw-controller): preserve runtime-mutated package files during reconcile by seeding package/base files without overwriting existing storage objects.
- fix(copaw): stop Matrix typing indicators when a run completes without sending a message or is cancelled.
- fix(copaw): require slash-prefixed runtime control commands and normalize Element double-slash commands.
- fix(manager): quote coding CLI skill frontmatter descriptions that contain colons.
- feat(controller): separate CR resource names from runtime worker names across controller identity, credentials, storage defaults, and readiness reporting.
