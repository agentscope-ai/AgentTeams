# Manager Guide

Detailed guide for setting up and configuring the HiClaw Manager.

## Installation

See [quickstart.md](quickstart.md) Step 1 for basic installation.

## Configuration

The Manager is configured via environment variables set during installation. The installer generates a `.env` file with all settings.

### Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `HICLAW_LLM_API_KEY` | Yes | - | LLM API key |
| `HICLAW_LLM_PROVIDER` | No | `qwen` | LLM provider (`qwen` for Alibaba Cloud, `openai-compat` for OpenAI-compatible APIs) |
| `HICLAW_DEFAULT_MODEL` | No | `qwen3.5-plus` | Default model ID |
| `HICLAW_ADMIN_USER` | No | `admin` | Human admin Matrix username |
| `HICLAW_ADMIN_PASSWORD` | No | (auto-generated) | Human admin password (min 8 chars, MinIO requirement) |
| `HICLAW_MATRIX_DOMAIN` | No | `matrix-local.hiclaw.io:18080` | Matrix server domain (used inside container) |
| `HICLAW_MATRIX_CLIENT_DOMAIN` | No | `matrix-client-local.hiclaw.io` | Element Web domain |
| `HICLAW_AI_GATEWAY_DOMAIN` | No | `aigw-local.hiclaw.io` | AI Gateway domain (for LLM and MCP) |
| `HICLAW_FS_DOMAIN` | No | `fs-local.hiclaw.io` | File system domain |
| `HICLAW_PORT_GATEWAY` | No | `18080` | Host port for Higress gateway |
| `HICLAW_PORT_CONSOLE` | No | `18001` | Host port for Higress console |
| `HICLAW_PORT_ELEMENT_WEB` | No | `18088` | Host port for Element Web direct access |
| `HICLAW_GITHUB_TOKEN` | No | - | GitHub PAT for MCP Server |
| `HICLAW_WORKER_IMAGE` | No | `hiclaw/worker-agent:latest` | Worker Docker image for direct creation |
| `HICLAW_WORKSPACE_DIR` | No | `~/hiclaw-manager` | Host directory for Manager workspace (bind-mounted to `/root/manager-workspace`) |
| `HICLAW_DATA_DIR` | No | `hiclaw-data` | Docker volume name for persistent data |
| `HICLAW_MOUNT_SOCKET` | No | `1` | Mount container runtime socket for direct Worker creation |
| `HICLAW_YOLO` | No | - | Set to `1` to enable YOLO mode (autonomous decisions, no interactive prompts) |
| `HICLAW_MANAGER_RUNTIME` | No | `openclaw` | Manager engine: **`openclaw`** (default, `hiclaw-manager` image) or **`copaw`** (`hiclaw-manager-copaw` image). Hermes is supported for **Workers** only, not as a Manager runtime. |

### QwenPaw Manager (formerly CoPaw, `HICLAW_MANAGER_RUNTIME=copaw`)

When you choose the QwenPaw Manager at install time, the controller runs the **`hiclaw-manager-copaw`** image instead of the OpenClaw-based **`hiclaw-manager`**. Behavior is the same role (coordinate Workers/Teams over Matrix, drive Higress/MCP flows); only the agent engine and config layout differ (Python QwenPaw vs Node OpenClaw). Multi-channel setup and skills follow the QwenPaw workspace conventions under `/root/manager-workspace`.

### Customizing the Manager Agent

The Manager Agent's behavior is defined by three files stored in the **`hiclaw-storage`** MinIO bucket (S3 path prefix `agents/manager/`). The installer bind-mounts the host workspace to **`/root/manager-workspace`** in the Manager container, which is kept in sync with that bucket — edit either via MinIO UI/API or by editing files on the host under `HICLAW_WORKSPACE_DIR` (default `~/hiclaw-manager`).

1. **SOUL.md** - Agent identity, security rules, communication model
2. **HEARTBEAT.md** - Periodic check routine (OpenClaw heartbeat or QwenPaw equivalent, depending on runtime)
3. **AGENTS.md** - Available skills and task workflow

If your install still exposes MinIO on localhost, use the MinIO Console; otherwise use `mc` from inside **`hiclaw-controller`** or edit the mirrored files under the workspace directory on the host.

### Adding Skills

The repo ships **16** built-in Manager skills under `manager/agent/skills/` (synced into the bucket as `agents/manager/skills/<name>/SKILL.md`): **channel-management**, **file-sync-management**, **git-delegation-management**, **hiclaw-find-worker**, **human-management**, **matrix-server-management**, **mcp-server-management**, **mcporter**, **model-switch**, **project-management**, **service-publishing**, **task-coordination**, **task-management**, **team-management**, **worker-management**, **worker-model-switch**.

Place additional self-contained `SKILL.md` files under `agents/manager/skills/<skill-name>/`. The Manager runtime auto-discovers skills from that directory.

To add a new skill:
1. Create directory: `agents/manager/skills/<your-skill-name>/`
2. Write `SKILL.md` with complete API reference and examples
3. The Manager Agent will discover it automatically (~300ms)

### Managing MCP Servers

To add a new MCP Server (e.g., GitLab, Jira):

1. Configure the MCP Server in Higress Console
2. Add the MCP Server entry via Higress API: `PUT /v1/mcpServer`
3. Authorize consumers: `PUT /v1/mcpServer/consumers`
4. Create a skill for Workers that documents the available tools

## Multi-Channel Communication

The Manager supports multiple communication channels beyond the built-in Matrix DM. Admins can reach the Manager from Discord, Feishu, Telegram, or any other channel supported by OpenClaw.

### Adding a Non-Matrix Channel

1. Configure the channel in the Manager's `openclaw.json` (or `manager-openclaw.json.tmpl`) by adding a `channels.<channel>` block with the admin's user ID in `dm.allowFrom`. See [OpenClaw channel docs](https://github.com/nicepkg/openclaw) for per-channel setup.
2. Restart (or reload config) to activate the new channel.
3. Contact the Manager from that channel — it will recognize you as the admin because only allowlisted senders can reach it.

### Primary Channel

The Manager sends proactive notifications (cross-channel escalation, etc.) to the **primary channel**. By default this is Matrix DM.

**Setting the primary channel**: On the first DM from a new channel, the Manager will ask whether you want to make it the primary channel. Reply "yes" to confirm. You can also switch at any time by saying e.g. "switch primary channel to Discord".

**Stored in**: `~/hiclaw-manager/primary-channel.json` (persists across restarts)

**Fallback**: If the primary channel is unavailable or not configured, the Manager automatically falls back to Matrix DM.

### Trusted Contacts

By default, only the admin can interact with the Manager. If you want to allow another person (e.g. a teammate) to ask questions without giving them admin rights, you can add them as a **Trusted Contact**:

1. Ask them to send a message to the Manager on any configured channel.
2. Tell the Manager: "you can talk to the person who just messaged me" (or similar).
3. The Manager adds them to `~/hiclaw-manager/trusted-contacts.json`.

Trusted Contacts can receive general responses, but the Manager will **never** share sensitive information (API keys, credentials, Worker configs) with them and will not execute any management operations on their behalf.

To revoke access: "stop talking to [person]" — the Manager removes them from the list.

### Cross-Channel Escalation

When the Manager is working inside a Matrix project room and needs an urgent admin decision, it can escalate to the admin on their primary channel (e.g. send a question to your Discord DM) without requiring you to be in the Matrix room. Your reply is automatically routed back to the originating room to continue the workflow.

## Session Management

### OpenClaw Session Retention

The Manager and Worker OpenClaw instances use **type-based session policies**:

```json
"session": {
  "resetByType": {
    "dm":    { "mode": "daily", "atHour": 4 },
    "group": { "mode": "daily", "atHour": 4 }
  }
}
```

- **DM sessions** (Manager ↔ Human Admin): reset daily at 04:00.
- **Group rooms** (Worker rooms, project rooms): reset daily at 04:00, same as DM sessions.

### Session Reset Fallback

When a Worker's session is reset (context wiped due to 2 days of inactivity), the following files allow resuming any task without losing progress:

#### Progress Logs

During task execution, Workers append to a daily progress log after every meaningful action:

```
~/hiclaw-fs/shared/tasks/{task-id}/progress/YYYY-MM-DD.md
```

These files are stored in shared MinIO storage and are readable by both the Manager and other Workers. They capture completed steps, current state, issues encountered, and next planned actions — providing a full audit trail even after a session reset.

#### Task History (LRU Top 10)

Each Worker maintains a local task history file:

```
~/hiclaw-fs/agents/{worker-name}/task-history.json
```

This file records the 10 most recently active tasks (task ID, brief description, status, task directory path, last worked timestamp). When a new task pushes the count above 10, the oldest entry is archived to `history-tasks/{task-id}.json`.

#### Resuming a Task After Reset

When the Manager or Human Admin asks a Worker to resume a task after a session reset, the Worker:

1. Reads `task-history.json` (or `history-tasks/{task-id}.json` for older tasks) to locate the task directory
2. Reads `spec.md` and `plan.md` from the task directory
3. Reads recent `progress/YYYY-MM-DD.md` files (newest first) to reconstruct context
4. Continues work and appends to today's progress log

## Monitoring

### Logs

**v1.1.0+ embedded install:** Higress, Tuwunel, and MinIO run inside **`hiclaw-controller`**. The **`hiclaw-manager`** container only runs the coordinator agent; infrastructure logs are on the controller.

```bash
# Manager Agent (stdout/stderr + startup scripts)
docker logs hiclaw-manager -f
docker exec hiclaw-manager cat /var/log/hiclaw/manager-agent.log

# OpenClaw runtime log (agent events, tool calls, LLM interactions) — OpenClaw Manager only
docker exec hiclaw-manager bash -c 'cat /tmp/openclaw/openclaw-*.log' | jq .

# Infrastructure + Higress Console (embedded stack)
docker logs hiclaw-controller -f
docker exec hiclaw-controller cat /var/log/hiclaw/higress-console.log
docker exec hiclaw-controller cat /var/log/hiclaw/tuwunel.log
```

### Replay Conversation Logs

After running `make replay`, conversation logs are saved automatically:

```bash
# View the latest replay log
make replay-log

# Logs are stored in logs/replay/replay-{timestamp}.log
```

### Health Checks

```bash
# Matrix / MinIO (not published on host by default — exec into controller)
docker exec hiclaw-controller curl -sf http://127.0.0.1:6167/_matrix/client/versions
docker exec hiclaw-controller curl -sf http://127.0.0.1:9000/minio/health/live

# Higress Console (host port)
curl -s http://127.0.0.1:18001/
```

### Consoles

- **Higress Console**: http://localhost:18001 — gateway routes and consumers
- **Element Web**: http://127.0.0.1:18088 — IM (direct host port), or via gateway hostname `http://matrix-client-local.hiclaw.io:18080` when `*-local.hiclaw.io` resolves to localhost
- **MinIO**: embedded install keeps MinIO **inside** `hiclaw-controller` (no default host publish). Use `mc` from that container, the MinIO API through internal URLs, or browse objects if you add a console route yourself
- **OpenClaw control UI** (OpenClaw Manager): http://127.0.0.1:18888

## Backup and Recovery

### Data Volume

All persistent data is stored in the `hiclaw-data` Docker volume:
- Tuwunel database (Matrix history)
- MinIO storage (Agent configs, task data)
- Higress configuration

Additionally, the user's home directory can be shared with agents for file access:

#### Home Directory Sharing (Optional)
You can optionally share the user's home directory with agents:
- By default, `$HOME` is available at `/host-share` inside the container
- A symlink is created from the original host home path (e.g., `/home/zhangty`) to `/host-share`
- Agents can access and manipulate files using the same paths as on the host
- This enables seamless file access between host and agents using consistent paths
- To enable this feature, the installer will prompt for the directory to share (default: $HOME)

### Backup

```bash
docker run --rm -v hiclaw-data:/data -v $(pwd):/backup ubuntu \
  tar czf /backup/hiclaw-backup-$(date +%Y%m%d).tar.gz /data
```

### Restore

```bash
docker run --rm -v hiclaw-data:/data -v $(pwd):/backup ubuntu \
  tar xzf /backup/hiclaw-backup-YYYYMMDD.tar.gz -C /
```

### Directory Structure

The system maintains the Docker volume for persistent storage and can optionally share the host directory:

- `hiclaw-data` Docker volume: Contains all persistent system data
- Host `$HOME` directory: Optionally shared to container at `/host-share`
- Inside container: Original host path (e.g., `/home/zhangty`) via symlink to `/host-share` when available
- This provides consistent file paths between host and container environments when sharing is enabled

This allows agents to directly read and write files from the host system using identical paths when directory sharing is enabled,
facilitating file transfer and processing workflows with path consistency.

### Example Usage

```bash
# Example 1: Install with home directory sharing (recommended)
HICLAW_LLM_API_KEY=your-key-here ./install/hiclaw-install.sh manager

# Example 2: Place files in home directory for agent access
mkdir -p ~/project-inputs/
echo "Sample data" > ~/project-inputs/sample.txt

# Example 3: Agent can access files at the same path in container as on host
# Host path: /home/zhangty/project-inputs/sample.txt
# Container path: /home/zhangty/project-inputs/sample.txt (via symlink)

# Example 4: Use in agent configuration to access host files
# In agent configuration, refer to files using the same path as host:
# Host: /home/zhangty/data/input.txt
# Container: /home/zhangty/data/input.txt (identical path via symlink)
```

## YOLO Mode

YOLO mode makes the Manager operate fully autonomously — it skips all interactive admin prompts and makes reasonable decisions on its own. Intended for CI/testing and automated workflows.

### Activation

Two ways to activate (either one is sufficient):

```bash
# Option 1: environment variable at container start
docker run -e HICLAW_YOLO=1 ... hiclaw/manager-agent:latest

# Option 2: touch a file in the workspace (takes effect immediately, no restart needed)
docker exec hiclaw-manager touch /root/manager-workspace/yolo-mode
```

`make test` and `make replay` both enable YOLO mode automatically.

### Behavior

| Situation | Normal mode | YOLO mode |
|-----------|-------------|-----------|
| GitHub PAT not configured | Ask admin | Skip GitHub integration, note "GitHub not configured" |
| Other decisions requiring confirmation | Prompt admin | Make the most reasonable choice, explain in message |

YOLO mode does **not** affect security rules, Worker credential isolation, or human visibility of Agent communication.
