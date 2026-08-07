# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

**Bug Fixes**

- **Worker custom skill loading**: Validate and upload Manager-hosted Worker skill files before updating assignments, avoid recursive CR updates during reconciliation, and sync plus enable assigned skills in the native QwenPaw workspace.

---

**Bug 修复**

- **Worker 自定义 Skill 加载**：更新分配前校验并上传 Manager 托管的 Worker Skill 文件，避免调和期间递归更新 CR，并在 QwenPaw 原生 workspace 中同步、启用已分配 Skill。
