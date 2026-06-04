# Harness Worker for HiClaw

A HiClaw worker runtime that delegates the agent loop to an external CLI tool instead of running a gateway in-process.

Supported CLIs:

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

- **Request/response model** — each Matrix message spawns one CLI subprocess, no persistent PTY.
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

## Claude harness — stream-json processing

`ClaudeHarness.process_stream_line()` handles the JSONL events emitted by `claude --output-format stream-json --verbose`:

| Event | Action |
|-------|--------|
| `system/init` | Save `session_id` to state |
| `assistant` / text block | Accumulate into `state["text_chunks"]` |
| `assistant` / tool_use | `_log_tool_use()`: log + append `> 🔧 **Tool**: …` to chat (up to `_MAX_ACTIVITY_LINES=20`) |
| `user` / tool_result | Append `> ✅ \`result\`` or `> ❌ **error**: …` (subject to cap) |
| `result` | Append overflow marker if >20 tool calls; append stats footer `> 📊 **in/out** N/N tok · ⏱ Xs · N turns · N calls` |

**Tool activity cap (`_MAX_ACTIVITY_LINES = 20`):**
- Chat shows up to 20 tool lines.
- If exceeded: `> _… +N more tool calls (see pod logs)_` appears before the stats footer.
- Pod logs (`logger.info/warning`) always capture every tool call regardless of the cap.

**Tool format dispatch** in `_format_tool_ui()`:

| Tool | Chat display |
|------|-------------|
| `Bash` | `🖥️ **Bash**: \`<command>\`` (truncated at 120 chars) |
| `Read` | `📖 **Read**: <path>` |
| `Edit` / `Write` / `MultiEdit` | `✏️ **Edit**: <path>` / `📝 **Write**: <path>` / `✏️ **MultiEdit**: <path>` |
| `Glob` / `Grep` | `🔍 **Glob**: <pattern>` / `🔍 **Grep**: <pattern>` |
| `WebSearch` / `WebFetch` | `🌐 **WebSearch**: <query>` |
| `TodoWrite` | `📋 **TodoWrite**: N items` |
| `mcp__*` | `🔌 **MCP** <server>: <first-arg>` |
| other | `⚙️ **<Name>**: <args>` |

## LLM routing

Claude CLI routes through Higress AI Gateway via credential priority:

1. `HICLAW_CLAUDE_BASE_URL` + `HICLAW_LLM_API_KEY` — explicit operator override
2. `HICLAW_AI_GATEWAY_URL` + `HICLAW_WORKER_GATEWAY_KEY` — default in-cluster (Higress auto-detects Anthropic wire format, converts to MiniMax)
3. `_DEFAULT_BASE_URL` + `_DEFAULT_API_KEY` — local dev fallback

Model is read from `openclaw.json → agents.defaults.model.primary` (format `"hiclaw-gateway/MiniMax-M2"` → `"MiniMax-M2"`).

## Bridge (`bridge_config`)

On startup `ClaudeHarness.bridge_config(openclaw_cfg, harness_home)` writes:

| File | Content |
|------|---------|
| `workspace/.claude/settings.json` | model, permissions (`dontAsk`), env vars |
| `workspace/.claude.json` | MCP servers (from `config/mcporter.json` or `.harness/mcp-local.json`) |
| `workspace/CLAUDE.md` | Concatenation of `SOUL.md` + `AGENTS.md` |
| `workspace/.claude/skills/` | Symlinks to `workspace/skills/` |
| `workspace/.claudeignore` | From `.harness/claudeignore` or default |
| `workspace/memory/` | Auto-created (Claude Code auto-memory writes here) |

Per-worker override: drop a file at MinIO path `<worker>/.harness/claude.settings.json` to inject `customInstructions`, extra permissions, etc. Controller-managed fields (model, permissions, env) always take precedence.

## Session continuity

Worker-wide session state is persisted to `<harness_home>/sessions/current`:

- `_save_session(sid)` writes on every successful CLI invocation.
- `_load_session()` is called at startup — pod restarts resume the previous conversation.
- The `--resume <session-id>` flag is added to the `claude -p` argv when a session is active.

## Matrix reply formatting

Outbound replies use `org.matrix.custom.html` with `formatted_body` generated by `hiclaw_common.matrix._to_html()`:

- `<think>…</think>` blocks → `<blockquote>💭 …</blockquote>`
- Markdown (bold, blockquote, inline code, links) → HTML via `markdown-it-py`
- Fallback to regex if `markdown-it-py` is absent

## Usage

```bash
harness-worker \
  --name my-worker \
  --fs localhost:9000 \
  --fs-key minioaccess \
  --fs-secret miniosecret \
  --fs-bucket hiclaw-storage \
  --harness-type claude
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `HICLAW_WORKER_NAME` | *(required)* | Worker identity |
| `HICLAW_FS_ENDPOINT` | *(required)* | MinIO endpoint |
| `HICLAW_FS_ACCESS_KEY` | *(required)* | MinIO access key |
| `HICLAW_FS_SECRET_KEY` | *(required)* | MinIO secret key |
| `HICLAW_FS_BUCKET` | `hiclaw-storage` | MinIO bucket |
| `HICLAW_INSTALL_DIR` | `/root/hiclaw-fs/agents` | Workspace root |
| `HICLAW_HARNESS_TYPE` | `claude` | CLI variant: `claude\|gemini\|opencode\|codex` |
| `HICLAW_HARNESS_TIMEOUT_MS` | `600000` | Per-invocation timeout (ms) |
| `HICLAW_AI_GATEWAY_URL` | *(required in-cluster)* | Higress gateway URL |
| `HICLAW_WORKER_GATEWAY_KEY` | *(required in-cluster)* | API key for Higress |
| `HICLAW_CLAUDE_BASE_URL` | *(optional override)* | Claude CLI base URL override |
| `HICLAW_LLM_API_KEY` | *(optional override)* | API key override |

## Build & deploy

```bash
# In HiClaw/ root — builds controller first (harness uses it as a base stage)
make build-harness-worker VERSION=<VER> DOCKER_PLATFORM=linux/amd64 \
  REGISTRY=127.0.0.1:30000 REPO=momovn-dev \
  HIGRESS_REGISTRY=higress-registry.ap-southeast-7.cr.aliyuncs.com

# Tag + save + push via crane (Docker Desktop VM cannot reach localhost:30000 directly)
docker tag hiclaw/harness-worker:<VER> 127.0.0.1:30000/momovn-dev/hiclaw-harness-worker:<VER>
docker save 127.0.0.1:30000/momovn-dev/hiclaw-harness-worker:<VER> -o /tmp/harness.tar
crane push --insecure /tmp/harness.tar 127.0.0.1:30000/momovn-dev/hiclaw-harness-worker:<VER>

# Deploy
helm upgrade --install hiclaw ./helm-deploy \
  -f ./helm-deploy/values-dev-plain.yaml \
  -n agentic --create-namespace

# Tail tool-use logs
kubectl logs -n agentic -l app=hiclaw-harness-worker -f
```

## Related docs

- [HiClaw/docs/harness-worker.md](../docs/harness-worker.md) — detailed event processing, env vars reference
