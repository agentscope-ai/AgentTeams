# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**Bug Fixes**

- **QwenPaw Worker model capability propagation**: Project the model `input` modalities into `runtime.yaml` and register the gateway provider with explicit `supports_multimodal`/`supports_image`/`supports_video` flags and `probe_source=manual` on the QwenPaw worker. This prevents QwenPaw's self-probe from overwriting controller-injected multimodal capability for reasoning models (whose `reasoning_content` exhausts the probe's token budget, failing the semantic vision check). ([#931](https://github.com/agentscope-ai/AgentTeams/issues/931))
- **Worker custom skill loading**: Validate and upload Manager-hosted Worker skill files before updating assignments, grant Managers access to Worker storage prefixes, avoid recursive CR updates during reconciliation, keep local/default MinIO paths off unavailable STS, restore Manager MinIO access after Controller restarts, verify uploaded `SKILL.md` files, restore Worker file-sync notifications, and sync plus enable assigned skills in the native QwenPaw workspace.
- **QwenPaw Manager custom skill hot loading**: Continuously mirror workspace skills into the native QwenPaw workspace, then refresh and enable additions or updates without restarting the Manager.
- **QwenPaw Manager Skill attachments**: Initialize the Matrix attachment directory at startup so an admin can send a complete Worker Skill ZIP to the Manager for validation and distribution.
- **Team Room membership convergence**: Explicitly join Team Leaders and Workers with their own Matrix tokens after invitation, so pending invites cannot leave a Team Active while Hermes members remain unreachable. ([41f6e517](https://github.com/agentscope-ai/AgentTeams/commit/41f6e5175a6fb8bf1c7ee19632d3d2fac6f66f26), [#1142](https://github.com/agentscope-ai/AgentTeams/issues/1142))

---

**Bug 修复**

- **QwenPaw Worker 模型能力传递**：将模型 `input` 模态投影到 `runtime.yaml`，并在 QwenPaw worker 注册网关 provider 时显式传递 `supports_multimodal`/`supports_image`/`supports_video` 标志和 `probe_source=manual`。这防止 QwenPaw 自探测覆盖 Controller 注入的多模态能力——推理模型（`reasoning_content` 耗尽探测 token 预算、语义视觉校验失败）自探测必然失败。([#931](https://github.com/agentscope-ai/AgentTeams/issues/931))
- **Worker 自定义 Skill 加载**：更新分配前校验并上传 Manager 托管的 Worker Skill 文件，授予 Manager 对 Worker 存储前缀的访问权限，避免调和期间递归更新 CR，避免本地或默认 MinIO 路径误请求不可用的 STS，Controller 重启后恢复 Manager 的 MinIO 访问并校验已上传的 `SKILL.md`，恢复 Worker 的 file-sync 通知，并在 QwenPaw 原生 workspace 中同步、启用已分配 Skill。
- **QwenPaw Manager 自定义 Skill 热加载**：持续将 workspace Skill 投影到 QwenPaw 原生 workspace，并自动刷新、启用新增或更新的 Skill，无需重启 Manager。
- **QwenPaw Manager Skill 附件**：启动时创建 Matrix 附件目录，使管理员可以将完整的 Worker Skill ZIP 发送给 Manager，由其校验并分发。
- **Team Room 成员收敛**：邀请 Team Leader 和 Worker 后，使用各自的 Matrix token 显式加入 Team Room，避免 Team 已处于 Active 状态时 Hermes 成员仍停留在 invite、无法接收消息。([41f6e517](https://github.com/agentscope-ai/AgentTeams/commit/41f6e5175a6fb8bf1c7ee19632d3d2fac6f66f26), [#1142](https://github.com/agentscope-ai/AgentTeams/issues/1142))
