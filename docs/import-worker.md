# Import Worker Guide

Import pre-configured Workers into HiClaw, or declaratively manage Workers, Teams, and Human users.

## Overview

HiClaw uses a thin-shell + container-internal CLI architecture for resource management:

- **`hiclaw-apply.sh`** — runs on the host; forwards declarative YAML to `hiclaw apply` inside the Manager container. The primary way to create and manage Workers, Teams, and Humans.
- **`hiclaw-import.sh`** — runs on the host; handles ZIP package imports and forwards to the `hiclaw` CLI inside the Manager container.
- **`hiclaw` CLI** — runs inside the Manager container; handles all resource management (apply, get, delete).

## Declarative YAML Management

The recommended way to manage HiClaw resources is via YAML files.

### Create a Worker

```yaml
# worker.yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
  skills:
    - github-operations
    - git-delegation
  mcpServers:
    - github
```

```bash
bash hiclaw-apply.sh -f worker.yaml
```

### Create a Team

A Team consists of a Leader and one or more Workers. The Leader receives tasks from the Manager and coordinates the team internally.

```yaml
# team.yaml
apiVersion: hiclaw.io/v1beta1
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
bash hiclaw-apply.sh -f team.yaml
```

### Add a Human User

Human users get a Matrix account and are invited into the appropriate rooms based on their permission level.

```yaml
# human.yaml
apiVersion: hiclaw.io/v1beta1
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
bash hiclaw-apply.sh -f human.yaml
```

Permission levels:
- **L1**: Equivalent to Admin — can communicate with all agents
- **L2**: Team-scoped — can communicate with specified teams' leaders and workers
- **L3**: Worker-only — can communicate with specified workers only

### Batch Import

Use `---` separators to apply multiple resources in one file:

```yaml
apiVersion: hiclaw.io/v1beta1
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
apiVersion: hiclaw.io/v1beta1
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
bash hiclaw-apply.sh -f full-setup.yaml
```

### Full Sync (Prune)

To synchronize a complete desired state, deleting resources not in the YAML:

```bash
bash hiclaw-apply.sh -f company-setup.yaml --prune
```

### Manage Existing Resources

Inside the Manager container (or via `docker exec`):

```bash
# List all workers
docker exec hiclaw-manager hiclaw get workers

# Show a specific worker's config
docker exec hiclaw-manager hiclaw get worker alice

# Delete a worker
docker exec hiclaw-manager hiclaw delete worker alice
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
    "base_image": "hiclaw/worker-agent:latest",
    "apt_packages": ["ffmpeg", "imagemagick"],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

## Scenario 1: Migrate a Standalone OpenClaw

If you have an existing OpenClaw instance running on a server and want to bring it under HiClaw management as a Worker, follow these steps.

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
Analyze my current setup and generate a HiClaw migration package.
```

The OpenClaw will read the migration skill's instructions, understand HiClaw's Worker architecture, and then:

1. Run `analyze.sh` to scan tool dependencies (skill scripts, shell history, cron payloads, AGENTS.md code blocks)
2. Intelligently adapt your AGENTS.md — keeping your custom role and behavior definitions while removing parts that conflict with HiClaw's builtin Worker configuration (communication protocol, file sync, task execution rules, etc.)
3. Adapt SOUL.md for HiClaw's Worker identity format
4. Generate a Dockerfile that extends the HiClaw Worker base image with your required system tools
5. Package everything into a ZIP and output the file path

This step requires the OpenClaw AI to be involved — the scripts alone cannot intelligently adapt your configuration. The OpenClaw reads the SKILL.md to understand HiClaw's conventions and makes informed decisions about what to keep, modify, or remove.

### Step 3: Review the Package (Recommended)

Before importing, review the generated files:

```bash
unzip -l /tmp/hiclaw-migration/migration-my-worker-*.zip
```

Check `tool-analysis.json` to verify the detected dependencies are correct. Edit the Dockerfile if needed.

### Step 4: Transfer and Import

Transfer the ZIP to the HiClaw Manager host, then run:

```bash
bash hiclaw-import.sh worker --name my-worker --zip migration-my-worker-20260318-100000.zip
```

The `hiclaw` CLI inside the container will:
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
| SOUL.md / AGENTS.md | Yes | Adapted for HiClaw format |
| Custom skills | Yes | Placed in `skills/` |
| Cron jobs | Yes | Converted to HiClaw scheduled tasks |
| Memory files | Yes | MEMORY.md and daily notes |
| System tool dependencies | Yes | Installed via custom Dockerfile |
| API keys / auth profiles | No | HiClaw uses its own AI Gateway credentials |
| Device identity | No | New identity generated during registration |
| Conversation sessions | No | Sessions reset daily in HiClaw |
| Discord/Slack channel config | No | HiClaw uses Matrix |

## Scenario 2: Import a Worker Template

Worker templates are pre-built packages that define a Worker's role, skills, and tool dependencies. They can be shared within a team or published to the community.

### Import from a Local ZIP

```bash
bash hiclaw-import.sh worker --name devops-alice --zip devops-worker-template.zip
```

### Import from a URL

```bash
bash hiclaw-import.sh worker --name devops-alice --zip https://example.com/templates/devops-worker.zip
```

### Import from a Remote Package (Nacos)

```bash
bash hiclaw-import.sh worker --name devops-alice --package nacos://instance-xxx/namespace/agent-spec/devops/v1
```

### Create a Worker Without a Package

Create a Worker directly with a model and optional built-in skills, no ZIP needed:

```bash
bash hiclaw-import.sh worker --name bob --model claude-sonnet-4-6 \
    --skills github-operations,git-delegation \
    --mcp-servers github
```

Or via YAML (preferred for repeatable deployments):

```bash
bash hiclaw-apply.sh -f worker.yaml
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
    "base_image": "hiclaw/worker-agent:latest",
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

### hiclaw-import.sh (Bash — macOS/Linux)

```bash
bash hiclaw-import.sh worker --name <name> [options]
bash hiclaw-import.sh -f <resource.yaml> [--prune] [--dry-run]
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
| `--runtime <runtime>` | Agent runtime (`openclaw`\|`copaw`) | `openclaw` |
| `--dry-run` | Show changes without applying | off |
| `--yes` | Skip interactive confirmations | off |

**YAML mode** (`-f`): delegates to `hiclaw-apply.sh`.

### hiclaw-import.ps1 (PowerShell — Windows)

```powershell
.\hiclaw-import.ps1 worker -Name <name> [-Zip <path-or-url>] [-Package <uri>] [-Model MODEL] [-Skills s1,s2] [-McpServers m1,m2] [-Runtime rt] [-DryRun] [-Yes]
.\hiclaw-import.ps1 -File <resource.yaml> [-Prune] [-DryRun]
```

Parameters mirror the Bash version.

### hiclaw-apply.sh (Bash — macOS/Linux)

```bash
bash hiclaw-apply.sh -f <resource.yaml> [options]
```

| Option | Description | Default |
|--------|-------------|---------|
| `-f <path>` | YAML resource file (required) | — |
| `--prune` | Delete resources not in YAML | off |
| `--dry-run` | Show changes without applying | off |
| `--yes` | Skip delete confirmation when pruning | off |

## Troubleshooting

### Import script fails at "Checking Manager container"

The HiClaw Manager container must be running. Start it with:

```bash
docker start hiclaw-manager
```

### Image build fails

Check the Dockerfile in the ZIP package. Common issues:
- Package names may differ between Ubuntu versions
- pip/npm packages may have been renamed or removed

You can edit the Dockerfile in the extracted ZIP and retry.

### Worker starts but doesn't respond

1. Check Worker container logs: `docker logs hiclaw-worker-<name>`
2. Verify the Worker appears in Element Web in its dedicated room
3. Ensure the Manager's `workers-registry.json` has the correct entry
4. Try sending `@<worker-name>:<matrix-domain> hello` in the Worker's room
