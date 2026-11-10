# Codex Harness members

Codex is an opt-in Worker runtime, packaged independently in [agentteams-harness-codex](https://github.com/agentteams-group/agentteams-harness-codex). The same runtime can be assigned a Team Leader or Worker role. It does not replace the Manager runtime.

Build and test the adapter in its repository, then configure a tested image with `spec.image` or `AGENTTEAMS_CODEX_WORKER_IMAGE`. The Controller deliberately has no default Codex image until a tested release is published. It projects the existing member `runtime.yaml` contract and manages Matrix, storage and container lifecycle.

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: codex-dev
spec:
  runtime: codex
  model: gpt-5.3-codex
  image: your-registry/agentteams-harness-codex:your-tested-version
```

For Helm, set `worker.defaultImage.codex.repository` and its independent `tag`. Setting a repository without a tag fails chart rendering. Existing runtime defaults are unchanged.

The model route must implement the Responses API. A successful Chat Completions request does not verify Codex compatibility. Matrix is the adapter's user interface; it supports progress, file results, cancellation and steering. See the adapter repository for supported controls, sandbox configuration, current limitations and actual acceptance evidence. In particular, Matrix E2EE, AgentSpec packages, DingTalk transport and legacy SSE MCP are not supported by this PoC.
