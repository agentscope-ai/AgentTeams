# OpenClaw patch series

AgentTeams-specific patches applied on top of the pinned OpenClaw tag
(`OPENCLAW_TAG` in `hack/versions.env`, fetched from the fork configured by
`OPENCLAW_REPO_URL` in `openclaw-base/Dockerfile`).

- Apply **in order** `0001` → `0008` (the series is one logical change set
  split by concern; `git apply` order matters).
- Verify against a checkout: `hack/check-openclaw-patches.sh --repo <checkout>`.
- Every post-patch file was verified byte-for-byte against the GitHub blob
  sha of the verified running build (2026-10-28); the base blob shas below
  are recorded from the same verification pass. Six of the ten modified
  base files only have short (7-hex) base shas recorded — the full sha was
  not captured at verification time; the post shas are full 40-hex.

| Patch | Files | Base blob sha | Post blob sha | Concern |
| --- | --- | --- | --- | --- |
| 0001-matrix-debug-trace | `extensions/matrix/src/matrix/debug-trace.ts` (new) | — | `1b0ba5f4bfcf357c42aa1c9bc138351f88ac387e` | Matrix debug trace helper |
| 0002-matrix-shared-client | `extensions/matrix/src/matrix/client/shared.ts` | `ff13bb0e6c9f8f16634edbc8dcd6d1e8e693ad51` | `7e9ed9be9537cad38e6626f2ee9c68241dbd0b9f` | Shared client state |
| 0003-matrix-private-network-guard | `extensions/matrix/src/matrix/client/private-network-host.ts` | `d8d96b1…` | `b1f1d3707768e49f3c077a1b15315c61370c0e14` | Private-network host guard |
| 0004-matrix-history-context | `src/auto-reply/reply/history.ts` | `ab75f86534d0d6bbd099a5d90eb28fb93e27a808` | `1c592c1f6f096b8369356b99b79b14ef015daddd` | Reply history context |
| 0005-matrix-mentions | `extensions/matrix/src/matrix/monitor/mentions.ts`, `…/mentions.test.ts`, `…/monitor/index.ts` | `201bb70…` / `14aae68…` / `cb07f09…` | `f09461a3aac57df0342d7e24596386417f8239ab` / `b65759befc5839e6e518adea4ed022f99486f486` / `9c5089469529bab8daf156da7728e40371d10f48` | Mention monitoring (raw-ID hot-reload, startup grace) |
| 0006-matrix-config-reload-plan | `src/gateway/config-reload-plan.ts` | `1ab4dac66a7b5aee3fe2a655f13f5007a865094c` | `47f82076a67c8835c4a388b0a16ac0ac0beb96cf` | Config reload plan |
| 0007-matrix-client-bootstrap-and-channel | `extensions/matrix/src/matrix/client-bootstrap.ts`, `extensions/matrix/src/channel.ts` | `28e72fd72745140fa51c11e6c41da778162270c5` / `2e28612…` | `e07d40fd5b49f6ece680120419ca536ca28be682` / `220e082cb2510c0a71cd9f16264f1e6b0185aecb` | Runtime client resolution + channel wiring |
| 0008-matrix-tests | `extensions/matrix/src/matrix/client.test.ts` | `a27362d…` | `47e2ab5619dbbb3754fd44b8884ec5c3ac3a4c35` | Tests for the above |

`openclaw-base/Dockerfile` copies this directory in as a named build
context (`openclaw-patches`) and applies the series in a `RUN` step —
any `git apply --check` failure fails the image build. The resulting tree
is committed in the image's git (`"agentteams patch series"`) so later
stages (P7) can rebase against the patched tree.

## OpenClaw 补丁序列

AgentTeams 在固定 OpenClaw tag（`hack/versions.env` 的 `OPENCLAW_TAG`，
来自 `openclaw-base/Dockerfile` 中 `OPENCLAW_REPO_URL` 配置的 fork）之上
应用的一组补丁。

- 必须按 `0001` → `0008` 顺序应用（同一逻辑变更集按关注点拆分，
  `git apply` 顺序敏感）。
- 在本地 checkout 上验证：`hack/check-openclaw-patches.sh --repo <checkout>`。
- 所有补丁后文件已与验证过的在跑构建的 GitHub blob sha 逐字节对齐
  （2026-10-28 验证）；下表 base blob sha 取自同一次验证——其中 10 个
  被改文件里有 6 个只留下了短 sha（7 位 hex，验证当时未记录全量），
  post sha 均为完整 40 位。

| 补丁 | 覆盖文件 | base blob sha | post blob sha | 关注点 |
| --- | --- | --- | --- | --- |
| 0001-matrix-debug-trace | `extensions/matrix/src/matrix/debug-trace.ts`（新文件） | — | `1b0ba5f4bfcf357c42aa1c9bc138351f88ac387e` | Matrix 调试跟踪辅助 |
| 0002-matrix-shared-client | `extensions/matrix/src/matrix/client/shared.ts` | `ff13bb0e6c9f8f16634edbc8dcd6d1e8e693ad51` | `7e9ed9be9537cad38e6626f2ee9c68241dbd0b9f` | 客户端共享状态 |
| 0003-matrix-private-network-guard | `extensions/matrix/src/matrix/client/private-network-host.ts` | `d8d96b1…` | `b1f1d3707768e49f3c077a1b15315c61370c0e14` | 私网 host 防护 |
| 0004-matrix-history-context | `src/auto-reply/reply/history.ts` | `ab75f86534d0d6bbd099a5d90eb28fb93e27a808` | `1c592c1f6f096b8369356b99b79b14ef015daddd` | 回复历史上下文 |
| 0005-matrix-mentions | `extensions/matrix/src/matrix/monitor/mentions.ts`、`…/mentions.test.ts`、`…/monitor/index.ts` | `201bb70…` / `14aae68…` / `cb07f09…` | `f09461a3aac57df0342d7e24596386417f8239ab` / `b65759befc5839e6e518adea4ed022f99486f486` / `9c5089469529bab8daf156da7728e40371d10f48` | 提及监听（raw-ID 热重载、启动宽限） |
| 0006-matrix-config-reload-plan | `src/gateway/config-reload-plan.ts` | `1ab4dac66a7b5aee3fe2a655f13f5007a865094c` | `47f82076a67c8835c4a388b0a16ac0ac0beb96cf` | 配置重载计划 |
| 0007-matrix-client-bootstrap-and-channel | `extensions/matrix/src/matrix/client-bootstrap.ts`、`extensions/matrix/src/channel.ts` | `28e72fd72745140fa51c11e6c41da778162270c5` / `2e28612…` | `e07d40fd5b49f6ece680120419ca536ca28be682` / `220e082cb2510c0a71cd9f16264f1e6b0185aecb` | 运行时客户端解析 + channel 接线 |
| 0008-matrix-tests | `extensions/matrix/src/matrix/client.test.ts` | `a27362d…` | `47e2ab5619dbbb3754fd44b8884ec5c3ac3a4c35` | 上述改动对应测试 |

`openclaw-base/Dockerfile` 以 named build context（`openclaw-patches`）
把本目录拷进镜像构建，并在 `RUN` 步骤中顺序应用——任何
`git apply --check` 失败都会让镜像构建失败。补丁后的树会在镜像内 git
提交（`"agentteams patch series"`），供后续阶段（P7）对打过补丁的树做
rebase。
