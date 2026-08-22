# Declarative Resource Management

HiClaw uses Kubernetes CRD-style declarative YAML to manage all platform resources — Workers, Teams, and Humans. You describe the desired state, and the HiClaw Controller handles creation, updates, and deletion automatically.

## Core Concepts

### Organization Structure

HiClaw uses a three-tier organization that maps to real enterprise team structures:

```
Admin (Human administrator)
  │
  ├── Manager (AI Agent, management entry point)
  │     ├── Team Leader A (special Worker, coordinates team tasks)
  │     │     ├── Worker A1
  │     │     └── Worker A2
  │     ├── Team Leader B
  │     │     └── Worker B1
  │     └── Worker C (standalone Worker, not part of any Team)
  │
  └── Human Users (real people, access based on permission level)
        ├── Level 1: Admin-equivalent, can talk to all roles
        ├── Level 2: Can talk to specified Teams' Leaders + Workers
        └── Level 3: Can only talk to specified Workers
```

### Three Resource Types

| Resource | Description | Underlying Entity |
|----------|-------------|-------------------|
| Worker | AI Agent execution unit | Docker container + Matrix account + MinIO space |
| Team | Collaboration group with Leader + N Workers | A set of Worker containers + Team Room |
| Human | Real human user | Matrix account + Room permissions |

All resources share a unified API version: `apiVersion: hiclaw.io/v1beta1`.

## Worker

A Worker is the basic execution unit in HiClaw — an AI Agent running in a Docker container with its own Matrix communication account and MinIO storage space.

### Basic Configuration

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6        # LLM model
  identity: |                      # Worker public identity (generates IDENTITY.md)
    - Name: Alice
    - Specialization: DevOps, CI/CD pipeline management
  soul: |                          # Worker personality and values (generates SOUL.md)
    # Alice - DevOps Worker
    ## Personality
    - Methodical and detail-oriented, always double-checks before deploying
    - Proactive about potential risks, raises concerns early
    - Prefers automation over manual processes
    ## Values
    - Stability first: never sacrifice reliability for speed
    - Transparency: always explain what you're doing and why
  agents: |                        # Agent behavior rules (generates AGENTS.md)
    ## Behavior
    - Monitor CI/CD pipelines proactively
    - Alert on failures immediately
  skills:                          # HiClaw built-in skills
    - github-operations
    - git-delegation
  mcpServers:                      # HiClaw built-in MCP Servers (authorized via Higress gateway)
    - github
```

### Field Reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `metadata.name` | string | Yes | — | Worker name, globally unique |
| `spec.model` | string | Yes | — | LLM model ID, e.g. `claude-sonnet-4-6`, `qwen3.5-plus` |
| `spec.runtime` | string | No | `openclaw` | Agent runtime: `openclaw` or `copaw` |
| `spec.image` | string | No | `hiclaw/worker-agent:latest` | Custom Docker image |
| `spec.identity` | string | No | — | Worker public identity, used to generate IDENTITY.md |
| `spec.soul` | string | No | — | Worker personality and values (generates SOUL.md) |
| `spec.agents` | string | No | — | Agent behavior rules, used to generate AGENTS.md |
| `spec.skills` | []string | No | — | Built-in skills, distributed by Manager |
| `spec.mcpServers` | []string | No | — | Built-in MCP Servers, authorized via Higress gateway |
| `spec.package` | string | No | — | Custom package URI: `file://`, `http(s)://`, or `nacos://` |

### identity / soul / agents vs package

There are two ways to configure a Worker's identity and behavior:

- **Inline**: Define `spec.identity`, `spec.soul`, and `spec.agents` directly in the YAML. The Controller generates the corresponding IDENTITY.md, SOUL.md, and AGENTS.md. Best for lightweight configurations.
- **Package**: Provide a ZIP via `spec.package` containing the full config (IDENTITY.md, SOUL.md, AGENTS.md, custom skills, Dockerfile, etc.). Best for complex setups requiring custom skills or system dependencies.

When both are set, inline fields override the corresponding files in the package. This allows you to use a package as a base template while customizing specific aspects via YAML — for example, importing a shared package but overriding `soul` to give the Worker a unique role definition.

### Built-in Skills vs Custom Skills

`spec.skills` refers to HiClaw platform built-in capabilities, distributed by the Manager via `push-worker-skills.sh` to the Worker's MinIO space.

For custom skills, use `spec.package` to provide a ZIP containing a `skills/` directory. Built-in and custom skills are merged without conflict.

### Worker with Custom Package

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: devops-alice
spec:
  model: claude-sonnet-4-6
  runtime: openclaw
  skills: [github-operations]
  mcpServers: [github]
  package: file://./devops-alice.zip    # Contains custom SOUL.md, skills, Dockerfile, etc.
```

### Worker Creation Flow

When the Controller receives a Worker resource, it executes:

1. Resolve `spec.package` (if present) — download and extract to a temp directory
2. Register a Matrix account and create a communication Room (Manager + Admin + Worker)
3. Create a MinIO user and bucket, configure Higress gateway authorization
4. Generate `openclaw.json` config (including `groupAllowFrom` permission matrix)
5. Push all config files (SOUL.md, skills, crons, etc.) to MinIO
6. Update `workers-registry.json`
7. Start the Worker container

### Worker Status

| Phase | Meaning |
|-------|---------|
| Pending | Resource created, waiting for Controller to process |
| Running | Container running, Agent online |
| Stopped | Container stopped |
| Failed | Creation or runtime failure — check `status.message` |

## Team

A Team is HiClaw's collaboration unit, consisting of one Team Leader and one or more Team Workers. The Manager delegates tasks to the Team Leader, who handles decomposition, assignment, and aggregation — achieving team-level autonomy.

### Basic Configuration

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  description: Full-stack development team
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
    soul: |
      # Alpha Lead - Team Leader
      ## Personality
      - Calm and organized, keeps the team focused on priorities
      - Patient with team members, encourages open communication
      ## Values
      - Clarity: every task must have clear acceptance criteria before assignment
      - Trust: delegate fully, don't micromanage
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
      skills: [github-operations]
      mcpServers: [github]
      soul: |
        # Alpha Dev - Backend Developer
        ## Personality
        - Pragmatic problem-solver, favors simple solutions over clever ones
        - Thorough code reviewer, catches edge cases early
        ## Values
        - Code quality: write tests before shipping
        - Keep it simple: avoid premature abstraction
    - name: alpha-qa
      model: claude-sonnet-4-6
      soul: |
        # Alpha QA - QA Engineer
        ## Personality
        - Skeptical by nature, always asks "what could go wrong?"
        - Meticulous about reproducing and documenting issues
        ## Values
        - User experience first: test from the user's perspective
        - No silent failures: every bug gets a clear report
```

### Field Reference

**Team-level fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `metadata.name` | string | Yes | Team name, globally unique |
| `spec.description` | string | No | Team description |
| `spec.admin` | object | No | Team admin (defaults to global Admin) |
| `spec.leader` | object | Yes | Team Leader configuration |
| `spec.workers` | []object | Yes | Team Worker list |

**Leader fields:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `leader.name` | string | Yes | Leader name |
| `leader.model` | string | No | LLM model |
| `leader.identity` | string | No | Leader public identity (generates IDENTITY.md) |
| `leader.soul` | string | No | Leader personality and values (generates SOUL.md) |
| `leader.agents` | string | No | Custom behavior rules (appended after builtin AGENTS.md) |
| `leader.package` | string | No | Custom package URI |

**Worker fields (same as standalone Worker spec):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `workers[].name` | string | Yes | Worker name |
| `workers[].model` | string | No | LLM model |
| `workers[].runtime` | string | No | Agent runtime |
| `workers[].identity` | string | No | Worker public identity (generates IDENTITY.md) |
| `workers[].soul` | string | No | Worker personality and values (generates SOUL.md) |
| `workers[].agents` | string | No | Custom behavior rules (appended after builtin AGENTS.md) |
| `workers[].skills` | []string | No | Built-in skills |
| `workers[].mcpServers` | []string | No | Built-in MCP Servers |
| `workers[].package` | string | No | Custom package URI |

### What Makes Team Leader Special

A Team Leader is essentially a Worker container, but with key differences:

- Uses the `team-leader-agent` template (SOUL.md.tmpl + AGENTS.md + HEARTBEAT.md)
- Has the `team-task-management` skill (manages team-state.json, finds available Workers)
- Does NOT have Manager-exclusive skills like `worker-management` or `mcp-server-management`
- Marked as `role: "team_leader"` in `workers-registry.json`
- Follows a delegation-first principle — always assigns tasks to team Workers, never executes domain tasks itself

### Team Leader AGENTS.md Assembly

The Team Leader's AGENTS.md is assembled in three layers, each managed independently:

```
<!-- hiclaw-builtin-start -->
[Builtin: Team Leader workspace rules, task flow, skills reference]
<!-- hiclaw-builtin-end -->

<!-- hiclaw-team-context-start -->
## Coordination
- Upstream coordinator: @manager:{domain}
- Team Admin: @admin:{domain}
- Team: alpha-team
- Team members: alpha-dev, alpha-qa
<!-- hiclaw-team-context-end -->

[User-provided content from spec.agents (if any)]
```

- The builtin section is auto-managed by HiClaw and updated on upgrades
- The team context is auto-injected with the team name, members, and coordinator info
- User-provided `spec.agents` content is placed after both sections and preserved across updates

### Room Topology

Creating a Team produces the following Matrix Rooms:

```
Leader Room:   Manager + Global Admin + Leader        ← Manager-to-Leader communication channel
Team Room:     Leader + Team Admin + W1 + W2 + ...    ← Leader-to-Workers collaboration space
Worker Room:   Leader + Team Admin + Worker           ← Leader-to-individual-Worker private chat
Leader DM:     Team Admin ↔ Leader                    ← Team management channel
```

Key design: the Team Room does NOT include the Manager, establishing a delegation boundary. The Manager communicates with the Leader only through the Leader Room and never reaches into the team directly.

### Task Flow

```
Admin assigns task → Manager
  ↓
Manager determines the task matches a Team's domain
  ↓
Manager creates task spec, @mentions Leader
  ↓
Leader decomposes into sub-tasks, assigns to team Workers
  ↓
Workers complete execution, @mention Leader
  ↓
Leader aggregates results, @mentions Manager
  ↓
Manager notifies Admin
```

### Team Status

| Phase | Meaning |
|-------|---------|
| Pending | Resource created, waiting for Controller to process |
| Active | Leader and all Workers running |
| Degraded | Some Workers unavailable, Leader still running |

### Team Admin

You can assign a dedicated admin (Team Admin) for a Team, replacing the global Admin for team management:

```yaml
spec:
  admin:
    name: pm-zhang
    matrixUserId: "@pm-zhang:domain"
```

If not specified, the global Admin is used by default. The Team Admin is invited to the Team Room and Leader DM, and can communicate directly with the Leader on team matters.

## Human

A Human resource represents a real person. Upon creation, a Matrix account is automatically registered and the user is invited to the appropriate Rooms based on their permission level, enabling human-AI collaboration.

### Basic Configuration

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: John Doe
  email: john@example.com
  permissionLevel: 2
  accessibleTeams: [alpha-team]
  accessibleWorkers: []
  note: Frontend lead
```

### Field Reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `metadata.name` | string | Yes | — | User identifier, globally unique |
| `spec.displayName` | string | Yes | — | Display name |
| `spec.email` | string | No | — | Email for sending credentials |
| `spec.permissionLevel` | int | Yes | — | Permission level: 1, 2, or 3 |
| `spec.accessibleTeams` | []string | No | — | Accessible Team list (effective for L2) |
| `spec.accessibleWorkers` | []string | No | — | Accessible standalone Worker list (effective for L2/L3) |
| `spec.note` | string | No | — | Notes |

### Three-Level Permission Model

Permission levels are inclusive — higher levels include all permissions of lower levels.

**Level 1 — Admin Equivalent**

Can talk to all roles in the system, including Manager, all Team Leaders, and all Workers. `accessibleTeams` and `accessibleWorkers` fields are ignored.

Use case: CTO, VP of Engineering.

```yaml
spec:
  permissionLevel: 1
```

**Level 2 — Team-Scoped**

Can talk to specified Teams' Leaders and all their Workers, plus specified standalone Workers.

Use case: Product manager, team member.

```yaml
spec:
  permissionLevel: 2
  accessibleTeams: [alpha-team, beta-team]
  accessibleWorkers: [standalone-dev]
```

**Level 3 — Worker-Only**

Can only talk to specified Workers. `accessibleTeams` field is ignored.

Use case: External collaborator, specialized staff.

```yaml
spec:
  permissionLevel: 3
  accessibleWorkers: [alice, bob]
```

### How Permissions Work

Human permissions are enforced through two mechanisms:

1. **Room invitations**: The Human is invited to the corresponding Matrix Rooms
2. **groupAllowFrom**: The Human's Matrix ID is added to the `openclaw.json` config of the corresponding Agents — Agents only respond to @mentions from whitelisted users

| Level | groupAllowFrom Changes | Room Invitations |
|-------|----------------------|------------------|
| L1 | Added to Manager + all Leaders + all Workers | All Rooms |
| L2 | Added to specified Teams' Leaders + Workers + specified standalone Workers | Specified Team Rooms + Worker Rooms |
| L3 | Added to specified Workers | Specified Worker Rooms |

### Human Creation Flow

1. Register a Matrix account (random password auto-generated)
2. Calculate which Agents need modification based on permissionLevel
3. Update `groupAllowFrom` in each affected Agent's `openclaw.json`
4. Invite the Human to the corresponding Rooms
5. Update `humans-registry.json`
6. Push updated configs to MinIO, notify Agents to `file-sync`
7. Send a welcome email (if SMTP and email are configured)

### Automatic Welcome Email

When `spec.email` is set and SMTP is configured, a welcome email is automatically sent after the Human account is created, containing all the information needed to log in:

```
Subject: Welcome to HiClaw - Your Account Details

Hi {displayName},

Your HiClaw account has been created:

  Username: {matrix_user_id}
  Password: {generated_password}
  Login URL: {element_web_url}

Please log in and change your password immediately.

— HiClaw
```

SMTP is configured via environment variables in the Manager container:

| Variable | Description |
|----------|-------------|
| `HICLAW_SMTP_HOST` | SMTP server address |
| `HICLAW_SMTP_PORT` | SMTP port |
| `HICLAW_SMTP_USER` | SMTP username |
| `HICLAW_SMTP_PASS` | SMTP password |
| `HICLAW_SMTP_FROM` | Sender address |

If SMTP is not configured or `spec.email` is empty, email sending is skipped without affecting account creation. The initial password is still recorded in `status.initialPassword` and can be retrieved via `hiclaw get human <name>`.

### Notes

- Humans don't need containers, MinIO spaces, or Higress authorization — only a Matrix account and Room permissions
- Target Teams must exist before creating an L2 Human
- Target Workers must exist before creating an L3 Human
- Changing permissionLevel triggers a full recalculation of groupAllowFrom

## Package URI

Both Workers and Team Workers support custom configuration packages via `spec.package`. Three URI formats are supported:

| Format | Example | Description |
|--------|---------|-------------|
| `file://` | `file://./alice.zip` | Local file, transferred via `docker cp` |
| `http(s)://` | `https://example.com/worker.zip` | Remote download |
| `nacos://` | `nacos://instance-xxx/ns/agent-spec/worker-xxx/v1` | Pulled from Nacos config center |

Nacos URI format: `nacos://{instance-id}/{namespace}/{group}/{data-id}/{version}`

### Package Directory Structure

Regardless of URI format, the extracted package follows a unified structure:

```
{package}/
├── manifest.json           # Package metadata (required)
├── Dockerfile              # Custom image build (optional)
├── config/
│   ├── SOUL.md             # Worker identity and role definition
│   ├── AGENTS.md           # Agent behavior rules
│   ├── MEMORY.md           # Long-term memory
│   └── memory/             # Memory files directory
├── skills/                 # Custom skills
│   └── <skill-name>/
│       └── SKILL.md
└── crons/
    └── jobs.json           # Scheduled tasks
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
    "apt_packages": ["ffmpeg"],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

## Operations

### hiclaw-apply.sh — Declarative Apply (Recommended)

Runs on the host, forwarding YAML to the `hiclaw` CLI inside the Manager container:

```bash
# Create/update a single resource
bash hiclaw-apply.sh -f worker.yaml

# Batch create (use --- separators in YAML)
bash hiclaw-apply.sh -f company-setup.yaml

# Full sync (delete resources not in YAML)
bash hiclaw-apply.sh -f company-setup.yaml --prune

# Preview changes
bash hiclaw-apply.sh -f company-setup.yaml --dry-run
```

| Option | Description |
|--------|-------------|
| `-f <path>` | YAML resource file (required) |
| `--prune` | Delete resources not present in YAML |
| `--dry-run` | Show changes without applying |
| `--yes` | Skip delete confirmation |

### hiclaw-import.sh — Imperative Import

For importing Workers from ZIP packages:

```bash
# Import from local ZIP
bash hiclaw-import.sh worker --name alice --zip ./alice.zip

# Import from URL
bash hiclaw-import.sh worker --name alice --zip https://example.com/alice.zip

# Import from Nacos
bash hiclaw-import.sh worker --name alice --package nacos://instance-xxx/ns/agent-spec/alice/v1

# Create without a package
bash hiclaw-import.sh worker --name bob --model claude-sonnet-4-6 \
    --skills github-operations,git-delegation --mcp-servers github
```

### hiclaw CLI — In-Container Management

Operate directly inside the Manager container (or via `docker exec`):

```bash
# List all resources
docker exec hiclaw-manager hiclaw get workers
docker exec hiclaw-manager hiclaw get teams
docker exec hiclaw-manager hiclaw get humans

# View a single resource
docker exec hiclaw-manager hiclaw get worker alice

# Delete a resource
docker exec hiclaw-manager hiclaw delete worker alice
docker exec hiclaw-manager hiclaw delete team alpha-team
docker exec hiclaw-manager hiclaw delete human john
```

### HTTP API — Cloud Management

The `hiclaw-controller` includes a built-in HTTP API Server (`:8090`) for cloud management platforms:

```
POST   /api/v1/apply                    # Incremental apply (body is YAML)
POST   /api/v1/apply?prune=true         # Full sync
GET    /api/v1/workers                   # List all Workers
GET    /api/v1/teams                     # List all Teams
GET    /api/v1/humans                    # List all Humans
DELETE /api/v1/workers/alice             # Delete a specific resource
```

> **Note:** In the current single-container deployment mode, port 8090 is NOT exposed to the host — it is only accessible from within the Manager container. In the future K8s deployment mode (`HICLAW_KUBE_MODE=incluster`), the controller will be deployed as a standalone Pod, exposing this API via a Kubernetes Service.

## Batch Deployment

Use `---` separators to define all resources in a single YAML file and deploy an entire organization in one apply.

Execution order is handled automatically by the Controller: create order is Team → Worker → Human; delete order is Human → Worker → Team.

```yaml
# company-setup.yaml

# --- Team definitions ---
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: product-team
spec:
  description: Product development team
  leader:
    name: product-lead
    model: claude-sonnet-4-6
  workers:
    - name: backend-dev
      model: claude-sonnet-4-6
      skills: [github-operations, git-delegation]
      mcpServers: [github]
    - name: frontend-dev
      model: claude-sonnet-4-6
      skills: [github-operations]
    - name: qa-engineer
      model: claude-sonnet-4-6
---
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: ops-team
spec:
  description: Operations team
  leader:
    name: ops-lead
    model: claude-sonnet-4-6
  workers:
    - name: monitor
      model: claude-sonnet-4-6
---
# --- Standalone Worker ---
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: admin-assistant
spec:
  model: claude-sonnet-4-6
---
# --- Human users ---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: zhang-san
spec:
  displayName: Zhang San
  email: zhangsan@example.com
  permissionLevel: 2
  accessibleTeams: [product-team]
  note: Product manager
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: li-si
spec:
  displayName: Li Si
  email: lisi@example.com
  permissionLevel: 2
  accessibleTeams: [product-team]
  note: Backend developer
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: wang-wu
spec:
  displayName: Wang Wu
  email: wangwu@example.com
  permissionLevel: 3
  accessibleWorkers: [admin-assistant]
  note: Administrative staff
```

One-command deployment:

```bash
bash hiclaw-apply.sh -f company-setup.yaml
```

For subsequent changes, just edit the YAML and re-apply. Use `--prune` to automatically clean up removed resources:

```bash
bash hiclaw-apply.sh -f company-setup.yaml --prune
```

## Controller Architecture

### Processing Flow

```
Entry point (hiclaw-apply.sh / HTTP API / hiclaw CLI)
  ↓
YAML written to MinIO hiclaw-config/{kind}/{name}.yaml
  ↓
mc mirror syncs to local filesystem (10-second interval)
  ↓
fsnotify detects file changes → parses YAML → writes to kine (SQLite)
  ↓
controller-runtime informer detects changes → triggers Reconciler
  ↓
Reconciler executes scripts (create-worker.sh / create-team.sh / create-human.sh)
```

### Reconciler Actions

| Reconciler | CREATE | UPDATE | DELETE |
|-----------|--------|--------|--------|
| Worker | Create container + Matrix account + MinIO space | model change → regenerate config; skills change → re-push | Stop container + clean up resources |
| Team | Create Leader + Workers + Team Room | workers list change → add/remove Workers | Delete Workers → Leader → Team Room |
| Human | Register Matrix account + configure permissions + send email | permissionLevel change → recalculate groupAllowFrom | Remove from all groupAllowFrom → kick from Rooms |

All resources use the Kubernetes finalizer pattern to ensure cleanup before deletion.

### Two Deployment Modes

| Dimension | embedded (default) | incluster (K8s) |
|-----------|--------------------|-----------------|
| Config storage | MinIO `hiclaw-config/` | K8s etcd (CRDs stored directly in K8s) |
| Controller detection | fsnotify → kine → informer | controller-runtime watches K8s API directly |
| Switch via | `HICLAW_KUBE_MODE=embedded` | `HICLAW_KUBE_MODE=incluster` |

## Communication Permission Matrix

HiClaw uses the `groupAllowFrom` field in `openclaw.json` to control which @mentions each Agent accepts, enabling fine-grained communication permissions.

| Role | groupAllowFrom includes |
|------|------------------------|
| Manager | Admin, all Team Leaders, all standalone Workers, Human L1 |
| Team Leader | Manager, Admin, all team Workers, Human L1, Human L2 for this Team |
| Team Worker | Leader, Admin, Human L1, Human L2 for this Team, specified Human L3 |
| Standalone Worker | Manager, Admin, Human L1, specified Human L2/L3 |

Key rules:
- Manager does not penetrate Teams — communicates only with the Leader, never directly with team Workers
- Team Workers only recognize their Leader — groupAllowFrom does not include Manager
- Permissions are inclusive — Human L1 > L2 > L3, higher levels include all lower-level permissions
- Standalone Workers maintain the existing pattern — communicate directly with Manager

## FAQ

**Q: Can Teams and standalone Workers coexist?**

Yes. Teams and standalone Workers coexist in the same HiClaw instance. The Manager decides whether to delegate to a Team Leader or assign directly to a standalone Worker based on the task domain.

**Q: What happens when a Human's permissionLevel is changed?**

The Controller recalculates the Human's groupAllowFrom across all affected Agents, removes old permissions, adds new ones, and updates Room invitations.

**Q: Can a Team Worker belong to multiple Teams?**

No. Each Worker can only belong to one Team (or be a standalone Worker).

**Q: What if the target Team doesn't exist yet when creating an L2 Human?**

The Controller marks the Human as Pending and automatically backfills permissions once the target Team is created.

**Q: Does `--prune` delete all resources not in the YAML?**

Yes. `--prune` compares the resources in the YAML against the current state and deletes extras. Deletion order is Human → Worker → Team to respect dependencies. Use `--dry-run` first to preview changes.
