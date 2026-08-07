# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**Bug Fixes**

- **Worker custom skill loading**: Validate and upload Manager-hosted Worker skill files before updating assignments, grant Managers access to Worker storage prefixes, avoid recursive CR updates during reconciliation, keep local/default MinIO paths off unavailable STS, restore Manager MinIO access after Controller restarts, verify uploaded `SKILL.md` files, restore Worker file-sync notifications, and sync plus enable assigned skills in the native QwenPaw workspace.
- **QwenPaw Manager custom skill hot loading**: Continuously mirror workspace skills into the native QwenPaw workspace, then refresh and enable additions or updates without restarting the Manager.
- **QwenPaw Manager Skill attachments**: Initialize the Matrix attachment directory at startup so an admin can send a complete Worker Skill ZIP to the Manager for validation and distribution.

---

**Bug 修复**

- **Worker 自定义 Skill 加载**：更新分配前校验并上传 Manager 托管的 Worker Skill 文件，授予 Manager 对 Worker 存储前缀的访问权限，避免调和期间递归更新 CR，避免本地或默认 MinIO 路径误请求不可用的 STS，Controller 重启后恢复 Manager 的 MinIO 访问并校验已上传的 `SKILL.md`，恢复 Worker 的 file-sync 通知，并在 QwenPaw 原生 workspace 中同步、启用已分配 Skill。
- **QwenPaw Manager 自定义 Skill 热加载**：持续将 workspace Skill 投影到 QwenPaw 原生 workspace，并自动刷新、启用新增或更新的 Skill，无需重启 Manager。
- **QwenPaw Manager Skill 附件**：启动时创建 Matrix 附件目录，使管理员可以将完整的 Worker Skill ZIP 发送给 Manager，由其校验并分发。
