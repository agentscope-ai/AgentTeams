# Worker Guide

Guide for deploying, managing, and troubleshooting AgentTeams Worker Agents.

## Overview

Workers are lightweight, stateless containers that:
- Connect to the Manager via Matrix for task communication
- Sync configuration from centralized MinIO storage
- Use AI Gateway for LLM access
- Use mcporter CLI for MCP Server tool calls (GitHub, etc.)

### Declarative create / update (v1.1.0+)

Workers are **CRD-backed**. Besides asking the Manager in Matrix, you can:

- Run **`agt create worker` / `agt update worker`** inside `agentteams-controller` or `agentteams-manager` (see [faq.md](troubleshooting/faq.md)).
- Apply YAML with **`install/agentteams-apply.sh`** (forwards to `agt apply -f` in the Manager container).

Full field reference: [Declarative Resource Management](resource-management.md).

### Filesystem layout by `spec.runtime`

| Runtime | Primary workspace | Notes |
|---------|-------------------|--------|
| **openclaw** | `/root/agentteams-fs/agents/<worker-name>/` (`HOME` points here) | `openclaw.json`, `SOUL.md`, `AGENTS.md`, skills, `.openclaw/` live under this tree. Shared data: `/root/agentteams-fs/shared/`. |
| **copaw** | `/root/.agentteams-worker/<worker-name>/` (runtime config in `.copaw/`) | Legacy compatibility path. A symlink **`/root/agentteams-fs`** → the per-worker tree keeps scripts that assume OpenClaw-style paths working. |
| **qwenpaw** | `/root/agentteams-fs/agents/<worker-name>/` (QwenPaw config in `.qwenpaw/`) | QwenPaw 2.x path. When switching from `copaw`, persisted state is restored before the legacy `.copaw/` state is migrated to `.qwenpaw/`. Shared data: `/root/agentteams-fs/shared/`. |
| **hermes** | `/root/agentteams-fs/agents/<worker-name>/` (`HOME` equals workspace, same mirror root as OpenClaw) | Hermes policy/state under **`.hermes/`** inside that directory (e.g. `.hermes/config.yaml`, `state.db`). |

The Controller contains an OpenHuman backend, but the shipped Worker CRD enum does not currently accept an explicit `spec.runtime: openhuman`. Until that contract is fixed in a separate business-code change, do not create an OpenHuman Worker through the normal Worker CR flow.

## Installation

Workers are created by the Manager Agent **or** the controller via declarative APIs. The Manager handles all infrastructure setup (Matrix account, Higress consumer, config files, etc.) and can either create the Worker container directly or provide a command for manual execution.

### Method 1: Direct Creation (Recommended for Local Development)

If the Manager has access to the host's container runtime socket (default when using `make install`), it can create Worker containers directly:

1. Tell Manager: "Create a new Worker named alice for frontend dev. Create it directly."
2. Manager creates all infrastructure and starts the container automatically
3. No manual steps needed

### Method 2: Docker Run Command (for Manual or Edge Deployment)

If the Manager doesn't have socket access, it will reply with a `docker run` command:

1. Tell Manager: "Create a new Worker named alice for frontend dev"
2. Manager creates infrastructure and provides a `docker run` command
3. Copy and run the command on the target host:

```bash
docker run -d --name agentteams-worker-alice \
  -e AGENTTEAMS_WORKER_NAME=alice \
  -e AGENTTEAMS_FS_ENDPOINT=http://<MANAGER_HOST>:9000 \
  -e AGENTTEAMS_FS_ACCESS_KEY=<ACCESS_KEY> \
  -e AGENTTEAMS_FS_SECRET_KEY=<SECRET_KEY> \
  agentteams/worker-agent:latest
```

The Manager will provide all the specific values in its reply.

## Installing Skills on a Worker

There are currently two stable ways to install a Skill on an existing Worker:

| Method | Best for | Persistent result |
|---|---|---|
| Distribute through the Manager | Let the Manager validate, distribute, and maintain a declarative assignment record | Uploads the complete Skill and updates `Worker.spec.skills` |
| Distribute through the Dashboard | Select one or more target Workers in the UI when you already have a ZIP | Uploads the complete Skill, updates `Worker.spec.skills`, and triggers a reload |

Both methods write the complete Skill to the Worker's canonical persistent storage before adding its name to `spec.skills`. The Worker runtime synchronizes and loads the files from that storage. The methods differ mainly in their entry point and where the distributable source is maintained: the Manager keeps a source copy under `worker-skills/`, while the Dashboard can distribute directly from its catalog or an uploaded ZIP.

### Method 1: Distribute through the Manager

Provide the Skill to the Manager in either of these ways:

1. Put the complete third-party skill under `$AGENTTEAMS_WORKSPACE_DIR/worker-skills/<skill-name>/` on the Manager host. The default is `~/agentteams-manager/worker-skills/<skill-name>/`; or
2. Send the Manager a ZIP attachment containing one complete Skill root with `SKILL.md` and any optional `scripts/` or `references/`.

Then ask the Manager to install that Skill for a named Worker and verify the assignment. For a ZIP attachment, explicitly ask the Manager to safely extract and validate it before distribution.

For example:

> Install the `alert-fusion` skill from `~/worker-skills/alert-fusion/` for Worker `amy-ai`. Verify that the files were uploaded and the Worker assignment was updated.

Or, after sending the ZIP attachment:

> Install the Skill from the ZIP attachment I just sent for Worker `amy-ai`. Safely extract and validate it, distribute the complete Skill, and verify the Worker assignment.

The Manager uploads and verifies the files before updating `Worker.spec.skills`. This ordering prevents a Worker from receiving an assignment that points to missing content. QwenPaw Workers then synchronize the assigned skill into their native workspace and refresh and enable it automatically.

You can check the assignment by asking the Manager:

> List the skills currently assigned to Worker `amy-ai` and confirm whether `alert-fusion` is included.

For operator-side inspection or troubleshooting, use the equivalent CLI query:

```bash
agt get workers amy-ai -o json | jq '.skills'
```

`agt get workers amy-ai -o json | jq '.skills'` shows the declarative `spec.skills` assignment record. Manager and Dashboard distribution both update it. Verify the actual files and runtime availability separately as described below.

### Method 2: Distribute through the Dashboard

This method requires an enabled Dashboard with credentials for the AgentTeams object store. Embedded installations created by the AgentTeams Bash installer configure this connection automatically.

#### Prepare the Skill ZIP

The upload package must meet these requirements:

- The filename ends in `.zip`, and the archive is no larger than 64 MB.
- The archive contains one complete Skill root. `SKILL.md` may be at the ZIP root or inside that Skill root directory.
- `SKILL.md` starts with YAML frontmatter containing non-empty `name` and `description` fields.
- `name` contains only letters, numbers, dots, underscores, and hyphens, and starts with a letter or number.
- Include `scripts/`, `references/`, and other supporting files with `SKILL.md`; the Dashboard preserves the complete file structure under the Skill root.

#### Distribute from the Resource Center marketplace

1. Open the Dashboard and select **资源中心 (Resource Center) → 市场 (Marketplace)** in the left navigation.
2. If the Skill is not yet in the marketplace, click **上传技能 (Upload Skill)** in the upper-right corner, choose the Skill ZIP, click **解析预览 (Parse Preview)** to confirm its name and description, and then click **上传 (Upload)**.
3. Find the target Skill in the marketplace and click the send icon labeled **分发到 Worker (Distribute to Worker)** in that row's action area.
4. Select one or more target Workers in the **分发技能到 Worker (Distribute Skill to Worker)** dialog.
5. Click **分发到 N 个 Worker (Distribute to N Workers)** and wait for each Worker's upload, assignment, and reload result.

You can also upload a Skill ZIP directly to one Worker through **Workers → target Worker → 详情 (Details) → 上传技能包 (Upload Skill Package)**.

When creating or editing a Worker, you can also select existing marketplace or Nacos Skills in the form's **技能 (Skills)** field. After saving the Worker, the Dashboard synchronizes any missing complete Skill packages to that Worker's canonical persistent directory, reconciles `spec.skills`, and then attempts to restart the Worker. Upload or `spec.skills` update failures are shown as partial failures rather than reported as successful installations.

> **Note:** **上传技能 (Upload Skill)** only adds a Skill to the Dashboard's centralized marketplace; it does not distribute that Skill to a Worker. After uploading, click **分发到 Worker (Distribute to Worker)** in that Skill's row, use **上传技能包 (Upload Skill Package)** in Worker details for a direct upload, or select the Skill while creating or editing a Worker.

#### Load and verify

The Dashboard validates the ZIP and `SKILL.md`, preserves all files under the Skill root, and writes them to `agents/<worker-name>/skills/<skill-name>/` in object storage. Marketplace and Worker-details distribution update `Worker.spec.skills` only after the complete package is present. When you select a Skill while creating or editing a Worker, the Dashboard saves the Worker first, then synchronizes any missing package and reconciles that field. Once the files and declarative assignment are in place, it attempts to restart the Worker so the runtime loads the new assignment immediately. Distribute only while the Worker is idle to avoid interrupting an active task.

- On a successful reload, the UI shows the installation or restart result for that Worker.
- If the restart is not confirmed, the uploaded files and assignment remain available. The UI reports a partial failure, and a subsequent Controller reconcile can still make the assignment effective without another upload.

Reopen **Workers → target Worker → 详情 (Details)** and confirm the Skill under **已分发技能 (Distributed Skills)**. To verify runtime availability rather than only file presence, ask that Worker to confirm it can discover and use the Skill.

Dashboard distribution updates `Worker.spec.skills`, so the Skill should appear in `agt get workers <name> -o json | jq '.skills'`. A later Controller reconcile may ask the Manager to restore a declared Skill, but the Manager is not required to keep a source copy for Dashboard-distributed Skills. Manager recovery failure is ignored while the canonical Worker copy exists; if that copy is missing and the Manager cannot restore it, the Worker receives a non-blocking warning. For a remote Skill assignment, failure to refresh the requested version or label also produces a non-blocking warning while retaining any existing canonical copy. The warning identifies the Skill and requested version or label, but omits remote source credentials.

For an end-to-end example that starts with packaging and verifies distribution, runtime discovery, and actual use, follow [Use case 6: Add and use a custom Skill](use-cases.md#8-use-case-6-add-and-use-a-custom-skill).

## Troubleshooting

### Worker won't start

```bash
# Check container logs
docker logs agentteams-worker-alice

# Common issues:
# - "openclaw.json not found": Manager hasn't created config yet
# - "mc: command not found": Image build issue
# - Connection refused: Manager container not running or ports not exposed
```

### Worker can't connect to Matrix

```bash
# Verify Matrix server is reachable from Worker (via gateway port)
docker exec agentteams-worker-alice curl -sf http://matrix-local.agentteams.io:18080/_matrix/client/versions

# Check Worker's openclaw.json for correct Matrix config
docker exec agentteams-worker-alice cat /root/agentteams-fs/agents/alice/openclaw.json | jq '.channels.matrix'
```

### Worker can't access LLM

```bash
# Test AI Gateway access with Worker's key
# Note: these commands run inside the Worker container where domain names resolve to Manager's internal IP
docker exec agentteams-worker-alice curl -sf \
  -H "Authorization: Bearer $(jq -r '.models.providers."agentteams-gateway".apiKey' /root/agentteams-fs/agents/alice/openclaw.json)" \
  http://aigw-local.agentteams.io:8080/v1/models

# If 401: Check that Worker's consumer key in openclaw.json matches the one in Higress.
# If 403: Worker may not be authorized for the AI route. Ask Manager to add.
```

### Worker can't access MCP (GitHub)

```bash
# Test mcporter connectivity (run inside Worker container)
docker exec agentteams-worker-alice mcporter --transport http \
  --server-url "http://aigw-local.agentteams.io:8080/mcp-servers/mcp-github/mcp" \
  --header "Authorization=Bearer <WORKER_KEY>" \
  call list_repos '{"owner": "test"}'

# If 403: Worker not authorized for this MCP Server. Ask Manager.
```

### Resetting a Worker

```bash
# Stop and remove the container
docker stop agentteams-worker-alice
docker rm agentteams-worker-alice

# Then ask Manager to recreate the Worker:
# "Please recreate the alice worker container"
# Manager will re-run create-worker.sh which regenerates credentials and restarts the container
```

> Note: Worker config and task data live in MinIO, not in the container. Removing the container does not lose any work.

## Lifecycle Management

The Manager automatically manages Worker container lifecycle:

- **Auto-stop**: Idle Workers (no active finite tasks) are stopped after a configurable timeout to save resources
- **Auto-start**: When a task is assigned to a stopped Worker, the Manager wakes it up before sending the task
- **Auto-recreate on restart**: When the Manager container restarts, it checks all registered Workers and recreates any whose container is missing or whose Manager IP has changed

You can also manually control Workers by asking the Manager:
- "Stop the alice worker"
- "Start the alice worker"
- "What is the status of all workers?"

## Architecture Details

### Startup Sequence

Each runtime has its own entrypoint, but they all perform the following work:

1. Obtain object-storage credentials and restore configuration and persistent state from `agents/<name>/`.
2. Prepare the runtime-specific workspace, agent prompt files, and skills.
3. Translate model, Matrix channel, MCP Server, and Team context into the runtime's native configuration.
4. Start file synchronization or the runtime configuration update loop.
5. Launch the selected OpenClaw, CoPaw, QwenPaw, or Hermes runtime.

See [Filesystem layout by `spec.runtime`](#filesystem-layout-by-specruntime) above for the concrete paths. When troubleshooting, use the logs and configuration paths for the selected runtime instead of applying OpenClaw's `openclaw.json` consumption model to every Worker.

### File Sync

- **Local to Remote**: Real-time via `mc mirror --watch`
- **Remote to Local**: Periodic pull every 5 minutes

### Config Hot-Reload

When Manager updates Worker's config in MinIO:
1. MinIO receives the updated file
2. mc mirror pulls it to Worker's local filesystem (next 5-min cycle, or immediately if Manager pushes)
3. OpenClaw detects file change (~300ms) and hot-reloads config

### Environment Variables

| Variable | Description |
|----------|-------------|
| `AGENTTEAMS_WORKER_NAME` | Worker identifier (e.g., `alice`) |
| `AGENTTEAMS_MATRIX_URL` | Matrix Homeserver URL (e.g., `http://matrix-local.agentteams.io:18080`) |
| `AGENTTEAMS_AI_GATEWAY_URL` | AI Gateway URL (e.g., `http://aigw-local.agentteams.io:18080`) |
| `AGENTTEAMS_FS_ENDPOINT` | MinIO endpoint URL (e.g., `http://<MANAGER_HOST>:9000`) |
| `AGENTTEAMS_FS_BUCKET` | Bucket name for non-default storage layouts |
| `AGENTTEAMS_FS_ACCESS_KEY` | MinIO access key (Worker-specific, generated by Manager) |
| `AGENTTEAMS_FS_SECRET_KEY` | MinIO secret key (Worker-specific, generated by Manager) |

> All values are generated by the Manager and provided in the `docker run` command or set automatically during direct creation. You should not need to set these manually.
>
> Runtime scripts now use `AGENTTEAMS_MATRIX_URL` and `AGENTTEAMS_AI_GATEWAY_URL` directly; legacy aliases are no longer part of the main contract.

### Syncing Files Manually

Inside the Worker container, run `agentteams-sync` to pull the latest config and skill files from MinIO:

```bash
docker exec agentteams-worker-alice agentteams-sync
```

This is useful after the Manager pushes updated skills or config to MinIO and you want to apply them immediately without waiting for the next sync cycle.
