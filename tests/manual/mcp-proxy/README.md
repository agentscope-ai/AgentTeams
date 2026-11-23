# AT MCP 代理实测程序

同一个本地进程提供原生 HTTP API、官方 MCP Python SDK 的 SSE 和 Streamable HTTP 后端。凭证通过环境变量设置，审计文件只记录认证是否通过，不记录凭证。

```sh
python3 -m venv /tmp/at-mcp-venv
/tmp/at-mcp-venv/bin/pip install -r tests/manual/mcp-proxy/requirements.txt
export MCP_TEST_SECRET='your-test-credential'
export MCP_TEST_AUDIT=/tmp/at-mcp-audit.jsonl
/tmp/at-mcp-venv/bin/python tests/manual/mcp-proxy/server.py
```

默认仅监听 `127.0.0.1:19100`。Docker 中的网关访问宿主测试程序时，设置 `MCP_TEST_HOST=0.0.0.0`，后端主机使用 `host.docker.internal:19100`。通过 `MCP_TEST_PORT` 改端口。

每种认证有独立前缀：`/none`、`/bearer`、`/basic`、`/header`、`/query`。Basic 用户名为 `tester`；Header API Key 名为 `X-API-Key`；Query API Key 名为 `api_key`；凭证值均为 `MCP_TEST_SECRET`。

以 Bearer 为例：

| 接入类型 | 后端地址 | 工具 |
| --- | --- | --- |
| HTTP API 转 MCP | `http://host.docker.internal:19100/bearer` | GET `/api/echo`，必填 Query 字符串 `text` |
| SSE MCP 代理 | `http://host.docker.internal:19100/bearer/sse` | `echo(text)` |
| Streamable HTTP MCP 代理 | `http://host.docker.internal:19100/bearer/stream/` | `echo(text)` |

Dashboard 中配置后端认证，然后执行工具发现和工具调用。返回 `text`、认证模式 `mode` 及后端来源 `source`。

`verify.py` 经真实 Dashboard 管理 API 创建/更新三个名称固定的测试服务 `at-mcp-{rest,sse,stream}-1116`，不要在已有同名非测试资源的实例运行。使用浏览器登录后导出的 Playwright storage state（含登录凭证，须私密保存），以及单独保存测试凭证的文件：

```sh
/tmp/at-mcp-venv/bin/python tests/manual/mcp-proxy/verify.py \
  --state /private/path/browser-state.json \
  --secret-file /private/path/backend-secret \
  --output /tmp/mcp-result.json
```

`--auth` 支持 `bearer`、`basic`、`header`、`query`、`none`；`--credential wrong` / `missing` 验证拒绝路径。脚本断言保存、发现、真实回显或拒绝结果；HTTP 转 MCP 的工具发现无需访问后端，错误凭证应在真实工具调用时被拒绝。输出不包含后端凭证。默认 Dashboard 为 `http://127.0.0.1:13005`，可用 `--dashboard` 指定。

本程序验证 Dashboard → Higress → 后端的代理链路，不运行模型推理或 Matrix 协作流程。
