# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, `agentteams-controller/`, and release-facing install/chart changes here before the next release.

---

**Bug Fixes**

- **Installer Token Plan models**: Update the quick-start Token Plan GLM option to `glm-5.2` while accepting existing `glm-5` values during upgrades. (Refs [#1076](https://github.com/agentscope-ai/AgentTeams/issues/1076))
- **CoPaw Worker workspace projection**: Write Worker prompts, skills, tool configuration, and Matrix agent settings into CoPaw's default workspace so Team Leaders load their assigned role and join the Team Room. ([9074def](https://github.com/agentscope-ai/AgentTeams/commit/9074def3))
- **Team Worker room boundary convergence**: Remove Manager again after standalone Worker infrastructure reconciliation restores regular Team Worker personal-room membership. ([b5b0add](https://github.com/agentscope-ai/AgentTeams/commit/b5b0add))
- **Team Worker reference enforcement**: Keep referenced Worker CRs protected during direct deletion and reject Team API members whose required role is empty. ([d96f1ed](https://github.com/agentscope-ai/AgentTeams/commit/d96f1ed))
- **Team Worker room membership**: Force Manager out of regular Team Worker personal rooms when equal Matrix power levels prevent a normal kick. ([43545c2](https://github.com/agentscope-ai/AgentTeams/commit/43545c2))
- **CoPaw Team assignment localparts**: Route Team Leader assignments that mention a Team Worker by Matrix localpart from Leader DM to Team Room. ([973e291](https://github.com/agentscope-ai/AgentTeams/commit/973e291))
- **CoPaw Team coordination routing**: Route Team Leader worker assignments sent through the `message` tool from Leader DM to Team Room, matching the Matrix channel send path. ([92c8145](https://github.com/agentscope-ai/AgentTeams/commit/92c8145))
- **Pinned OpenClaw source fetch**: Fetch the pinned OpenClaw commit directly so the base image build does not depend on a retired-brand external branch name. ([b0081c2](https://github.com/agentscope-ai/AgentTeams/commit/b0081c2))

**Branding and Compatibility**

- **Pre-v1.2 installer storage prefix**: Select the legacy `hiclaw/agentteams-storage` prefix for v1.1.2 images while keeping the AgentTeams prefix for v1.2 and newer images. ([ed81b7b](https://github.com/agentscope-ai/AgentTeams/commit/ed81b7b9))
- **Complete AgentTeams runtime rename**: Rename installer and Helm entrypoints, the controller Go module and CLI, and container filesystem paths to AgentTeams while preserving thin compatibility aliases and upgrade migration for existing HiClaw installations. ([3121f5f](https://github.com/agentscope-ai/AgentTeams/commit/3121f5f))
- **Hard-cut AgentTeams naming**: Remove retired-brand installer wrappers, environment fallbacks, CLI aliases, Helm naming branches, runtime path migrations, and active source paths so fresh AgentTeams deployments use one canonical contract end to end. ([d20e606](https://github.com/agentscope-ai/AgentTeams/commit/d20e606617edefbbc42c28c1201c5629fa73fd88))
- **Hard-cut Team and Worker resources**: Make Worker CRs the sole owners of runtime configuration and lifecycle, make Team CRs reference existing Workers through `spec.workerMembers`, and remove inline-member, registry, migration, and dependent-script compatibility paths. ([b3cf360](https://github.com/agentscope-ai/AgentTeams/commit/b3cf360))
- **Terminal Team API consumers**: Preserve Team admin and human-member fields in `agt` JSON output, and update integration cleanup and Team DAG setup for independently managed Worker CRs. ([cd05efe](https://github.com/agentscope-ai/AgentTeams/commit/cd05efe))
- **Terminal Team room topology**: Remove Manager from regular Team Worker personal rooms while retaining the Leader room, and restore Manager membership when Workers return to standalone operation. ([a5d6435](https://github.com/agentscope-ai/AgentTeams/commit/a5d6435))
