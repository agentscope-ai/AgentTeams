# Harness Worker

`harness-worker` is the fourth HiClaw runtime, delegating the agent loop to an external CLI tool (Claude Code, Gemini CLI, OpenCode, Codex) instead of running a gateway in-process.

## Supported CLIs

| Harness | CLI | Session resume | Output format |
|---------|-----|----------------|---------------|
| `claude` | `claude -p … --output-format stream-json --verbose` | `--resume <session-id>` | stream-json (JSONL) |
| `gemini` | `gemini --prompt … --yolo --output-format json` | *(single-turn)* | json |
| `opencode` | `opencode run … --format json --dangerously-skip-permissions` | `--session <id>` | json |
| `codex` | `codex exec … --json --ephemeral` | `codex exec resume --last` | jsonl |

## Architecture

```
Manager (OpenClaw/CoPaw)
    │ openclaw.json
    ▼ (Matrix + MinIO)
Worker Pod (runtime=harness, harnessType=claude|gemini|opencode|codex)
    ├── FileSync:      MinIO ↔ /root/hiclaw-fs/agents/<name>  (hiclaw_common.sync)
    ├── Bridge:        openclaw.json → native CLI config files
    ├── Matrix relay:  mautrix + hiclaw_common policies
    │       ▼ inbound Matrix message
    │   asyncio.create_subprocess_exec(<harness-cli> …)
    │       ▼ stdout (stream-json/json/jsonl), line by line
    │   process_stream_line → reply text + session_id
    │       ▲ send reply (HTML-formatted) to Matrix room
    └── Background:    sync_loop + push_loop
```

**Key design decisions:**

- **Request/response model** — each Matrix message spawns one CLI subprocess; no persistent PTY.
- **`--resume <session-id>`** — Claude harness maintains worker-wide session state across messages and pod restarts.
- **`hiclaw_common`** — shared Python package (`HiClaw/shared/python/hiclaw_common`) provides policies, FileSync, mautrix relay, and Matrix HTML formatting used by both harness and hermes runtimes.

## Package structure

```
HiClaw/harness/src/harness_worker/
├── cli.py             # Typer CLI (--harness-type flag)
├── config.py          # WorkerConfig
├── sync.py            # Thin re-export of hiclaw_common.sync (runtime_home_dir=".harness")
├── matrix_relay.py    # Thin adapter over hiclaw_common.matrix.MautrixRelay
├── worker.py          # Bootstrap: start → sync → Matrix relay → _invoke_harness
├── bridge.py          # openclaw.json → CLAUDE_HOME, harness-home layout
└── harness/
    ├── base.py        # BaseHarness ABC
    ├── claude.py      # ClaudeHarness (primary, full-featured)
    ├── gemini.py      # GeminiHarness
    ├── opencode.py    # OpenCodeHarness
    └── codex.py       # CodexHarness

HiClaw/shared/python/hiclaw_common/src/hiclaw_common/
├── policies.py        # DualAllowList, HistoryBuffer, apply_outbound_mentions
├── sync.py            # FileSync, push_loop, sync_loop
└── matrix.py          # MautrixRelay (mautrix-based Matrix client + HTML formatter)
```

## Components

### `BaseHarness`

Abstract base class in [harness/base.py](../HiClaw/harness/src/harness_worker/harness/base.py). All adapters implement:

| Method | Purpose |
|--------|---------|
| `bridge_config(cfg, harness_home)` | Write `settings.json`, generate `CLAUDE.md`, sync `.claude/skills/` symlinks, seed `mcpServers` |
| `build_command(message, session_id, workspace)` | Build `argv` for one non-interactive CLI invocation |
| `process_stream_line(line, state)` | Parse one JSONL line from streaming stdout (mutates `state`) |
| `parse_output(stdout_bytes)` | Full-output parse; returns `(text, session_id)` |
| `env(openclaw_cfg)` | Return per-harness auth env vars merged into subprocess environment |

Harnesses register via `@register_harness("name")`; the factory `build_harness(name)` looks up the registry.

### `Worker`

Bootstrap in [worker.py](../HiClaw/harness/src/harness_worker/worker.py):

1. Downloads all files from MinIO (`FileSync.mirror_all`).
2. Reads `openclaw.json` and re-authenticates the Matrix session.
3. Calls `harness.bridge_config(openclaw_cfg, harness_home)` to write native config.
4. Starts background `sync_loop` + `push_loop` tasks.
5. Enters `_run_matrix_relay()`: subscribes to Matrix and invokes harness per message.

### `MatrixRelay`

Thin adapter over `hiclaw_common.matrix.MautrixRelay`. On each inbound message:

1. Skips own messages and replayed history (events before startup timestamp).
2. Evaluates `DualAllowList.permits(sender, is_dm)`.
3. Drains `HistoryBuffer` for non-DM rooms (provides context window).
4. Calls `on_invoke(full_message)` → `_invoke_harness(message, session_id)`.
5. Applies `apply_outbound_mentions` (MSC3952 compliance) and sends reply as HTML.

## Worker._invoke_harness

File: [HiClaw/harness/src/harness_worker/worker.py](../HiClaw/harness/src/harness_worker/worker.py)

```python
proc = await asyncio.create_subprocess_exec(
    *argv, env=merged_env,
    stdout=PIPE, stderr=PIPE,
    cwd=str(workspace_dir),
)
# Read stdout line by line as the CLI streams — do NOT use communicate()
while True:
    line_bytes = await proc.stdout.readline()
    if not line_bytes:
        break
    self._harness.process_stream_line(line.strip(), state)

text = "".join(state.get("text_chunks", [])) or "(no response)"
new_sid = state.get("session_id")
```

Default timeout: `HICLAW_HARNESS_TIMEOUT_MS=600000` (10 minutes).

If a single JSON line from `claude --output-format stream-json` exceeds the 64 KB asyncio buffer limit, the worker catches `asyncio.LimitOverrunError`, appends a truncation warning to the reply, drains the buffer, and breaks — rather than crashing.

## ClaudeHarness — stream-json format

File: [HiClaw/harness/src/harness_worker/harness/claude.py](../HiClaw/harness/src/harness_worker/harness/claude.py)

### Event format

`claude --output-format stream-json --verbose` emits wrapped events:

```jsonc
{"type": "system",    "subtype": "init",    "session_id": "abc123"}
{"type": "assistant", "message": {"content": [
    {"type": "text",     "text": "I will check…"},
    {"type": "tool_use", "name": "Bash", "input": {"command": "ls /tmp"}}
]}, "session_id": "abc123"}
{"type": "user", "message": {"content": [
    {"type": "tool_result", "tool_use_id": "…", "content": "file1.txt", "is_error": false}
]}, "session_id": "abc123"}
{"type": "result", "subtype": "success", "result": "…",
    "session_id": "abc123", "duration_ms": 4210, "num_turns": 2,
    "usage": {"input_tokens": 1205, "output_tokens": 342}}
```

### process_stream_line — event handling

`process_stream_line(line, state)` is called for each stdout line:

| Event type | Action | Log |
|------------|--------|-----|
| `system/init` | Save `session_id` to state | `claude session init: <id>` |
| `assistant` / text block | Accumulate into `state["text_chunks"]` | — |
| `assistant` / tool_use | `_log_tool_use()`: log + append formatted line to chat (subject to cap) | per-tool format |
| `user` / tool_result | Append success/error line to chat (subject to cap) | `claude tool_result: <preview>` |
| `result` | Append overflow marker + stats footer; fallback text if no chunks | `claude result: input_tokens=… output_tokens=… duration=…ms turns=…` |
| `content_block_start` (SSE fallback) | Initialise accumulator in `state["active_tools"][idx]` | `claude tool start: <name>` |
| `content_block_delta / input_json_delta` (SSE) | Accumulate JSON fragments | *(silent)* |
| `content_block_stop` (SSE) | Join + parse fragments → `_log_tool_use` | per-tool format |

### Tool activity cap

`_MAX_ACTIVITY_LINES = 20` limits how many tool lines appear in the Matrix chat reply:

- Up to 20 tool_use/tool_result lines are shown verbatim.
- If exceeded: `> _… +N more tool calls (see pod logs)_` is inserted **before** the stats footer.
- The stats footer always appears: `> 📊 **in/out** N/N tok · ⏱ Xs · N turns · N calls`
- Pod logs (`logger.info/warning`) capture every tool call regardless of the cap.

### Tool format dispatch (`_format_tool_ui`)

| Tool | Chat display |
|------|-------------|
| `Bash` | `🖥️ **Bash**: \`<command>\`` (truncated at 120 chars, newlines → ` ↵ `) |
| `Read` | `📖 **Read**: <path>` |
| `Edit` / `MultiEdit` | `✏️ **Edit**: <path>` |
| `Write` | `📝 **Write**: <path>` |
| `Glob` / `Grep` | `🔍 **Glob**: <pattern>` / `🔍 **Grep**: <pattern>` |
| `WebSearch` / `WebFetch` / `Fetch` | `🌐 **WebSearch**: <query>` |
| `TodoWrite` | `📋 **TodoWrite**: N items` |
| `AskUser` | `❓ **AskUser**: <question>` |
| `Task` | `🤖 **Task**: <description>` |
| `mcp__*` | `🔌 **MCP** <server>: <first-arg>` |
| other | `⚙️ **<Name>**: <args>` |

## Per-harness CLI details

### Claude (`claude`)

| Setting | Value |
|---------|-------|
| Non-interactive flag | `claude -p "<message>"` |
| Session resume | `--resume <session-id>` |
| Output format | `--output-format stream-json --verbose` |
| Model flag | `--model <model-id>` |
| Config file | `<workspace>/.claude/settings.json` |
| MCP servers | `<workspace>/.claude.json` → `projects[cwd]["mcpServers"]` |
| Project instructions | `<workspace>/CLAUDE.md` (generated from `SOUL.md` + `AGENTS.md`) |
| Skills | `<workspace>/.claude/skills/<name>/` (symlinked from `workspace/skills/`) |
| Permissions | `dontAsk` with `allow: ["mcp__*"]` for native MCP tool calls |

**bridge_config merge order** (later wins):

```
1. Existing settings.json on disk          (user customisations survive restarts)
2. .harness/claude.settings.json           (per-worker MinIO override)
3. Controller-managed fields (always win):
     model, permissions (dontAsk + allow mcp__*), env (ANTHROPIC_*, timeouts)
```

**CLAUDE.md generation** — reads `workspace/SOUL.md` and `workspace/AGENTS.md` (synced from MinIO) and writes `workspace/CLAUDE.md`. Claude CLI reads this as project instructions automatically.

**Skills symlinks** — mirrors `workspace/skills/<name>/` → `workspace/.claude/skills/<name>/` as symlinks. Stale symlinks for removed skills are cleaned up; non-symlink directories are left untouched.

**MCP servers** — reads `workspace/config/mcporter.json` (generated by the controller from `spec.mcpServers`) and writes into `workspace/.claude.json` under `projects[cwd]["mcpServers"]`. HTTP, SSE, and stdio transports are supported:

```json
{
  "projects": {
    "/root/hiclaw-fs/agents/<worker-name>": {
      "mcpServers": {
        "deepwiki": { "type": "http",  "url": "https://mcp.deepwiki.com/mcp" },
        "github":   { "type": "sse",   "url": "https://mcp.github.com/sse"  },
        "my-tool":  { "type": "stdio", "command": "python3", "args": ["/opt/mcp/server.py"] }
      }
    }
  }
}
```

Entries from `config/mcporter.json` are fully controller-owned (stale entries replaced on every bridge run). Entries from `.harness/mcp-local.json` are merged after and win on name collision. Existing `.claude.json` content is preserved.

**Stdio MCP server override:** drop `.harness/mcp-local.json` in the worker's MinIO path:

```json
{
  "mcpServers": {
    "my-tool": {
      "transport": "stdio",
      "command": "python3",
      "args": ["/root/hiclaw-fs/agents/<worker>/.harness/my_server.py"]
    }
  }
}
```

**`.claudeignore`** — drop `.harness/claudeignore` in MinIO to control which files Claude Code ignores. If absent, a default is written (ignores `.harness/`, `.claude/`, `*.tar`, `*.log`).

**Hot-reload** — `_on_files_pulled` detects three change categories:

| Changed files | Action |
|---|---|
| `openclaw.json` | Full re-bridge (model + env + settings.json + CLAUDE.md + skills + .claudeignore) |
| `SOUL.md` or `AGENTS.md` | Lightweight: regenerate `CLAUDE.md` only |
| `skills/*` | Lightweight: re-sync `.claude/skills/` symlinks only |

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

## LLM routing via Higress

Higress ai-proxy 2.0 uses **auto-protocol detection** — it inspects the request path to determine the wire format automatically:

| Client path | Detected protocol | Upstream |
|---|---|---|
| `/v1/chat/completions` | OpenAI | pass-through |
| `/v1/messages` | Anthropic (Claude) | converted to OpenAI |

Claude CLI always sends to `ANTHROPIC_BASE_URL + /v1/messages`. Setting `ANTHROPIC_BASE_URL` to the bare Higress gateway URL is sufficient — no `/anthropic` suffix needed.

**Credential priority** (resolved at `bridge_config` time):

1. `HICLAW_CLAUDE_BASE_URL` + `HICLAW_LLM_API_KEY` — explicit operator override
2. `HICLAW_AI_GATEWAY_URL` + `HICLAW_WORKER_GATEWAY_KEY` — default in-cluster (injected by controller into every worker pod)
3. `_DEFAULT_BASE_URL` + `_DEFAULT_API_KEY` — local dev fallback

**Model constraint:** the model name in the request body must match a Higress AI route `modelPredicate`. Model is read from `openclaw.json → agents.defaults.model.primary` (format `"hiclaw-gateway/MiniMax-M2"` → `"MiniMax-M2"`). If no matching predicate exists, the gateway returns 404.

## Bridge (`bridge_config`)

On startup, `ClaudeHarness.bridge_config(openclaw_cfg, harness_home)` writes:

| File | Content |
|------|---------|
| `workspace/.claude/settings.json` | model, permissions (`dontAsk`), env vars |
| `workspace/.claude.json` | MCP servers (from `config/mcporter.json` or `.harness/mcp-local.json`) |
| `workspace/CLAUDE.md` | Concatenation of `SOUL.md` + `AGENTS.md` |
| `workspace/.claude/skills/` | Symlinks to `workspace/skills/` |
| `workspace/.claudeignore` | From `.harness/claudeignore` or default |
| `workspace/memory/` | Auto-created so Claude Code's auto-memory feature can write here |

## Session continuity

Worker-wide session state is persisted to `<harness_home>/sessions/current`:

- `_save_session(sid)` writes after every successful CLI invocation.
- `_load_session()` is called at startup — pod restarts resume the previous conversation automatically.
- `--resume <session-id>` is appended to the `claude -p` argv when a session is active.

## Matrix reply formatting

Outbound replies are sent as `org.matrix.custom.html` with `formatted_body` generated by `hiclaw_common.matrix._to_html()`:

- `<think>…</think>` blocks → `<blockquote>💭 …</blockquote>` (Element.io does not render `<details>`)
- Markdown (bold, blockquote, inline code, links) → HTML via `markdown-it-py`
- Fallback to regex-based conversion if `markdown-it-py` is absent at runtime

## Worker CRD spec

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

## Filesystem layout

```
/root/hiclaw-fs/agents/<worker-name>/          ← workspace_dir (synced from MinIO)
├── openclaw.json                               ← agent configuration (Manager-managed)
├── SOUL.md                                     ← agent persona / values (Manager-managed)
├── AGENTS.md                                   ← agent behaviour rules (Manager-managed)
├── CLAUDE.md                                   ← generated by bridge from SOUL.md + AGENTS.md
├── .claudeignore                               ← generated by bridge from .harness/claudeignore
├── .claude.json                                ← generated by bridge (project-level MCP servers)
├── config/
│   └── mcporter.json                           ← MCP server list HTTP/SSE (Manager-managed)
├── skills/                                     ← skill files synced from MinIO
│   └── <skill-name>/
│       └── SKILL.md
├── memory/                                     ← Claude Code auto-memory (worker-managed, pushed to MinIO)
├── .claude/
│   ├── settings.json                           ← generated by bridge_config
│   └── skills/
│       └── <skill-name> → …/skills/<skill-name>  ← absolute symlink
└── .harness/                                   ← harness_home (not synced to MinIO)
    ├── ready                                   ← touched when relay is up (readiness probe)
    ├── claude.settings.json                    ← optional settings override (deep-merged before controller fields)
    ├── mcp-local.json                          ← optional stdio/HTTP MCP servers
    ├── claudeignore                            ← optional .claudeignore source
    └── sessions/
        └── current                             ← last Claude session-id
```

**Ownership:**
- **Manager-managed (read-only in worker):** `openclaw.json`, `SOUL.md`, `AGENTS.md`, `config/mcporter.json`, `skills/`
- **Bridge-generated (derived, not pushed to MinIO):** `CLAUDE.md`, `.claudeignore`, `.claude.json`, `.claude/settings.json`, `.claude/skills/` symlinks
- **Worker-managed (pushed to MinIO):** `memory/`, `MEMORY.md`, `.harness/sessions/`
- **Harness-local overrides (in MinIO, not pushed back):** `.harness/claude.settings.json`, `.harness/mcp-local.json`, `.harness/claudeignore`

## Environment variables

### Required (injected by controller)

| Variable | Description |
|----------|-------------|
| `HICLAW_WORKER_NAME` | Worker identity |
| `HICLAW_FS_ENDPOINT` | MinIO endpoint |
| `HICLAW_FS_ACCESS_KEY` | MinIO access key |
| `HICLAW_FS_SECRET_KEY` | MinIO secret key |
| `HICLAW_AI_GATEWAY_URL` | Higress gateway base URL |
| `HICLAW_WORKER_GATEWAY_KEY` | Per-worker Higress consumer key |
| `HICLAW_MATRIX_DOMAIN` | Matrix server domain |

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `HICLAW_FS_BUCKET` | `hiclaw-storage` | MinIO bucket |
| `HICLAW_INSTALL_DIR` | `/root/hiclaw-fs/agents` | Workspace root |
| `HICLAW_HARNESS_TYPE` | `claude` | CLI variant: `claude\|gemini\|opencode\|codex` |
| `HICLAW_HARNESS_TIMEOUT_MS` | `600000` | Per-invocation timeout (ms) |
| `HICLAW_CLAUDE_BASE_URL` | — | Explicit LLM base URL (overrides gateway) |
| `HICLAW_LLM_API_KEY` | — | Explicit LLM API key (overrides gateway key) |

## Adding a new model

1. Create a Higress AI route with the new `modelPredicate` (via Higress console or API).
2. Update the worker's Team CR:
   ```yaml
   spec:
     workers:
       - name: dev-1
         runtime: harness
         model: MiniMax-M2.7
   ```
3. The harness reads `agents.defaults.model.primary` from `openclaw.json` and passes it directly to `claude --model` and every API request. No image rebuild required.

## Deployment in our k8s cluster

```bash
# Build (in HiClaw/ root — controller is a base stage for harness)
make build-harness-worker VERSION=<VER> DOCKER_PLATFORM=linux/amd64 \
  REGISTRY=<registry> REPO=<repo> \
  HIGRESS_REGISTRY=<higress-registry>

# Push via crane (avoids Docker Desktop VM ↔ host network limitations)
docker tag hiclaw/harness-worker:<VER> <registry>/<repo>/hiclaw-harness-worker:<VER>
docker save <registry>/<repo>/hiclaw-harness-worker:<VER> -o /tmp/harness.tar
crane push --insecure /tmp/harness.tar <registry>/<repo>/hiclaw-harness-worker:<VER>

# Deploy
helm upgrade --install hiclaw ./helm-deploy \
  -f ./helm-deploy/values-dev-plain.yaml \
  -n agentic --create-namespace

# Tail tool-use logs in real time
kubectl logs -n agentic -l app=hiclaw-harness-worker -f

# Rolling update after image push (patch Worker CR, then bounce pod)
kubectl patch team <team-name> -n <namespace> --type=json \
  -p='[{"op":"replace","path":"/spec/workers/<idx>/image","value":"<registry>/<repo>/hiclaw-harness-worker:<VER>"}]'
kubectl delete pod hiclaw-worker-<worker-name> -n <namespace>
```

## Troubleshooting

### Pod logs show `model=... url=http://higress-gateway...`

Expected — confirms the harness is routing through the Higress gateway:

```
bridge: claude settings → /root/hiclaw-fs/agents/dev-1/.claude/settings.json
  (model=MiniMax-M2, url=http://higress-gateway.<namespace>.svc.cluster.local:80)
```

### 404 from gateway

The model name does not match any Higress AI route `modelPredicate`. Check existing routes in the Higress console and align the Team CR `model` field.

### Worker ignores Matrix messages

Check DM / group policy env vars:

```bash
kubectl exec -n <namespace> hiclaw-worker-<name> -- env | grep MATRIX
```

### Claude CLI returns `(no response)`

- Verify `ANTHROPIC_BASE_URL` is set to the gateway URL (not a direct Anthropic endpoint).
- Confirm `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` matches `HICLAW_WORKER_GATEWAY_KEY`.
- Test the route directly:
  ```bash
  curl -s -X POST http://higress-gateway.<namespace>.svc.cluster.local:80/v1/messages \
    -H "Authorization: Bearer <gateway-key>" \
    -H "Content-Type: application/json" \
    -d '{"model":"MiniMax-M2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
  ```

### MinIO sync fails at startup

Verify MinIO credentials and that the worker's bucket/prefix exists. The controller creates the MinIO user and bucket policy when the Worker CR is reconciled.
