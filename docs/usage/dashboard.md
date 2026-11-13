# Dashboard Guide

English | [中文](../zh-cn/usage/dashboard.md)

This guide covers Dashboard v1.4.1 with Core v1.2.7. Dashboard has its own release version. Install through the [Quickstart](../quickstart.md); for an existing cluster, see the [standalone Dashboard guide](https://github.com/agentteams-group/agentteams-dashboard#-quick-start).

## First sign-in and model setup

The local installer enables Dashboard at `http://127.0.0.1:13000/`, opens the browser with `admin` prefilled, and copies the generated password to the clipboard. If this is unavailable, use the credentials printed in the terminal. Bash and PowerShell support this flow.

Installation does not require a model key. If the Manager has no configured model, choose **Configure model** in its conversation. In model management, configure the provider, select a model, save and verify it, then return to the conversation. Provider and model are selected separately. Available management actions depend on your account permissions; a member without model-management access can still chat with an authorized, configured Agent.

## Find the right workspace

The v1.4.1 sidebar uses the following Chinese labels. Visible entries depend on permissions, deployment mode, and feature settings.

| Entry | Use it for |
|---|---|
| **助手** | Manager and Agent conversations; inspect responses and ongoing work |
| **我的团队** | Teams available to the current Human and their collaboration context |
| **工作台** | Task board, artifacts, coding runs, loops, and **Tracing** |
| **资产中心** | Models, tools/MCP, Skills, knowledge, and files |
| **成员与团队 → 团队与Agent** | A unified list of Teams, Managers, and unassigned Workers, with topology and details shown in the page |
| **设置** | Account and appearance settings; choose a theme manually or follow the system |

Select an Agent to inspect its runtime, model, status, and available session/lifecycle actions. Choose a Provider before choosing its Model. Resource management and chat permissions are checked separately; a hidden management button does not necessarily mean the Agent is unavailable for conversation.

## Conversations and human attention

Thinking and tool calls from the same turn are grouped together. During execution, the process preview shows a bounded amount of live content; after completion, it folds automatically. Expand it when you need the intermediate details.

The task board and conversation task context expose progress and outstanding human attention. Open the relevant task to inspect the request and submit a decision. Sending an additional chat message does not necessarily resolve an existing structured attention request. Use task pause/resume actions when available; see [Project Workflow API](project-workflow-api.md) for lifecycle semantics.

Voice input writes a transcription into the draft for review before sending. See the [Dashboard voice-input guide](https://github.com/agentteams-group/agentteams-dashboard/blob/main/docs/voice-input.md) for local recognition and optional service configuration.

## Skills and personal files

Open **资产中心 → 技能** to upload a complete Skill ZIP and distribute it to Workers. Uploading to the catalog alone does not assign it to a Worker. Distribution writes the package, updates `Worker.spec.skills`, and attempts to restart the Worker; do this while it is idle. See the [Worker Guide](worker-guide.md#method-2-distribute-through-the-dashboard).

Personal Human files and Agent workspaces are distinct. Use personal-file sharing or a file reference in the conversation when making a Human file available to an Agent. The system administrator also has a personal Human file area. See [Human Work Area](human-work-area.md) and [Operator Human Files](operator-human-files.md) for access and storage details.

## Optional Tracing

Open **工作台 → Tracing**. Collection is off by default; an unconfigured deployment displays setup guidance. An administrator can open **可观测配置**, configure a query backend and an existing OTLP/HTTP Collector, test, save, and explicitly apply settings to existing supported Workers.

Core v1.2.7 stores these settings in the Controller namespace's `agentteams-observability` Secret (`config.json`). Langfuse queries and query secrets belong to the Controller. Dashboard no longer uses its old local `observability.json`; after upgrading an installation that used that file, save the settings again through the UI. Empty secret fields retain saved values.

Saving does not restart running Workers. New supported containers receive the saved settings; applying to existing managed QwenPaw/Hermes Workers uses the normal environment-update and recreation flow. Apply while idle, and check the result for each Worker. Disabling collection also needs an apply for existing containers. Successful configuration or a connection test does not prove ingestion: run a short conversation or task and inspect its trace.

For Langfuse, the Collector must already export to the chosen project. AgentLoop settings also live in the Controller, while Dashboard's AgentLoop query adapter uses its server-side cloud identity; see the [Dashboard backend contracts](https://github.com/agentteams-group/agentteams-dashboard/blob/main/docs/INTERFACES.md). Changing the query backend does not configure Collector exporters.

You can query authorized Langfuse traces without first creating a formal task:

```bash
docker exec agentteams-controller agt tracing --agent alice --since 1h
docker exec agentteams-controller agt tracing --team my-team --limit 20 --page 1 -o json
docker exec agentteams-controller agt tracing session '<session-id>' -o json
docker exec agentteams-controller agt tracing show '<trace-id>'
docker exec agentteams-controller agt tracing task '<project-id>' '<task-id>' --team my-team --related
```

The Agent filter identifies Worker resources, including Team Leaders. Unbounded list queries default to the last 24 hours; session queries cover retained history without that cutoff. Lists are paginated. A query only returns evidence the caller may access and the runtime actually emitted; a missing trace is not proof that no work occurred. See [task tracing design](../design/task-tracing-analysis.md) for Controller contracts and collection limits.
