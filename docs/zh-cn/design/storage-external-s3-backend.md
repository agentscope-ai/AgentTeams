# 外部 S3 兼容存储后端（`storage.provider=s3`）

AgentTeams 可以使用外部 S3 兼容对象存储作为存储后端，替代内嵌 MinIO。Chart 默认仍是 MinIO；本文覆盖可选的 `s3` provider，目前以 **RustFS 1.0.3** 实测（见[兼容性](#已验证兼容性)）。

> 本文是英文原文 [`docs/design/storage-external-s3-backend.md`](../../design/storage-external-s3-backend.md) 的中文镜像（按仓库 `docs/zh-cn/` 镜像惯例压缩翻译，章节与原文一一对应）；两文冲突时以英文原文为准。

## `provider=s3` 是什么

| 方面 | `minio`（默认） | `oss`（外部云） | `s3`（外部自建） |
|---|---|---|---|
| 引擎生命周期 | 本 chart 内的 StatefulSet | 外部、云托管 | 外部、由你运维 |
| 凭据 | 静态 root（chart secret） | credential-provider sidecar 的 STS | 固定 root（chart secret 或 existingSecret） |
| 建桶 | controller（EnsureBucket） | 外部 | **controller（EnsureBucket）** |
| worker 用户/策略 | controller 经 `mc admin` | 外部 | **controller 经 `mc admin`** |
| 凭据 sidecar | 无 | 有 | 无 |

后端必须同时支持 **S3** 与 **MinIO 兼容的 admin API**（`mc admin user/policy`）：controller 用镜像内置的 `mc` 管理每个 worker 的凭据。RustFS 两者都实现了（已实测；注意 RustFS 的重复操作是静默幂等而非报错，controller 的幂等 admin 客户端对两种风格都兼容）。

## 已验证兼容性

本文结论只覆盖真正跑过的版本与部署模式：

| 组件 | 版本 | 说明 |
|---|---|---|
| AgentTeams | `main` @ `6ede5977`（v1.2.5）+ 本 PR | K8s chart 安装 |
| `mc`（controller 内置 / 测试用） | `RELEASE.2025-08-13T08-35-41Z`（commit `7394ce0d`） | 与 controller 镜像同款 |
| RustFS | `1.0.3`（`rustfs/rustfs:1.0.3`，amd64/arm64） | 单节点本地盘；单节点 NFS loopback |
| MinIO（迁移源） | `RELEASE.2025-09-07` | 仅作迁移源 |
| 测试环境 | Raspberry Pi 5（arm64）、Docker | 完整 migrate→verify→cutover E2E：200 MiB / 100 对象 / 3 用户 / 7 策略 |

未验证：多节点 RustFS、生产模式下的 RustFS-on-NFS（按风险对待）、其它 S3 引擎（须先通过[可复现测试](#测试)）。

## 如何启用

```yaml
# values.yaml
storage:
  provider: s3
  mode: existing
  bucket: agentteams-storage
  s3:
    endpoint: "http://rustfs:9000"      # controller pod 可达
    # 推荐：引用已有 Secret
    existingSecret: my-rustfs-creds
    accessKeySecretKey: accesskey
    secretKeySecretKey: secretkey
    # 备选（仅开发）：内联凭据，由 chart 渲染 secret
    # accessKey: "rustfsadmin"
    # secretKey: "rustfsadmin"
```

`helm install` 在渲染期校验组合（`provider=s3` 要求 `mode=existing`、endpoint 与凭据）。此模式下 MinIO StatefulSet 不再渲染，也不部署 credential-provider sidecar。

## 测试

三层可复现测试：

1. **controller admin 生命周期单测**（CI，无需真实引擎）：`agentteams-controller/internal/oss/minio_admin_backend_test.go` 用 fake `mc` 驱动真实 `MinIOAdminClient`，覆盖两种引擎行为（MinIO 报错式幂等 / RustFS 静默式幂等），断言 `mc admin` 调用序列、alias 复用、策略文档形状与拆除顺序。`cd agentteams-controller && go test ./internal/oss/`
2. **Helm 渲染测试**（CI）：`tests/check-helm-agentteams.sh` 覆盖 s3 渲染（endpoint/secret/env 接线、无 MinIO StatefulSet、无 sidecar、`existingSecret` + 自定义 key 名）与失败模式（缺 endpoint / 缺凭据 / `mode=managed`）。
3. **真实引擎端到端**：`test/e2e/external-s3-backend.sh` 起 MinIO（源）与 RustFS（目标）容器，跑完整 S3 操作矩阵 + 两引擎上的 `mc admin` 用户/策略生命周期，并打印被测版本矩阵。需要 Docker，用法见脚本头。

## 从内嵌 MinIO 迁移到外部后端

> 本节是"把已有 AgentTeams 安装从内嵌 MinIO 迁到外部 S3（如 RustFS）"的运维指南，覆盖**最终数据对账**与**目标端已开始写入后的回滚**。

### 阶段总览

```
0. 冻结窗口      停 worker 写入（缩容 / 维护模式）
1. 批量拷贝      源 → 目标全量对象（mc mirror）；记录基线清单
2. IAM 重放      在目标重建用户/策略（mc admin）
3. 影子          让 worker 指向目标重新开工；源保持只读
4. 影子对账      只读：源相对基线未变；目标 = 基线 + 影子增量
5. cutover       把 AgentTeams 切到 provider=s3 —— 禁止整桶源→目标覆盖
6. （出问题）    回滚 —— 见下
```

**唯一必须记住的规则**：影子期一开始，目标端就是写入权威、源端只读。此后再做整桶 `mc mirror --overwrite src dst` 就是数据丢失操作——它会把 worker 写到目标的新内容全部回退成源端旧内容。因此正确性以**批量拷贝刚完成时记录的基线清单**为准，而不是"源等于目标"。

### 自动化迁移工具

[`scripts/storage-migrate/`](../../../scripts/storage-migrate/README.md) 把上述阶段实现为可续跑状态机（`preflight → data → IAM replay → verify → cutover`），含基线清单、影子增量门与回滚清单；支持三种数据模式（`mirror`、RustFS `odm`、`inplace`），`verify` 不全绿时拒绝 `cutover`。

```bash
bash scripts/storage-migrate/storage-migrate.sh --src-endpoint … --dst-endpoint … \
  --mode mirror --state-dir /var/lib/storage-migrate --dry-run
```

工具**从不自行改动生产**：`--phase cutover` 只生成给人执行的清单（见[Cutover 操作手册](#5-cutover-操作手册)），`--mark-cutover-done` 只记录操作者的**确认**——不校验重启、不校验配置。宿主机没有 `mc`/`jq`/`curl` 时用 runner 镜像（`scripts/storage-migrate/Dockerfile.runner`）。

### 1. 批量拷贝

```sh
mc mirror --overwrite --remove "src/agents"  "dst/agents"
mc mirror --overwrite --remove "src/shared"  "dst/shared"
# … 每个桶一条（agents / shared / teams / agentteams-config，或你部署的桶列表）
```

### 2. IAM 重放

controller 为每个 Worker 建一个用户 + 一个受限策略（`worker-<name>`，按团队/角色限定）。在目标端按同样语义重放：

```sh
mc admin user add dst <worker> <password>
mc admin policy create dst worker-<worker> policy.json
mc admin policy attach dst worker-<worker> --user <worker>
```

若随新版 chart 一起迁移，也可以交给 controller：切换 endpoint 后，首次 reconcile 的 `EnsureUser`/`EnsurePolicy` 会重建（两种引擎行为下都幂等）。预重放的好处是 cutover 前目标端已可验证。

### 3–4. 影子与影子对账

让 worker 指向目标运行，源端保持**只读**。

批量拷贝后立刻记录**基线清单**——它是之后每一步的对账基准：

```sh
mc ls --recursive --json src/<bucket> | sort > /tmp/baseline-<bucket>.jsonl
```

影子期内记录**影子增量**（Gate B、cutover 检查与回滚检查都以它为准）：

- **created** —— 目标上有、基线里没有的 key；
- **updated** —— 基线里存在但目标内容已变的 key（保留新值；仅比大小不足以证明内容）；
- **deleted** —— 基线里存在、目标上已消失的 key（影子期的有意删除，不是门失败）。

**Gate A —— 源端没变**：源必须与基线逐字一致，否则说明冻结被破，需要逐键显式对账。

```sh
mc ls --recursive --json src/<bucket> | sort > /tmp/now-<bucket>.jsonl
diff /tmp/baseline-<bucket>.jsonl /tmp/now-<bucket>.jsonl   # 必须为空
```

**Gate B —— 目标 = 基线 + 已记录增量**：不要要求"每个基线 key 都还在且大小不变"（影子里删掉的就是该没有；更新过的就是新内容新大小）。按记录核对：

```sh
# 1. 未被影子删除的基线 key 仍在目标
comm -23 /tmp/base-keys-<bucket>.txt /tmp/shadow-deleted-<bucket>.txt \
  | comm -23 - /tmp/dst-keys-<bucket>.txt          # 必须为空
# 2. 未被触碰的基线对象两端逐字节一致（读原始字节再 cmp；不要用 $(mc cat …)，它会剥掉末尾换行）
# 3. 增量成立：created 存在、updated 仍是影子内容、deleted 保持缺席（不得被源端复活）
# 4. 基线 ∪ created 之外没有新 key
```

此阶段**预期**的差异就是上面三类（created / updated / deleted）；**不要**用源端覆盖"修正"它们——把影子期的删除从源端拷回来，正是 Gate B 禁止的数据复活。

### 5. Cutover 操作手册

cutover 是**人工执行**的时序。迁移工具从不冻结写入、不改部署配置、不重启消费者：`--phase cutover` 只跑最终增量、打双端写探针，并生成下面九步的 `cutover-checklist.sh`（切换前可用 `--print-checklist --deploy-mode embedded|helm|both` 只读预览，不需要状态也不需要 mc）。每步标注 `[tool]`（工具已做）/ `[manual]`（人工执行）/ `[verify]`（核验证据）。

**为什么运行中的消费者保留旧 endpoint。** controller 在创建 Manager/Worker 容器时注入 `AGENTTEAMS_FS_ENDPOINT`/`FS_BUCKET`/`STORAGE_PREFIX` 与 per-worker 凭据；容器/pod 的 env 是**创建时固化**的，`restart` 不会重读。重建判定 `hashAppliedWorkerSpec` 只哈希成员 CR spec——不含 controller 自身的存储配置，也没有配置代次——所以只改 controller 配置永远不会重建已运行成员，会留下混用后端。只有删除成员容器/pod 才会让 controller 按当前配置重建。这就是本手册在切配置后**重建每个消费者**、而不是"改 endpoint 再 re-apply"的原因。

embedded 模式下冻结顺序很重要：内嵌 MinIO 在 controller 容器里，所以必须**先冻结成员、跑完最终增量，再动 controller**（controller 停了源就停了）。

1. `[tool]` 最终增量 + 校验——工具运行已完成（见 `report-cutover.json`）。RustFS 目标在 cutover 后保持 ODM 源回退关闭（`RUSTFS_ON_DEMAND_MIGRATION_ENABLED=false`）。
2. `[manual]` 冻结全部写入方——外部客户端 → 成员 → controller。
3. `[manual]` 一次性改齐部署配置（provider + endpoint + bucket + 凭据）。
4. `[manual]` 重建每个消费者（env 创建时固化；restart 不重读）。
5. `[manual]` 逐组件读写 + 权限检查（含 403 负测）。
6. `[manual]` 恢复流量。
7. `[manual]` 影子期源端保持只读。
8. `[manual]` 回滚（保留目标端写入，见下节）。
9. `[manual]` 记录完成：`--mark-cutover-done` 只记录操作者**确认**——不校验重启、配置或任何对象。

### 目标端已开始写入后的回滚

风险：worker 一旦写入目标，目标就持有源端没有的对象；简单"把 endpoint 切回去"会丢这些写入。

步骤（在源端下一次备份过期前执行则 RPO=0；RTO ≈ 反向增量同步 + cutover）：

1. **停目标写入**（冻结 worker；缩到 0）。
2. **反向增量同步**——把目标端有的一切拷回源端。因影子期源端一直只读（Gate A），反向集合正好是批量拷贝后目标端的 created/updated **以及删除**（含影子期，不只是 cutover 之后）：

   ```sh
   mc mirror --overwrite --remove dst/<bucket> src/<bucket>   # 每个桶
   ```

   `--remove` 负责把目标端删除带回源端；它**只有在 Gate A 为空时安全**。若 Gate A 非空，去掉 `--remove`，先逐键对账、再显式删除受影响源 key。

   （RustFS on-demand-migration 这类单向导入不适用于反向；反向只支持 `mc mirror`。）
3. **源对目标对账**——反向同步后源须与目标逐对象一致（数量/大小/抽样校验和）。与原始基线的差异是预期的：影子增量现在落在源端。
4. **切回**：`storage.provider=minio`，`helm upgrade`，验证一个 worker 往返读写。
5. 目标端保持冻结（只读）供排查；重试时再清空或重新同步。

**目标端还没写入时**的回滚更简单：直接切回 endpoint、丢弃目标状态即可——源端从未被动过。

### Cutover 后的保留期

旧 MinIO（停止、数据保留、镜像本地留档）至少保留一个完整备份周期 + 两个发布窗口；下线前做最后一次对账快照并归档数据目录。
