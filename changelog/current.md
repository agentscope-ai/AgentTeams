# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `openclaw-base/` here before the next release.

---

- fix(controller): add `+kubebuilder:subresource:status` on CR types; patch Worker finalizers instead of full `Update`; exponential backoff on REST update conflict retries
- fix(manager): document runtime-aware Worker dispatch (avoid @worker text in admin DM only); update task-management references, AGENTS.md, HEARTBEAT.md, channel-management skill
- fix(manager): separate runtime-specific AGENTS/HEARTBEAT for OpenClaw vs CoPaw; remove cross-runtime references from manager agent docs

