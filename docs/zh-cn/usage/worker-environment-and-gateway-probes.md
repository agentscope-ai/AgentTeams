# Worker 环境变量与网关连通性探针

管理员和 Manager 调用方可在 Worker 的 create / update 请求中设置 `env`：

```json
{"env":{"MY_SERVICE_URL":"https://example.com","MY_FEATURE_ENABLED":"true"}}
```

更新时：省略 `env` 保留原值，发送 `{}` 清空用户变量，发送完整对象则整体替换。值统一为字符串，包括空串和空白。名称必须匹配 `[A-Za-z_][A-Za-z0-9_]*`；值不得包含 NUL。API 拒绝与系统环境冲突的键。已有的直接 CR 用户沿用 reconciler 的“系统胜出”行为。

环境值属于管理员级配置，不是机密保险箱。Worker 读接口只返回 `envEditable`，仅 Admin/Manager 调用方能收到完整 `env`。切勿把凭据放进截图或日志。Scoped 用户既不能写入环境变量，也不能通过 Worker 响应读取它们的值。

环境变更复用现有的容器规格协调流程：受管的 Docker 容器会重建，可能打断当前任务；持久化工作区数据保留。API 接受更新并不代表替换容器已就绪。远端、非受管的 Worker 需由其操作员单独更新进程环境。

## 网关连通性探针

`POST /api/v1/workers/{name}/gateway-probe` 接受以下任一请求体：

```json
{"kind":"model"}
```

```json
{"kind":"mcp","server":"github"}
```

需要 Admin/Manager 访问权限和网关更新授权。处理函数会重新加载已保存的 Worker 规格和已持久化的 Consumer 凭据；不接受调用方提供的 URL、凭据、模型覆盖、提示词或工具调用，也不会修改授权。

- **Model**：用已保存的显式模型发送一次非流式 `chat/completions` 请求，请求最多 8 个输出 token，可能产生少量推理费用。它只检查模型选项是否返回，不返回生成的内容。
- **MCP**：在已配置网关的 `/mcp-servers/` 路径下，使用已保存的 Streamable HTTP 端点，初始化协议并用 Worker 凭据列出可用工具，但不会真正执行任何工具调用。Legacy SSE 端点和任意外部 MCP 主机不在本探针的覆盖范围内；上游 SSE 服务可经由 Streamable HTTP 网关端点代理访问。

请求有 20 秒截止时间、有界的响应读取、不跟随重定向。不会返回上游的错误体和凭据。结果只标识 Worker Consumer 与本次网关请求是否通过，不说明运行中的 Worker 任务或其本地网络是否成功。供应商特定的网关端点会报告为不支持，而不是错误地探测部署默认网关。

被拒调用时，检查目标路由的 Consumer 白名单与上游配置。切勿为让探针通过而关闭认证或允许所有 Worker 访问。
