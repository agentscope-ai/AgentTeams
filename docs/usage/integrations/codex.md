# Codex Harness members

Codex is an opt-in Worker runtime, packaged independently in [agentteams-harness-codex](https://github.com/agentteams-group/agentteams-harness-codex). The same runtime can be assigned a Team Leader or Worker role. It does not replace the Manager runtime.

Select `runtime: codex`; you do not need to provide an image. The Controller and installers default to `higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-harness-codex:v0.1.0`, independently versioned from AgentTeams. You can optionally override it with `spec.image` or `AGENTTEAMS_CODEX_WORKER_IMAGE`. The Controller projects the existing member `runtime.yaml` contract and manages Matrix, storage and container lifecycle.

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: codex-dev
spec:
  runtime: codex
  model: gpt-5.3-codex
```

Helm includes the same default image. To override it, set `worker.defaultImage.codex.repository` and its independent `tag`; it does not follow `global.imageTag`. For local installation, you can optionally set `AGENTTEAMS_CODEX_WORKER_VERSION` or `AGENTTEAMS_INSTALL_CODEX_WORKER_IMAGE`.

The model route must implement the Responses API. A successful Chat Completions request does not verify Codex compatibility. Matrix is the adapter's user interface; it supports progress, file results, cancellation and steering. See the adapter repository for supported controls, sandbox configuration, current limitations and actual acceptance evidence. In particular, Matrix E2EE, AgentSpec packages, DingTalk transport and legacy SSE MCP are not supported by this PoC.
