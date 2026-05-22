# Harness Worker

The **harness** runtime is a fourth HiClaw worker type that delegates the agent loop to an
external AI coding CLI instead of running an in-process LLM gateway.

Supported CLIs:

| `harnessType` | NPM package | Command |
|---------------|-------------|---------|
| `claude` | `@anthropic-ai/claude-code` | `claude` |
| `gemini` | `@google/gemini-cli` | `gemini` |
| `opencode` | `opencode-ai` | `opencode` |
| `codex` | `@openai/codex` | `codex` |

---

## Architecture

```
Manager (OpenClaw / CoPaw)
    │ assigns task via Matrix message
    ▼
Worker pod  (runtime=harness, harnessType=claude|gemini|opencode|codex)
    ├─ FileSync          MinIO ↔ /root/hiclaw-fs/agents/<name>/
    ├─ Bridge            openclaw.json → ~/.claude/settings.json
    │                                  | ~/.gemini/settings.json
    │                                  | ~/.config/opencode/opencode.json
    │                                  | ~/.codex/config.toml
    ├─ Matrix relay      (matrix-nio + HiClaw DualAllowList / HistoryBuffer)
    │       ▼ on inbound message
    │   subprocess  <harness-cli> -p "<message>" --resume <session-id>
    │       ▲ stdout (stream-JSON / JSON)  →  Matrix room reply
    └─ Background        sync_loop + push_loop (MinIO)
```

### Request / response model

Each Matrix message spawns a **fresh CLI invocation** using the harness's
non-interactive flag (`claude -p`, `gemini --prompt`, etc.) and `--resume <session-id>`
for multi-turn context. There is no long-lived PTY; each subprocess returns when the
response is complete. This matches how each CLI is designed for CI/automation use.

---

## LLM Routing via Higress Gateway

All harness workers route LLM requests through the same **Higress ai-proxy** gateway
that all other workers use, giving unified authentication, billing, and key rotation
without embedding provider secrets in the image.

### Auto-protocol detection

Higress ai-proxy 2.0 inspects the request **path** to determine the wire format
automatically:

| Client path | Detected protocol | Upstream format |
|---|---|---|
| `/v1/chat/completions` | OpenAI | OpenAI (pass-through) |
| `/v1/messages` | Anthropic (Claude) | OpenAI (converted) |

Claude CLI always sends requests to `ANTHROPIC_BASE_URL + /v1/messages`.
Setting `ANTHROPIC_BASE_URL` to the bare Higress gateway URL
(`http://higress-gateway.<namespace>.svc.cluster.local:80`) is sufficient — no
`/anthropic` suffix is needed. Higress converts the Anthropic request to the upstream
provider format (OpenAI for MiniMax) and converts the response back, including streaming.

### Credential resolution (Claude harness)

`_resolve_credentials()` in `claude.py` follows a three-level priority chain resolved
at `bridge_config` time:

```
1. HICLAW_CLAUDE_BASE_URL + HICLAW_LLM_API_KEY     explicit operator override
2. HICLAW_AI_GATEWAY_URL  + HICLAW_WORKER_GATEWAY_KEY  default in cluster
3. https://api.minimax.io/anthropic + dev-key       local dev fallback
```

The controller always injects `HICLAW_AI_GATEWAY_URL` and `HICLAW_WORKER_GATEWAY_KEY`
into every worker pod via `worker_env.go`, so level 2 is automatically active
in-cluster without any Team CR or Helm changes.

### Model constraint

Higress AI routes are created with `modelPredicates` that perform exact-match on the
model field in the request body. The model resolved from
`openclaw.json → agents.defaults.model.primary` must match an existing Higress route
predicate. For example `hiclaw-gateway/MiniMax-M2` resolves to `MiniMax-M2`, which
matches the `default-ai-route` predicate. If a worker uses a model with no matching
predicate, the gateway returns 404.

---

## Components

### `harness_worker.harness.BaseHarness`

Abstract base class in [harness/base.py](../harness/src/harness_worker/harness/base.py).
All four adapters implement:

| Method | Purpose |
|--------|---------|
| `bridge_config(cfg, harness_home)` | Write the harness's native config file(s) from `openclaw.json` |
| `build_command(message, session_id, workspace)` | Build `argv` for one non-interactive CLI invocation |
| `process_stream_line(line, state)` | Parse one JSONL line from streaming stdout (mutates `state`) |
| `parse_output(stdout_bytes)` | Full-output parse; returns `(text, session_id)` |
| `env(openclaw_cfg)` | Return per-harness auth env vars merged into subprocess environment |

A harness registers itself via the `@register_harness("name")` decorator. The factory
`build_harness(name)` looks up the registry.

### `harness_worker.worker.Worker`

Main bootstrap in [worker.py](../harness/src/harness_worker/worker.py):

1. Downloads all files from MinIO (`FileSync.mirror_all`).
2. Reads `openclaw.json` and re-authenticates Matrix session.
3. Calls `harness.bridge_config(openclaw_cfg, harness_home)` to write native config.
4. Starts background `sync_loop` + `push_loop` tasks.
5. Enters `_run_matrix_relay()`: subscribes to Matrix and invokes harness per message.

### `harness_worker.matrix_relay.MatrixRelay`

Built on `matrix-nio`. On each inbound message:

1. Skips own messages and replayed history (events before startup timestamp).
2. Evaluates `DualAllowList.permits(sender, is_dm)`.
3. Drains `HistoryBuffer` for non-DM rooms (provides context window).
4. Calls `_invoke_harness(full_message, session_id)`.
5. Applies `apply_outbound_mentions` (MSC3952 compliance) and sends reply.

### `harness_worker.bridge`

Helper functions for port remapping (container vs. host) and `openclaw.json` field
extraction, shared across harness adapters.

---

## Per-Harness CLI Details

### Claude (`claude`)

| Setting | Value |
|---------|-------|
| Non-interactive flag | `claude -p "<message>"` |
| Session resume | `--resume <session-id>` |
| Output format | `--output-format stream-json --verbose` (JSONL events) |
| Model flag | `--model <model-id>` |
| Config file | `<workspace>/.claude/settings.json` |

**bridge_config** writes `settings.json` with:
- `model` — from `openclaw.json agents.defaults.model.primary`
- `permissions.defaultMode` — `"dontAsk"` (subprocess mode; `bypassPermissions` is
  blocked for root containers)
- `env` — all `ANTHROPIC_*` overrides plus `API_TIMEOUT_MS` and
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`

**Per-worker MinIO override:** drop a file at
`<worker>/.harness/claude.settings.json` in MinIO to inject extra settings
(e.g. `customInstructions`, `mcpServers`). Bridge deep-merges it before controller
fields, so controller values always win.

### Gemini (`gemini`)

| Setting | Value |
|---------|-------|
| Non-interactive flag | `gemini --prompt "<message>" --yolo` |
| Session resume | Not supported — single-turn only |
| Output format | `--output-format json` |
| Config file | `~/.gemini/settings.json` |
| Required env | `GEMINI_API_KEY` or `GOOGLE_API_KEY` |

### OpenCode (`opencode`)

| Setting | Value |
|---------|-------|
| Non-interactive flag | `opencode run "<message>" --format json --dangerously-skip-permissions` |
| Session resume | `--session <id>` or `--continue` |
| Config file | `~/.config/opencode/opencode.json` |

### Codex (`codex`)

| Setting | Value |
|---------|-------|
| Non-interactive flag | `codex exec "<message>" --json --ephemeral --sandbox workspace-write` |
| Session resume | `codex exec resume --last "<message>"` |
| Output format | JSONL |
| Required env | `CODEX_API_KEY` or `OPENAI_API_KEY` |

---

## Worker CRD Spec

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: my-claude-worker
spec:
  runtime: harness
  harnessType: claude        # claude | gemini | opencode | codex  (default: claude)
  model: MiniMax-M2          # must match a Higress AI route modelPredicate
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: "2"
      memory: 2Gi
```

Or as part of a Team CR:

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: my-team
spec:
  workers:
    - name: dev-1
      runtime: harness
      harnessType: claude
      model: MiniMax-M2
```

---

## Deployment

### Helm values

```yaml
# helm-deploy/values.yaml
worker:
  defaultImage:
    harness:
      repository: higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/hiclaw-harness-worker
      tag: ""          # defaults to global.imageTag
  defaultHarnessType: "claude"
```

To pin a specific version:

```yaml
# helm-deploy/values-<env>.yaml
worker:
  defaultImage:
    harness:
      repository: higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/hiclaw-harness-worker
      tag: "latest"
```

### Build and push

```bash
cd HiClaw/harness

docker build --platform linux/amd64 \
  -t higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/hiclaw-harness-worker:latest .

docker push higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/hiclaw-harness-worker:latest
```

### Rolling update (patch Team CR image, then bounce pod)

```bash
kubectl patch team <team-name> -n <namespace> --type=json \
  -p='[{"op":"replace","path":"/spec/workers/<idx>/image",
        "value":"higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/hiclaw-harness-worker:latest"}]'

kubectl delete pod hiclaw-worker-<worker-name> -n <namespace>
```

---

## Environment Variables Reference

### Required (injected by controller)

| Variable | Description |
|----------|-------------|
| `HICLAW_WORKER_NAME` | Worker name |
| `HICLAW_FS_ENDPOINT` | MinIO endpoint |
| `HICLAW_FS_ACCESS_KEY` | MinIO access key |
| `HICLAW_FS_SECRET_KEY` | MinIO secret key |
| `HICLAW_AI_GATEWAY_URL` | Higress gateway base URL (e.g. `http://higress-gateway.<namespace>.svc.cluster.local:80`) |
| `HICLAW_WORKER_GATEWAY_KEY` | Per-worker Higress consumer key |
| `HICLAW_MATRIX_DOMAIN` | Matrix server domain |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `HICLAW_FS_BUCKET` | `hiclaw-storage` | MinIO bucket |
| `HICLAW_INSTALL_DIR` | `/root/hiclaw-fs/agents` | Workspace root |
| `HICLAW_HARNESS_TYPE` | `claude` | CLI variant: `claude\|gemini\|opencode\|codex` |
| `HICLAW_HARNESS_TIMEOUT_MS` | `600000` | Per-invocation timeout in milliseconds |
| `HICLAW_CLAUDE_BASE_URL` | — | Explicit LLM base URL (overrides gateway) |
| `HICLAW_LLM_API_KEY` | — | Explicit LLM API key (overrides gateway key) |

---

## Filesystem Layout

```
/root/hiclaw-fs/agents/<worker-name>/          ← workspace_dir (synced from MinIO)
├── openclaw.json                               ← agent configuration
├── SOUL.md                                     ← agent persona / instructions
├── AGENTS.md                                   ← agent capability declaration
├── skills/                                     ← synced skill files
├── .claude/
│   └── settings.json                           ← generated by bridge_config
└── .harness/
    ├── ready                                   ← touched when relay is up (readiness)
    ├── claude.settings.json                    ← optional per-worker MinIO override
    └── sessions/
        └── current                             ← last Claude session-id
```

---

## Adding a New Model

1. Create (or update) a Higress AI route with the new `modelPredicate`:

   ```bash
   # Example: add MiniMax-M2.7 route via Higress console or API
   # modelPredicates: [{matchType: EQUAL, matchValue: "MiniMax-M2.7"}]
   ```

2. Update the worker's Team CR spec to use the new model name:

   ```yaml
   spec:
     workers:
       - name: dev-1
         runtime: harness
         model: MiniMax-M2.7
   ```

3. The harness reads `agents.defaults.model.primary` from `openclaw.json` (generated
   by the controller from the Team CR) and passes the model name directly to Claude CLI
   `--model` and in every API request. No image rebuild is required.

---

## Troubleshooting

### Pod logs show `model=... url=http://higress-gateway...`

Expected. The bridge log line confirms the harness is routing through the Higress gateway:

```
bridge: claude settings → /root/hiclaw-fs/agents/dev-1/.claude/settings.json (model=MiniMax-M2, url=http://higress-gateway.<namespace>.svc.cluster.local:80)
```

### 404 from gateway

The model name in `openclaw.json` does not match any Higress AI route `modelPredicate`.
Check existing routes in Higress console and align the Team CR `model` field.

### Worker ignores Matrix messages

Check DM / group policy env vars:
```bash
kubectl exec -n <namespace> hiclaw-worker-<name> -- env | grep MATRIX
```

### Claude CLI returns `(no response)`

- Verify `ANTHROPIC_BASE_URL` is set to the gateway URL (not an Anthropic endpoint).
- Check `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` match the worker's `HICLAW_WORKER_GATEWAY_KEY`.
- Run a direct curl to the gateway to confirm the route is healthy:
  ```bash
  curl -s -X POST http://higress-gateway.<namespace>.svc.cluster.local:80/v1/messages \
    -H "Authorization: Bearer <gateway-key>" \
    -H "Content-Type: application/json" \
    -d '{"model":"MiniMax-M2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
  ```

### MinIO sync fails at startup

Verify MinIO credentials and that the worker's bucket/prefix exists. The controller
creates the MinIO user and bucket policy when the Worker CR is reconciled.
