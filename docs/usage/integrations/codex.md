# Codex Harness members

Codex is an opt-in runtime, packaged independently in [agentteams-harness-codex](https://github.com/agentteams-group/agentteams-harness-codex). Updated images support Manager, Team Leader and Worker roles. A Manager uses the shared Manager prompts, resource-management and diagnosis skills without requiring Team membership, and applies the Controller-owned Matrix sender policy.

The published v1.2.7 Codex image supports Worker/Leader only. To use a Codex Manager, select an updated image explicitly until a release containing Manager support is published:

```sh
agt create manager --name default --model <responses-model> --runtime codex --image <updated-codex-harness-image>
```

With Helm, set `manager.runtime=codex` and an updated `manager.image.repository` / `manager.image.tag`. The same image serves every role; no dedicated Codex Manager image is required.

Runtime selection is supported at launch. Seamless cross-Harness hot switching is tracked separately in [#1631](https://github.com/agentscope-ai/AgentTeams/issues/1631); a runtime round trip can replay messages from an older Harness-specific Matrix cursor.

Select `runtime: codex`; you do not need to provide an image. The v1.2.7 installer and Helm chart select `higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-harness-codex:v1.2.7`, part of the six-Harness release. A bare Controller without an image override still falls back to `v0.1.0`; set `AGENTTEAMS_CODEX_WORKER_IMAGE` explicitly when deploying it outside the installer or chart. You can optionally override it with `spec.image` or `AGENTTEAMS_CODEX_WORKER_IMAGE`. The Controller projects the existing member `runtime.yaml` contract and manages Matrix, storage and container lifecycle.

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
