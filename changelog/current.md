# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, `agentteams-controller/`, and release-facing install/chart changes here before the next release.

---

**Bug Fixes**

- **CoPaw Team coordination routing**: Route Team Leader worker assignments sent through the `message` tool from Leader DM to Team Room, matching the Matrix channel send path. ([92c8145](https://github.com/agentscope-ai/AgentTeams/commit/92c8145))
- **Pinned OpenClaw source fetch**: Fetch the pinned OpenClaw commit directly so the base image build does not depend on a retired-brand external branch name. ([b0081c2](https://github.com/agentscope-ai/AgentTeams/commit/b0081c2))

**Branding and Compatibility**

- **Complete AgentTeams runtime rename**: Rename installer and Helm entrypoints, the controller Go module and CLI, and container filesystem paths to AgentTeams while preserving thin compatibility aliases and upgrade migration for existing HiClaw installations. ([3121f5f](https://github.com/agentscope-ai/AgentTeams/commit/3121f5f))
- **Hard-cut AgentTeams naming**: Remove retired-brand installer wrappers, environment fallbacks, CLI aliases, Helm naming branches, runtime path migrations, and active source paths so fresh AgentTeams deployments use one canonical contract end to end. ([d20e606](https://github.com/agentscope-ai/AgentTeams/commit/d20e606617edefbbc42c28c1201c5629fa73fd88))
- **Hard-cut Team and Worker resources**: Make Worker CRs the sole owners of runtime configuration and lifecycle, make Team CRs reference existing Workers through `spec.workerMembers`, and remove inline-member, registry, migration, and dependent-script compatibility paths. ([b3cf360](https://github.com/agentscope-ai/AgentTeams/commit/b3cf360))
- **Terminal Team API consumers**: Preserve Team admin and human-member fields in `agt` JSON output, and update integration cleanup and Team DAG setup for independently managed Worker CRs. ([cd05efe](https://github.com/agentscope-ai/AgentTeams/commit/cd05efe))
