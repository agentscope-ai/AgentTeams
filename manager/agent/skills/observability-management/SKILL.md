---
name: observability-management
description: Use when you help a user enable Worker tracing, configure Dashboard observability, connect an OTLP Collector to Langfuse or AgentLoop, inspect traces, or troubleshoot missing spans.
---

# Worker Tracing

Use this workflow to guide the user through the AgentTeams Dashboard. You do
not need CMS or a Jaeger UI to use Dashboard Tracing. Missing CMS environment
variables in your container do not tell you whether Dashboard is deployed.
If you do not know the Dashboard address, ask the user for its address; do
not infer that the UI is unavailable from your local filesystem or ports.

## Supported Workers

You can apply this collection configuration to **QwenPaw and Hermes** Workers
whose images contain the telemetry adapter. DeepSeek Harness, OpenClaw, and
OpenHuman do not support this same built-in collection workflow in this
version. Do not promise that setting OTEL variables alone enables their
model/tool/turn spans. Manager tracing is not a selectable Worker target.

You can check a Worker's runtime and current phase with `agt get workers
<name> -o json`. A Running container proves startup, not trace ingestion.

## Guide the User Through Dashboard

1. Ask the user to open **Workbench → Tracing** when available, or **Settings → Management → Tracing** (the management
   group is labelled **运行与治理**). You can also point to the Dashboard URL
   with `/#console/governance/tracing` appended. In Tracing, an administrator
   opens **可观测配置**.
2. Explain that collection is optional and disabled by default. The user
   enables **启用 Worker Tracing** and selects the query backend.
   - For **Langfuse**, the user enters **Langfuse 服务端地址**, **浏览器访问地址**
     (optional), **Public Key**, **Secret Key**, and **Project ID**. The server
     address must be reachable from Dashboard; the browser address must be
     reachable from the user's browser. **测试连接** can resolve Project ID.
   - For **阿里云 AgentLoop**, the user enters **地域**, **CMS Workspace**,
     **Trace SLS Project**, and **Trace Logstore**. The query uses Dashboard's
     server-side Alibaba Cloud identity with read permission on that source.
     This is the original Span logstore, not `agent-trajectory`. Keep sources
     isolated for different organizations.
3. The user must already have an **OTLP/HTTP Collector** configured to export
   to the chosen platform. They enter **Collector OTLP/HTTP 地址**, reachable
   from the Worker network (for example `http://otel-collector:4318`), and
   optional **Collector Headers**. Saving Dashboard configuration does not
   deploy a Collector or configure its exporters. Collector exporters may
   send to both platforms; the query backend selects where Dashboard reads.
4. Explain **采集模型和工具的输入输出** before the user enables it: it captures
   task/model/tool content as well as timing. Let the user choose whether
   that content should be collected.
5. The user clicks **测试连接**, then **保存配置**. Saving does not restart
   Dashboard. Your Controller stores the configuration and injects it into
   newly created supported Workers. Existing Workers require an explicit apply.
6. Under **应用到 Worker**, the user selects the intended QwenPaw/Hermes
   Workers and clicks **应用到所选 Worker（重建生效）**. Explain before applying
   that running managed containers are rebuilt and current tasks may be
   interrupted; sleeping Workers receive the configuration on next start.
   Check individual apply results and wait for affected Workers to be ready.

Never ask the user to paste credentials into chat. They enter Secret Key and
Collector Headers in the password fields; query credentials stay in your
Controller's persisted configuration. Do not put Langfuse query credentials
into Worker env or repeat them in a response. To inspect a conversation,
use `agt tracing --agent <worker-name> --since 1h -o json`, or
`agt tracing --session <session-id> -o json`, then `agt tracing show
<trace-id> -o json` for observations. You can also filter by `--team`,
`--task`, `--project`, `--room`, and `--runtime`; inspect pagination before
claiming completeness. An empty formal-task list does not mean no traces
exist. For a formal task, load `task-diagnosis` and use `agt get tasks`
followed by `agt tracing task` to inspect task dependencies and causal links.

## Verify End to End

After the Worker is ready, ask the user to send it a small real task. In
Tracing, select that Worker and the relevant time range, then refresh after
a few seconds for ingestion. Select the new Trace to inspect its execution
waterfall, model/tool observations, timing, and any captured content. A
successful configuration save or apply is not proof that a Trace exists.

If no Trace appears, check in order: supported runtime/image, per-Worker
apply result and readiness, a task that actually ran after applying,
Worker-to-Collector network access, Collector exporter delivery, and query
backend/project/time filters. A successful backend connection test checks
query access; it does not prove Worker → Collector → backend delivery.
