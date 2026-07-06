# QwenPaw package mcp.json 使用说明

本文面向 AgentSpec package 作者，说明如何在 package 根目录编写
`mcp.json`，让 QwenPaw worker 将 MCP client 加载到默认 agent 的
`agent.json` 中。

## 适用范围

`mcp.json` 只用于 package 内置的 QwenPaw native MCP client。

它不等同于 runtime 下发的 `config/mcporter.json`：

- `mcp.json` 位于 AgentSpec package 根目录，由 QwenPaw worker 合入
  workspace 的 `agent.json`，供 QwenPaw native MCP 管理器加载。
- `config/mcporter.json` 由 controller/runtime config 下发，供
  `mcporter` CLI 使用。
- package 内的 `mcp.json` 不会覆盖 `config/mcporter.json`。

## 标准格式

推荐使用和 `mcporter.json` 对齐的外层结构：

```json
{
  "mcpServers": {
    "server-name": {
      "description": "可选说明",
      "transport": "stdio",
      "command": "npx",
      "args": ["-y", "some-mcp-server"],
      "env": {
        "TOKEN": "$env:SOME_TOKEN"
      }
    }
  }
}
```

`mcpServers` 是一个对象，key 是 MCP client 名称。该名称会成为
`agent.json` 中 `mcp.clients` 的 key。

## 字段说明

| 字段 | 是否必填 | 说明 |
| --- | --- | --- |
| `description` | 否 | 展示给使用者看的说明文本。 |
| `enabled` | 否 | 是否启用；未写时 QwenPaw 默认启用。 |
| `transport` | 否 | `stdio`、`http`、`streamable_http` 或 `sse`。`http` 会按 QwenPaw streamable HTTP 处理。 |
| `command` | stdio 必填 | stdio MCP server 的可执行命令。 |
| `args` | 否 | stdio 命令参数数组。 |
| `env` | 否 | stdio 子进程环境变量。 |
| `cwd` | 否 | stdio 子进程工作目录。 |
| `url` | HTTP/SSE 必填 | HTTP/SSE MCP 服务地址。 |
| `baseUrl` | 否 | `url` 的兼容别名；写入时会转换为 `url`。 |
| `headers` | 否 | HTTP/SSE 请求头。 |

兼容字段：

- `isActive` 会转换为 `enabled`。
- `type` 会转换为 `transport`。
- `baseUrl` 会转换为 `url`。

## stdio 示例

```json
{
  "mcpServers": {
    "aliyun-observability-mcp": {
      "description": "查询可观测数据",
      "transport": "stdio",
      "command": "{AGENT_WORKSPACE}/bin/start_mcp.py",
      "args": ["--workspace", "${AGENT_WORKSPACE}"],
      "cwd": "{AGENT_WORKSPACE}",
      "env": {
        "ALIBABA_CLOUD_ACCESS_KEY_ID": "$env:ALIBABA_CLOUD_ACCESS_KEY_ID",
        "ALIBABA_CLOUD_ACCESS_KEY_SECRET": "$env:ALIBABA_CLOUD_ACCESS_KEY_SECRET"
      }
    }
  }
}
```

说明：

- stdio client 会作为 QwenPaw 子进程启动。
- `command`、`args`、`cwd`、`env` 中支持 `{AGENT_WORKSPACE}` 和
  `${AGENT_WORKSPACE}` 占位符。
- Worker 会强制为 stdio client 注入正确的 `AGENT_WORKSPACE`，package
  中不要依赖自己覆盖这个变量。
- 如果脚本需要读 package 下发的凭证或配置，优先基于 `AGENT_WORKSPACE`
  拼路径，不要依赖 `~`。

## HTTP 示例

```json
{
  "mcpServers": {
    "package-docs": {
      "description": "查询 package 文档",
      "transport": "http",
      "url": "https://example.com/mcp",
      "headers": {
        "Authorization": "$env:DOCS_TOKEN"
      }
    }
  }
}
```

说明：

- `transport: "http"` 会由 QwenPaw 按 streamable HTTP MCP client 处理。
- 也可以直接写 `transport: "streamable_http"`。
- 如果省略 `transport`，但配置了 `url` 且没有 `command`，QwenPaw 会按
  HTTP MCP client 处理。

## SSE 示例

```json
{
  "mcpServers": {
    "event-stream": {
      "description": "事件流 MCP 服务",
      "transport": "sse",
      "url": "https://example.com/sse"
    }
  }
}
```

## 兼容旧格式

新 package 应使用标准 `mcpServers` 写法。为了兼容历史 package，worker 仍可读取：

```json
{
  "clients": {
    "docs": {
      "url": "https://example.com/mcp"
    }
  }
}
```

```json
{
  "mcp": {
    "clients": {
      "docs": {
        "url": "https://example.com/mcp"
      }
    }
  }
}
```

```json
{
  "docs": {
    "url": "https://example.com/mcp"
  }
}
```

这些旧格式只用于兼容迁移，不建议新 package 继续使用。

此外，worker 也能兼容读取 `mcpServers` 数组；数组中的每项必须带
`name` 或 `id`。这也只用于历史迁移，不建议新 package 使用。

## 加载和更新规则

- Worker 应用 package 时读取 package 根目录的 `mcp.json`。
- 读取后，worker 会把 MCP client 合入 QwenPaw 默认 agent 的 `agent.json`。
- 新 package 删除某个曾由旧 package 声明的 MCP client 时，worker 会从
  `agent.json` 中移除该 client。
- package 解包后，根目录的 `mcp.json` 不会保留到 workspace。
- `config/mcporter.json` 不受 package `mcp.json` 影响。

## 编写建议

- 新 package 一律使用 `{"mcpServers": {"name": {...}}}`。
- stdio MCP 脚本需要访问 workspace 时，使用 `AGENT_WORKSPACE`。
- 不要在 package `mcp.json` 中把 `AGENT_WORKSPACE` 设置成自定义路径。
- 不要在 `mcp.json` 中写明文密钥；优先引用环境变量或 workspace 内由
  package/config 下发的凭证文件。
- runtime 管理的 MCP 应走 Worker CR 的 `spec.mcpServers`，不要写进 package
  `mcp.json`。
