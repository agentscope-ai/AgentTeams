# Import Worker Guide

Import pre-configured Workers into AgentTeams, or declaratively manage Workers, Teams, and Human users.

## Overview

AgentTeams uses a thin-shell + container-internal CLI architecture for resource management:

- **`agentteams-apply.sh`** — runs on the host; copies YAML into the **`agentteams-manager`** container and runs `agt apply -f …` there.
- **`agentteams-import.sh`** — runs on the host; handles ZIP / package imports and forwards to the `agt` CLI **inside `agentteams-manager`**.
- **`agt` CLI** — present in **`agentteams-controller`**, **`agentteams-manager`**, and Worker images; talks to the controller REST API for apply/get/delete/create/update.

## Declarative YAML Management

The recommended way to manage AgentTeams resources is via YAML files.

### Create a Worker

```yaml
# worker.yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
  skills:
    - github-operations
    - git-delegation
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
```

```bash
bash agentteams-apply.sh -f worker.yaml
```

### Create a Team

A Team consists of a Leader and one or more Workers. The Leader receives tasks from the Manager and coordinates the team internally.

```yaml
# team.yaml
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  description: Full-stack development team
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
      skills: [github-operations]
    - name: alpha-qa
      model: claude-sonnet-4-6
```

```bash
bash agentteams-apply.sh -f team.yaml
```

### Add a Human User

Human users get a Matrix account and are invited into the appropriate rooms based on their permission level.

```yaml
# human.yaml
apiVersion: agentteams.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: John
  email: john@example.com       # credentials sent here after registration
  permissionLevel: 2            # 1=Admin equivalent, 2=Team-scoped, 3=Worker-only
  accessibleTeams: [alpha-team] # L2: can talk to the team's leader and all workers
  accessibleWorkers: []
  note: Frontend lead
```

```bash
bash agentteams-apply.sh -f human.yaml
```

Permission levels:
- **L1**: Equivalent to Admin — can communicate with all agents
- **L2**: Team-scoped — can communicate with specified teams' leaders and workers
- **L3**: Worker-only — can communicate with specified workers only

### Batch Import

Use `---` separators to apply multiple resources in one file:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
---
apiVersion: agentteams.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: John
  email: john@example.com
  permissionLevel: 2
  accessibleTeams: [alpha-team]
```

```bash
bash agentteams-apply.sh -f full-setup.yaml
```

`agt apply` currently supports **`-f` / `--file` only** for multi-document YAML. **`--prune`**, **`--dry-run`**, and **`--watch`** are **not implemented** — remove stale objects with `agt delete …` (from `agentteams-manager` or `agentteams-controller`) or edit resources explicitly.

### Manage Existing Resources

Inside **`agentteams-manager`** or **`agentteams-controller`** (or via `docker exec`):

```bash
# List all workers
docker exec agentteams-manager agt get workers

# Show a specific worker's config
docker exec agentteams-manager agt get worker alice

# Delete a worker
docker exec agentteams-manager agt delete worker alice
```

## Worker Package Format

A Worker package ZIP has the following structure:

```
worker-package.zip
├── manifest.json           # Package metadata (required)
├── Dockerfile              # Custom image build (optional)
├── config/
│   ├── SOUL.md             # Worker identity and role
│   ├── AGENTS.md           # Custom agent configuration
│   ├── MEMORY.md           # Long-term memory
│   └── memory/             # Memory files
├── skills/                 # Custom skills
│   └── <skill-name>/
│       └── SKILL.md
├── crons/
│   └── jobs.json           # Scheduled tasks
└── tool-analysis.json      # Tool dependency report (informational)
```

### manifest.json

```json
{
  "version": "1.0",
  "source": {
    "openclaw_version": "2026.3.x",
    "hostname": "my-server",
    "os": "Ubuntu 22.04",
    "created_at": "2026-03-18T10:00:00Z"
  },
  "worker": {
    "suggested_name": "my-worker",
    "model": "qwen3.5-plus",
    "runtime": "openclaw",
    "base_image": "agentteams/worker-agent:latest",
    "apt_packages": ["ffmpeg", "imagemagick"],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

`worker.runtime` (`openclaw`, `copaw`, or `hermes`) is honored by `agt apply worker --zip`
and overridden by an explicit `--runtime` flag. When neither is set the controller
falls back to its default runtime (`openclaw`).

## Scenario 1: Migrate a Standalone OpenClaw

If you have an existing OpenClaw instance running on a server and want to bring it under AgentTeams management as a Worker, follow these steps.

### Step 1: Install the Migration Skill on the Source OpenClaw

Copy the `migrate/skill/` directory to your OpenClaw's skills folder:

```bash
cp -r migrate/skill/ ~/.openclaw/workspace/skills/hiclaw-migrate/
```

Or ask your OpenClaw to install it:

```
Install the hiclaw-migrate skill from /path/to/hiclaw/migrate/skill/
```

### Step 2: Generate the Migration Package

Ask your OpenClaw to analyze its environment and generate the migration package:

```
Analyze my current setup and generate a AgentTeams migration package.
```

The OpenClaw will read the migration skill's instructions, understand AgentTeams's Worker architecture, and then:

1. Run `analyze.sh` to scan tool dependencies (skill scripts, shell history, cron payloads, AGENTS.md code blocks)
2. Intelligently adapt your AGENTS.md — keeping your custom role and behavior definitions while removing parts that conflict with AgentTeams's builtin Worker configuration (communication protocol, file sync, task execution rules, etc.)
3. Adapt SOUL.md for AgentTeams's Worker identity format
4. Generate a Dockerfile that extends the AgentTeams Worker base image with your required system tools
5. Package everything into a ZIP and output the file path

This step requires the OpenClaw AI to be involved — the scripts alone cannot intelligently adapt your configuration. The OpenClaw reads the SKILL.md to understand AgentTeams's conventions and makes informed decisions about what to keep, modify, or remove.

### Step 3: Review the Package (Recommended)

Before importing, review the generated files:

```bash
unzip -l /tmp/hiclaw-migration/migration-my-worker-*.zip
```

Check `tool-analysis.json` to verify the detected dependencies are correct. Edit the Dockerfile if needed.

### Step 4: Transfer and Import

Transfer the ZIP to the AgentTeams Manager host, then run:

```bash
bash agentteams-import.sh worker --name my-worker --zip migration-my-worker-20260318-100000.zip
```

The `agt` CLI inside the container will:
1. Parse `manifest.json` from the ZIP
2. Build a custom Worker image from the Dockerfile (if present)
3. Register a Matrix account and create a communication room
4. Create a MinIO user with scoped permissions
5. Configure Higress Gateway consumer and route authorization
6. Generate openclaw.json and push all config to MinIO
7. Update the Manager's workers-registry.json
8. Send a message to the Manager to start the Worker container

### Step 5: Verify

After the script completes, check the Worker in Element Web. The Manager will start the container and the Worker should appear online within a minute.

### What Gets Migrated

| Item | Migrated | Notes |
|------|----------|-------|
| SOUL.md / AGENTS.md | Yes | Adapted for AgentTeams format |
| Custom skills | Yes | Placed in `skills/` |
| Cron jobs | Yes | Converted to AgentTeams scheduled tasks |
| Memory files | Yes | MEMORY.md and daily notes |
| System tool dependencies | Yes | Installed via custom Dockerfile |
| API keys / auth profiles | No | AgentTeams uses its own AI Gateway credentials |
| Device identity | No | New identity generated during registration |
| Conversation sessions | No | Sessions reset daily in AgentTeams |
| Discord/Slack channel config | No | AgentTeams uses Matrix |

## Scenario 2: Import a Worker Template

Worker templates are pre-built packages that define a Worker's role, skills, and tool dependencies. They can be shared within a team or published to the community.

### Import from a Local ZIP

```bash
bash agentteams-import.sh worker --name devops-alice --zip devops-worker-template.zip
```

### Import from a URL

```bash
bash agentteams-import.sh worker --name devops-alice --zip https://example.com/templates/devops-worker.zip
```

### Import from a Remote Package (Nacos)

```bash
bash agentteams-import.sh worker --name devops-alice --package nacos://host:8848/namespace/devops/v1
bash agentteams-import.sh worker --name devops-alice --package nacos://host:8848/namespace/devops/label:latest
```

### Create a Worker Without a Package

Create a Worker directly with a model and optional built-in skills, no ZIP needed:

```bash
bash agentteams-import.sh worker --name bob --model claude-sonnet-4-6 \
    --skills github-operations,git-delegation \
    --mcp-servers github
```

Or via YAML (preferred for repeatable deployments):

```bash
bash agentteams-apply.sh -f worker.yaml
```

### Creating a Worker Template

To create a shareable Worker template:

1. Create a `manifest.json`:

```json
{
  "version": "1.0",
  "source": {
    "hostname": "template",
    "os": "N/A",
    "created_at": "2026-03-18T00:00:00Z"
  },
  "worker": {
    "suggested_name": "devops-worker",
    "base_image": "agentteams/worker-agent:latest",
    "apt_packages": [],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

2. Create `config/SOUL.md` with the Worker's role definition:

```markdown
# DevOps Worker

## AI Identity

**You are an AI Agent, not a human.**

## Role
- Name: devops-worker
- Specialization: CI/CD pipeline management, infrastructure monitoring, deployment automation
- Skills: GitHub Operations, shell scripting, Docker, Kubernetes

## Behavior
- Monitor CI/CD pipelines proactively
- Alert on failures immediately
- Automate routine deployment tasks
```

3. Optionally add `config/AGENTS.md` with custom instructions, `skills/` with custom skill definitions, and a `Dockerfile` if extra tools are needed.

4. Package it:

```bash
cd my-template-dir/
zip -r devops-worker-template.zip manifest.json config/ skills/ Dockerfile
```

## Command Reference

### agentteams-import.sh (Bash — macOS/Linux)

```bash
bash agentteams-import.sh worker --name <name> [options]
bash agentteams-import.sh -f <resource.yaml>   # forwards to agentteams-apply.sh (same flags as apply)
```

**Worker import mode:**

| Option | Description | Default |
|--------|-------------|---------|
| `--name <name>` | Worker name (required) | — |
| `--zip <path\|url>` | ZIP package (local path or URL) | — |
| `--package <uri>` | Remote package URI (`nacos://`, `http://`, `oss://`) | — |
| `--model <model>` | LLM model ID | `qwen3.5-plus` |
| `--skills <s1,s2>` | Comma-separated built-in skills | — |
| `--mcp-servers <m1,m2>` | Comma-separated MCP servers | — |
| `--runtime <runtime>` | Agent runtime (`openclaw`\|`copaw`\|`hermes`) | `openclaw` |
| `--yes` | Skip interactive confirmations (swallowed by wrapper when unsupported) | off |

**YAML mode** (`-f`): delegates to `agentteams-apply.sh` (only `-f` is required; extra unsupported flags are rejected by `agt apply`).

### agentteams-import.ps1 (PowerShell — Windows)

```powershell
.\agentteams-import.ps1 worker -Name <name> [-Zip <path-or-url>] [-Package <uri>] [-Model MODEL] [-Skills s1,s2] [-McpServers m1,m2] [-Runtime rt] [-Yes]
.\agentteams-import.ps1 -File <resource.yaml>
```

Parameters mirror the Bash version (no `-Prune`/`-DryRun` on YAML path).

### agentteams-apply.sh (Bash — macOS/Linux)

```bash
bash agentteams-apply.sh -f <resource.yaml> [-- additional args passed to agt apply]
```

| Option | Description | Default |
|--------|-------------|---------|
| `-f <path>` | YAML resource file (required) | — |

The install script header may mention **`--prune` / `--dry-run` / `--watch`** — those are **not** implemented in `agt apply` today; use explicit deletes instead.

## Troubleshooting

### Import script fails at "Checking Manager container"

The AgentTeams Manager container must be running. Start it with:

```bash
docker start agentteams-manager
```

### Image build fails

Check the Dockerfile in the ZIP package. Common issues:
- Package names may differ between Ubuntu versions
- pip/npm packages may have been renamed or removed

You can edit the Dockerfile in the extracted ZIP and retry.

### Worker starts but doesn't respond

1. Check Worker container logs: `docker logs agentteams-worker-<name>`
2. Verify the Worker appears in Element Web in its dedicated room
3. Ensure the Manager's `workers-registry.json` has the correct entry
4. Try sending `@<worker-name>:<matrix-domain> hello` in the Worker's room
