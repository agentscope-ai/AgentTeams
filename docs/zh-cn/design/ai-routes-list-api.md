# AI 路由列表 API

状态：已实现
API：`GET /api/v1/gateway/ai-routes`

## 问题

通过 AI 网关对外提供模型的部署环境，缺少一种只读、带内的方式来查看当前配置了哪些 AI 路由、以及哪些消费者在这些路由上获得授权。当前，列取路由需要**网关控制台访问权限（管理员凭据）**，因此仅持有控制器令牌的 API 客户端无法审计路由授权。

## 是路由目录，不是模型目录

AI 路由与模型是两回事，本端点仅报告前者：

- **路由（route）** 是网关的 `/v1` 入口：它承载消费者授权（`authConfig.allowedConsumers`）与上游提供方权重。默认部署恰好创建一条：`default-ai-route`。
- **模型（model）** 是 chat-completion 请求中 `model` 字段的取值（以及 Worker/Manager `spec.model` 中的取值）。模型 ID 由路由的**上游提供方**定义并服务——在默认部署中即 SGLang 实例，其自身的模型列表（如 `/v1/models`）才是有效 ID 的来源。一条路由可以服务多个模型。

因此，路由名**不是**模型别名：在默认部署中，客户端否则会收到 `default-ai-route`，仿佛它是一个模型选项。本端点的命名与形态与此相应（`gateway/ai-routes`，响应键 `routes`），且不对哪些模型名有效作任何断言。

网关自身的 `GET /v1/models` 也**不是**完整的模型列表：ai-proxy 插件只匹配 `chat/completions` 与 `embeddings` 流量，故该端点会漏报。控制台的路由列表与网关的 `/v1/models` 都不是模型目录；模型发现仍归提供方。

## 设计

`GET /api/v1/gateway/ai-routes` 经控制器代理控制台的 AI 路由列表，并为每条路由返回一个条目：

```json
{
  "routes": [
    {
      "name": "default-ai-route",
      "upstreams": [
        { "provider": "sglang-local", "weight": 100 }
      ],
      "allowedConsumers": ["manager", "worker-sysdev-lead"]
    }
  ],
  "total": 1
}
```

- **`name`** —— AI 路由名，原样返回（**不是**模型 ID）。
- **`upstreams`** —— 服务该路由的提供方，含控制台权重。路由无上游时省略。
- **`allowedConsumers`** —— 在该路由上获准的网关消费者（取自路由的 `authConfig`）。为空时省略。

实现说明：

- 控制台列表端点只返回路由名，因此客户端需逐条拉取各路由，以获取其上游与消费者白名单——与消费者授权代码已采用的"先列后取"模式相同。
- 任一路由不可读即令整个调用以 `502` 失败，而不是返回一个静默不完整的目录。
- 无路线表 API 的后端（`ai-gateway` 云供应商）返回 `501`。

## 授权

该路由注册于 `gateway` 资源类型的 `ActionGet`。既有授权矩阵已仅向 admin/manager 授予 `gateway` 类型；团队 leader、团队范围人类与 worker 账户均落入默认拒绝。无需改动授权器。

## 契约

`GET /api/v1/gateway/ai-routes` →

| 结果 | 代码 |
|--------|------|
| 成功 | `200` + 路由目录（可能为空） |
| 网关控制台不可达 / 出错 | `502` |
| 后端不支持路线表，或未配置网关 | `501` |
| 调用方低于 L1 | `403` |
