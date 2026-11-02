# 使用 Atlas Cloud 作为 LLM 服务

Atlas Cloud 提供 OpenAI-compatible 文本 API，AgentTeams 通过现有通用路径即可接入，无需专用 provider 类型、预设或适配服务。

| 配置项 | 取值 |
|---|---|
| Provider 类型 | `openai-compat` |
| Base URL | `https://api.atlascloud.ai/v1`（API 根路径，不含 `/chat/completions`） |
| API Key | 你的 Atlas Cloud API Key |
| 模型 | `GET https://api.atlascloud.ai/v1/models` 返回的准确模型 ID（例如 `deepseek-ai/deepseek-v4.1-flash`） |

网关上的模型 ID 会变化，配置前请先对照实时模型列表确认：

```bash
curl -s https://api.atlascloud.ai/v1/models | grep -o '"id":"[^"]*"' | head
```

## 配置

### Kubernetes（Helm）

```yaml
credentials:
  llmApiKey: "<your-atlascloud-api-key>"
  llmProvider: openai-compat
  llmBaseUrl: https://api.atlascloud.ai/v1
  defaultModel: <exact-model-id>
```

### 本地安装器

```bash
AGENTTEAMS_LLM_PROVIDER=openai-compat \
AGENTTEAMS_OPENAI_BASE_URL=https://api.atlascloud.ai/v1 \
AGENTTEAMS_LLM_API_KEY=<your-atlascloud-api-key> \
AGENTTEAMS_DEFAULT_MODEL=<exact-model-id> \
./agentteams-install.sh
```

## 验收

验收分为相互独立的几个层级，通过前一级不代表通过后一级。

### 第 1 级：基础连通性（Helm LLM 预检所覆盖的范围）

Helm 预检 Job 使用已配置的 Key、Base URL 和模型发送一次最小的非流式 `/chat/completions` 请求，只确认 Key 有效、地址可达、模型 ID 存在，不检查流式输出或工具调用。可手动复现：

```bash
export ATLASCLOUD_API_KEY=<your-atlascloud-api-key>
curl -s https://api.atlascloud.ai/v1/chat/completions \
  -H "Authorization: Bearer $ATLASCLOUD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<exact-model-id>","messages":[{"role":"user","content":"Reply with: ok"}],"max_tokens":64}'
```

预期的失败表现：Key 无效返回 HTTP 401，模型 ID 不存在返回 HTTP 400 `not found`。

### 第 2 级：模型能力检查（让 Agent 依赖该模型之前必做）

Agent 依赖流式输出和工具调用，连通性检查不覆盖这两项。请针对准备使用的模型直接对网关验证。

流式：在上面的请求中加入 `"stream": true`，应收到以 `data: [DONE]` 结尾的 `data: {...}` 分片。

工具调用：发送带 `tools` 的请求，确认响应的 `finish_reason` 为 `tool_calls`，且 `tool_calls[0].function` 的名称和 JSON 参数符合预期；再以 `role: "tool"` 消息回传工具结果，确认模型给出最终回答。

```bash
curl -s https://api.atlascloud.ai/v1/chat/completions \
  -H "Authorization: Bearer $ATLASCLOUD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<exact-model-id>",
       "messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],
       "tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather for a city",
         "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
       "max_tokens":256}'
```

能力因模型而异。更换 `defaultModel` 或为 Worker 指定其他模型时需重新检查。

### 第 3 级：端到端 Agent 验证

上述直连网关的检查不能证明完整的 AgentTeams 链路（Higress 路由、Manager、Worker 运行时）。安装完成后，请让 Manager 执行一个会调用工具并创建 Worker 的任务，确认其能完成。

## 验证记录

2026-11-02，使用 `deepseek-ai/deepseek-v4.1-flash`（当天 `/v1/models` 中在列），直接对 `https://api.atlascloud.ai/v1` 执行了第 1、2 级的请求：

| 检查项 | 结果 |
|---|---|
| 非流式对话补全 | HTTP 200，`finish_reason: stop` |
| 未知模型 ID／无效 Key | HTTP 400 `not found`／HTTP 401 |
| 流式输出 | HTTP 200，24 个分片，以 `[DONE]` 结束 |
| 工具调用（非流式） | `finish_reason: tool_calls`，`get_weather`，参数 `{"city": "Paris"}` |
| 工具调用（流式） | 从增量中拼出名称和参数，`finish_reason: tool_calls` |
| 工具结果回传 | HTTP 200，基于工具结果给出最终回答 |

未验证：使用这些配置对 AgentTeams 进行 Helm 安装或本地安装、Higress 路由、任何 Manager/Worker 任务（第 3 级）。

注意：网关会拒绝 `logprobs` 请求（HTTP 400）；`reasoning_effort` 除 `high`/`xhigh` 外只有部分模型支持。未经检查请勿发送这些参数。
