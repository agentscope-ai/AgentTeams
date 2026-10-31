# 外部 S3 兼容存储后端（`storage.provider=s3`）

完整文档见英文版：[`storage-external-s3-backend.md`](../../design/storage-external-s3-backend.md)。
本文件收录中文小节，随英文版增补同步。

## 托管 RustFS（单节点）

也可以不自行运维 S3 后端，而由 chart 直接部署单节点 RustFS
StatefulSet：镜像 tag 随 AgentTeams 发布 pin（与内置 MinIO 同一 pin
策略，`storage.rustfs.image.tag` 默认值与 `hack/mirror-images.sh`
条目始终一致）。

```yaml
# values.yaml
storage:
  provider: s3
  engine: rustfs              # chart 托管 RustFS（要求 provider=s3）
  bucket: agentteams-storage
  rustfs:
    image:
      tag: "1.0.4"            # 随发布 pin
    resources: {}
    persistence:
      size: 10Gi
    # 可选：引用已存在的 Secret（RUSTFS_ACCESS_KEY /
    # RUSTFS_SECRET_KEY），代替 values 内的默认凭据：
    # auth:
    #   existingSecret: my-rustfs-creds
```

controller 会自动接入 chart 内的 RustFS
（`AGENTTEAMS_FS_ENDPOINT=http://<release>-rustfs:9000`，凭据走
secretKeyRef）。`storage.engine=rustfs` 与外部端点互斥：engine 与
`storage.provider=minio`/`oss` 组合、与 `storage.mode=existing` 组合、
或 `storage.s3.endpoint` 非空时渲染直接失败——托管引擎部署自己的集群内
RustFS，端点始终由 chart 内的 StatefulSet 推导。若要指向外部
S3/RustFS，改用 `storage.engine=external`（默认）+
`storage.mode=existing` + `storage.s3.endpoint=<你的端点>`。

**版本 pin。** 与 MinIO 相同：RustFS 镜像 tag 随 AgentTeams 发布 pin。

**升级。** 改 `storage.rustfs.image.tag` 后 `helm upgrade`；数据在
StatefulSet PVC 上，升级保留（升级前确认 RustFS 版本的数据目录
兼容性）。

**回滚。** `helm rollback <release> <revision>` 回退到上一修订版本
（含 RustFS 镜像 tag）。

## 新装 vs. 存量切换

上面列出的后端选择（`minio` / `oss` / `s3` 外部 / `s3` +
`engine=rustfs`）定义的是**全新安装**部署什么，它们不是迁移机制。

对已运行的安装修改 `storage.provider`（或 `storage.engine`）**不是数据
迁移**：对象仍留在旧后端，新后端从空开始，切换窗口内的所有读写都会
中断。切换前需要自行搬完对象，再改 values。最小步骤：

1. 把每个 bucket 全量拷贝到目标（`mc mirror --overwrite --remove
   src/<bucket> dst/<bucket>`，逐 bucket 执行）。
2. 停写（缩容 Worker / 维护窗口）。
3. 修改 values（`storage.provider`、`storage.engine`、endpoint、
   凭据）并执行 `helm upgrade`。
4. 验证：controller 日志显示新的 provider，且一个 Worker 能通过新
   后端完成一次文件读写。

本 PR 不实现、也不测试切换本身——它不在本 PR 范围内。包含对账和回滚
的完整操作指南见英文版下方的「Migrating an embedded MinIO deployment
to an external backend」一节。

## 本地安装器边界

本地（embedded）安装器目前固定内置 MinIO，没有 RustFS 或外部 S3
选项，本 PR（仅 chart / Helm）不改变这一点。本地外部 S3 由 #1464
（open）承接：安装器将尊重 controller 本来就认的同一组
`AGENTTEAMS_FS_*` 变量。本地 RustFS 选择流程（安装时可选内置
RustFS 容器）是**提议中**的后续 PR——尚未实现，本地安装今天无法
选择 RustFS。

## 验收

对应 review 清单的证据，范围以本轮实际执行过的为准（2026-10-31，
Raspberry Pi 5，aarch64，Docker，RustFS
`docker.io/rustfs/rustfs:1.0.4`）：

- **原后端兼容。** 默认 `minio + managed` 渲染路径无 RustFS 资源
  （`tests/check-helm-agentteams.sh` 断言），本轮 chart 改动不触及
  默认 profile 渲染的任何模板；`oss + existing` 与 `s3 + existing`
  渲染路径由同一 helm 测试覆盖——18 项场景断言全 PASS，含新增的
  托管 RustFS 安全上下文断言（RustFS StatefulSet 上的
  `fsGroup: 10001`、`fsGroupChangePolicy: OnRootMismatch`、
  `runAsUser: 10001`、`runAsGroup: 10001`、`runAsNonRoot: true`、
  `allowPrivilegeEscalation: false`）、默认上游镜像仓库断言与
  fsGroup 覆盖透传断言。
- **RustFS 卷权限。** RustFS 1.0.4 以 uid/gid 10001 运行
  （`USER=rustfs`），镜像内 `/data` 为 `drwxr-x--- rustfs rustfs`。
  Pi 实测：(i) 属主继承自镜像的新鲜卷正常启动——
  `/minio/health/live` = 200，建桶与对象读写 OK；(ii) root 属主
  `0755` 目录作数据卷（即未应用 `fsGroup` 时的默认 PVC 形态）
  启动即 FATAL：`Server runtime failed: Io error: Permission denied
  (os error 13)`。因此 chart 以非 root 运行 StatefulSet
  （`runAsUser`/`runAsGroup` 10001、`runAsNonRoot: true`）并设
  `podSecurityContext.fsGroup: 10001` +
  `fsGroupChangePolicy: OnRootMismatch`，让 kubelet 对 PVC 应用组
  属主（且不在每次启动递归 chown 大卷）。该修复在渲染层验证
  （helm 断言）；真实集群 PVC 仍待 k8s 验收。
- **管理 API / 用户策略。** Pi 实机（1.0.4）：`mc admin user add`、
  `mc admin policy create`、`mc admin policy attach`、
  `mc admin user list` 全部通过。
- **文件访问。** Pi 实机（1.0.4）：`mc cp` / `mc cat` 15 字节对象
  往返内容一致。controller 侧文件访问沿用既有 Pi 证据（controller
  S3 读写 + Worker FileSync 单写 RustFS，1.0.4 上一轮）。
- **重启持久化。** Pi 上 `docker restart` 后对象仍在（15 B，
  STANDARD）。chart 形态下 StatefulSet PVC 跨 `helm upgrade` /
  `helm rollback` 保留，RustFS 数据目录不落镜像（容器内 `/data`
  即卷挂载）。

**未验（如实列出）。**

- 真实 Kubernetes 集群端到端：`helm install` → PVC 供应 → Pod 调度
  → controller wiring 全链路；上述卷权限修复仅在渲染层断言。
- 多节点 RustFS。
- NFS 生产形态下的 RustFS（见 RustFS 文档，按风险对待）。
