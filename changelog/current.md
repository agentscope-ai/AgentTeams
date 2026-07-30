# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, `agentteams-controller/`, and release-facing install/chart changes here before the next release.

---

**Bug Fixes**

- **Manager model parameters**: Helm and embedded install scripts now pass custom model context window, max output tokens, reasoning, and vision settings into the controller so regenerated Manager/Worker `openclaw.json` files keep the install-time model limits.
- **CoPaw Worker workspace projection**: Write Worker prompts, skills, tool configuration, and Matrix agent settings into CoPaw's default workspace so Team Leaders load their assigned role and join the Team Room. ([9074def](https://github.com/agentscope-ai/AgentTeams/commit/9074def3))
- **Team Worker room boundary convergence**: Remove Manager again after standalone Worker infrastructure reconciliation restores regular Team Worker personal-room membership. ([b5b0add](https://github.com/agentscope-ai/AgentTeams/commit/b5b0add))
- **Team Worker reference enforcement**: Keep referenced Worker CRs protected during direct deletion and reject Team API members whose required role is empty. ([d96f1ed](https://github.com/agentscope-ai/AgentTeams/commit/d96f1ed))
- **Team Worker room membership**: Force Manager out of regular Team Worker personal rooms when equal Matrix power levels prevent a normal kick. ([43545c2](https://github.com/agentscope-ai/AgentTeams/commit/43545c2))
- **CoPaw Team assignment localparts**: Route Team Leader assignments that mention a Team Worker by Matrix localpart from Leader DM to Team Room. ([973e291](https://github.com/agentscope-ai/AgentTeams/commit/973e291))
- **CoPaw Team coordination routing**: Route Team Leader worker assignments sent through the `message` tool from Leader DM to Team Room, matching the Matrix channel send path. ([92c8145](https://github.com/agentscope-ai/AgentTeams/commit/92c8145))
- **Pinned OpenClaw source fetch**: Fetch the pinned OpenClaw commit directly so the base image build does not depend on a retired-brand external branch name. ([b0081c2](https://github.com/agentscope-ai/AgentTeams/commit/b0081c2))

**Branding and Compatibility**
