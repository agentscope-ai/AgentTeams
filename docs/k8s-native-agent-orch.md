# HiClaw: Kubernetes-native multi-Agent collaboration orchestration

## 1. Positioning

HiClaw is an open-source **collaborative multi-Agent OS**: a declarative orchestration plane for multiple AI Agents working together.

Unlike a single-Agent runtime, HiClaw targets one question: **when autonomous Agents must behave like a real team on complex work, how do you orchestrate organization, communication policy, delegation, and shared state?**

HiClaw borrows Kubernetes ideas—declarative APIs, controller reconcile loops, CRD-style extension—and builds a control plane for Agent *teams*. You declare desired structure in YAML; the controller wires infrastructure and communication topology.

## 2. Why multi-Agent collaboration orchestration

### 2.1 From one Agent to an Agent team

The ecosystem is moving from “lone operators” to “team play”:

| Stage | Characteristics | Examples |
|------|-----------------|----------|
| Single Agent | One Agent completes tasks alone | OpenClaw, Cursor, Claude Code |
| Multi-Agent orchestration | Many Agents run independently; unified lifecycle | NVIDIA NemoClaw |
| Multi-Agent **collaboration** | Agents form teams with structure, protocols, shared state | **HiClaw** |

Single-Agent ceilings come from context and tooling. Beyond that boundary you need division of labor—but “many Agents running” ≠ “many Agents collaborating”:

- **Orchestration**: lifecycle, resources, isolation—*how to run* many Agents.
- **Collaboration**: org structure, who may message whom, delegation, shared state—*how they work together*.

HiClaw focuses on collaboration.

### 2.2 Parallels to the Kubernetes journey

| Container world | Agent world | Question answered |
|----------------|------------|-------------------|
| Docker | OpenClaw / Claude Code | How to run one isolated unit |
| Docker Compose | NemoClaw (single-Agent sandbox ops) | How to manage lifecycle and config |
| **Kubernetes** | **HiClaw** | How many units form a coherent system |

As Kubernetes sits on top of Docker without replacing it, HiClaw sits on top of Agent runtimes and adds collaboration orchestration.

## 3. Core architecture

### 3.1 Three-tier organization

HiClaw maps enterprise-style structure:

```
Admin (human administrator)
  │
  ├── Manager (AI coordinator; optional deployment pattern)
  │     ├── Team Leader A (special Worker; in-team scheduling)
  │     │     ├── Worker A1
  │     │     └── Worker A2
  │     ├── Team Leader B
  │     │     └── Worker B1
  │     └── Worker C (standalone Worker, not in a Team)
  │
  └── Human users (real people, permission tiers)
        ├── Level 1: Admin-equivalent, can talk to all roles
        ├── Level 2: Talk to configured Teams’ Leaders + Workers (+ standalone Workers)
        └── Level 3: Talk only to configured standalone Workers
```

Design principles:

- **A Team Leader is still a Worker**: same container/runtime class; different SOUL and skills—like control-plane and worker nodes both running kubelet.
- **The Manager does not penetrate Teams**: it talks to the Team Leader only, not to in-team Workers—delegation boundary; avoids bottlenecks.
- **Declarative comms policy**: `groupAllowFrom` gates @mentions; use CRD **`channelPolicy`** (`groupAllowExtra` / `groupDenyExtra` / `dmAllowExtra` / `dmDenyExtra`) to add or deny on top of defaults.

### 3.2 Declarative resources (CRD-style)

Four core kinds share `apiVersion: hiclaw.io/v1beta1`:

```
apiVersion: hiclaw.io/v1beta1
```

#### Worker — execution unit

**Naming:** The Python Worker runtime is **QwenPaw** (image `hiclaw-copaw-worker`). Older materials sometimes used **CoPaw** for the same runtime.

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6           # required: LLM model
  runtime: copaw                     # openclaw | copaw | hermes (default from install / CR)
  skills: [github-operations]        # platform built-in skills
  mcpServers:                        # MCP servers callable via mcporter
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http                # "http" (default) or "sse"
  package: file://./alice-pkg.zip    # optional: file/http(s)/nacos/packages/…
  soul: |                            # persona
    You are a frontend-focused engineer...
  expose:                            # ports published via Gateway
    - port: 3000
      protocol: http
  # state: Running                   # desired lifecycle: Running | Sleeping | Stopped
  # channelPolicy:                   # optional: allow/deny extras on group + DM defaults
  #   groupAllowExtra: ["@human:domain"]
```

Each Worker maps to: a Docker container (or K8s Pod) + Matrix account + MinIO namespace + Gateway Consumer token. If `spec.image` is omitted, defaults come from `HICLAW_WORKER_IMAGE` / `HICLAW_COPAW_WORKER_IMAGE` / `HICLAW_HERMES_WORKER_IMAGE` (or chart defaults).

#### Team — collaboration unit

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: frontend-team
spec:
  description: "Frontend development team"
  peerMentions: true                  # default true: Workers may @mention each other in team rooms
  # channelPolicy: …                  # optional team-wide overrides (same shape as Worker)
  # admin:                             # optional human Team Admin (name from humans-registry)
  #   name: pm-zhang
  #   matrixUserId: "@pm:domain"
  leader:
    name: frontend-lead
    model: claude-sonnet-4-6
    heartbeat:
      enabled: true
      every: 10m
    workerIdleTimeout: 720m
    # state: Running                  # optional desired lifecycle for Leader
  workers:
    - name: alice
      model: claude-sonnet-4-6
      skills: [github-operations]
      mcpServers:
        - name: github
          url: https://gateway.example.com/mcp-servers/github/mcp
    - name: bob
      model: qwen3.5-plus
      runtime: copaw
      skills: [github-operations]
      # expose / channelPolicy / state etc. align with standalone Worker fields
```

When a Team is created, the controller wires this topology (if `spec.admin` is set, “Admin” means **Team Admin**; otherwise **global Admin**):

```
Leader Room:  Manager + Global Admin + Leader    ← Manager talks only to Leader
Team Room:    Leader + Admin + W1 + W2 + …       ← Manager is NOT here (delegation boundary)
Worker Room:  Leader + Admin + Worker             ← private Leader↔member channel
Leader DM:    Admin ↔ Leader                     ← team alignment / management
```

**Team Room excludes the Manager**; the Leader decomposes work inside the team. Which humans join which rooms follows Human permissions and `spec.admin`.

#### Human — real user

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: "John Doe"
  email: john@example.com
  permissionLevel: 2                  # 1=Admin-equiv, 2=Team-scoped, 3=Worker-only
  accessibleTeams: [frontend-team]
  accessibleWorkers: [devops-alice]
```

#### Manager — coordinator (CR)

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Manager
metadata:
  name: default                       # common name for the primary instance in embedded installs
spec:
  model: claude-sonnet-4-6            # required
  runtime: openclaw                   # openclaw | copaw
  # soul: | …                         # optional SOUL.md override
  # agents: | …                       # optional AGENTS.md override
  skills: [worker-management]         # on-demand Manager skills
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
  # package: https://…/mgr.zip       # optional; same URI semantics as Worker
  config:
    heartbeatInterval: 15m
    workerIdleTimeout: 720m
    notifyChannel: admin-dm
  # state: Running                    # Running | Sleeping | Stopped
```

`Manager` is the same API group/version as `Worker` / `Team` / `Human` and is reconciled by the same controller. **Whether you “need” chat with the Manager Agent is a usage choice**: CLI / REST / YAML-only workflows avoid the chat entrypoint; default installs still run a Manager container whose desired config can be declared and reconciled via this CR.

**kubectl short names** (after CRDs are installed): `wk`, `tm`, `hm`, `mgr`.

### 3.3 Controller architecture

HiClaw follows the standard Kubernetes controller pattern.

**Declarative apply**: On the host, `install/hiclaw-apply.sh` copies YAML into the Manager container and runs `hiclaw apply -f`. The CLI issues REST calls **in YAML document order** (`POST`/`PUT` `/api/v1/workers`, `/teams`, `/humans`, `/managers`) and **does not** topologically sort dependencies—put depended-on resources first (e.g. `Team` before `Human` referencing `accessibleTeams`). **`--prune` and `--dry-run` are not implemented** in the current CLI (may differ from comments in some install scripts; trust the CLI).

```
Declarative YAML
    ↓ hiclaw apply
kine (etcd-compatible, SQLite backend) / native K8s etcd
    ↓ Informer watch
controller-runtime
    ↓ Reconcile loop
┌─────────────────────────────────────────────┐
│ Provisioner                                 │
│ - Matrix registration & rooms               │
│ - MinIO user & bucket                       │
│ - Higress Consumer & routes                 │
│ - K8s ServiceAccount (incluster)            │
├─────────────────────────────────────────────┤
│ Deployer                                    │
│ - Package fetch (file/http(s)/nacos/packages/…) │
│ - openclaw.json (incl. comms matrix)        │
│ - Push SOUL.md / AGENTS.md / skills         │
│ - Start container / create Pod              │
├─────────────────────────────────────────────┤
│ Worker backend abstraction                  │
│ - Docker (embedded)                         │
│ - Kubernetes (incluster)                    │
│ - Cloud-hosted                              │
└─────────────────────────────────────────────┘
```

Deployment modes:

| Mode | State store | Workers run as | Typical use |
|------|-------------|----------------|-------------|
| Embedded | kine + SQLite | Docker containers | Dev / small teams |
| Incluster | K8s etcd | Pods | Enterprise / cloud |

**Embedded vs Helm (packaging):**

- **Embedded** — `install/hiclaw-install.sh` starts **`hiclaw-controller`** (image bundles Higress, Tuwunel, MinIO, Element Web, and the controller binary). The controller then creates **`hiclaw-manager`** and each **Worker** as separate containers on the same Docker/Podman host.
- **Helm / in-cluster** — Chart [`helm/hiclaw`](../helm/hiclaw) deploys the same logical components as Kubernetes workloads (gateway, homeserver, storage, controller Deployment, and Manager/Worker Pods from CRs). CRD semantics match embedded; only the backend driver differs.

Both modes share reconcilers; backends mirror how Kubernetes abstracts CRI/CSI/CNI.

### 3.4 Matrix as the collaboration layer

HiClaw uses Matrix instead of a bespoke RPC bus:

| Concern | Why Matrix |
|---------|------------|
| Transparency | Agent traffic is visible in rooms; humans can watch live |
| Human-in-the-loop | Same IM client; @mention any Agent anytime |
| Open protocol | Federated design; less lock-in |
| Audit | Persistent history |
| Clients | Element, FluffyChat, mobile |

Tuwunel is bundled as a high-performance homeserver for single-container installs.

### 3.5 LLM/MCP security via Higress

The security layer is **[Higress](https://github.com/alibaba/higress)**—a **CNCF Sandbox** Envoy-based AI Gateway with LLM proxying, MCP hosting, and per-consumer auth. Together with HiClaw, LLM and MCP access can be policy-driven for every Agent.

#### Principle: real secrets never ship to Agents

```
Worker (holds only Consumer Token / GatewayKey)
    → Higress AI Gateway
        ├── key-auth WASM validates token
        ├── Consumer must be on Route allowedConsumers
        ├── inject real credential (API key / PAT / OAuth)
        └── proxy upstream
            ├── LLM APIs
            ├── MCP servers (GitHub, Jira, …)
            └── other services
```

**Real credentials live in the Gateway**; the Agent holds a revocable Consumer token only.

#### LLM path

For each Worker the controller typically:

1. Generates a Consumer token (GatewayKey).
2. Registers `worker-{name}` in Higress with key-auth.
3. Adds that Consumer to AI Routes’ `allowedConsumers`.

```
POST https://aigw-local.hiclaw.io/v1/chat/completions
Authorization: Bearer {GatewayKey}
```

The Worker’s `openclaw.json` points at the Gateway, not raw provider URLs.

#### MCP path

```
POST https://aigw-local.hiclaw.io/mcp-servers/github/mcp
Authorization: Bearer {GatewayKey}
```

Central MCP registration + per-Consumer `allowedConsumers` + mcporter config pointing at Gateway endpoints.

#### Fine-grained control

| Dimension | Mechanism | Example |
|-----------|-----------|---------|
| Per-Worker LLM | AI Route allowedConsumers | Worker A: GPT-4; Worker B: GPT-3.5 only |
| Per-Worker MCP | MCP allowedConsumers | Worker A: GitHub MCP; Worker B: none |
| Change at runtime | Edit allowedConsumers | Revoke without rotating upstream secrets |
| Fast revoke | Remove from list | WASM hot reload (~seconds) |

Analogous to ServiceAccount + RBAC: Consumer token ≈ SA token; `allowedConsumers` ≈ policy.

#### vs NemoClaw (security angle)

| Capability | NemoClaw | HiClaw + Higress |
|------------|----------|------------------|
| Credential isolation | OpenShell intercepts inference | Gateway proxy; Worker never sees API keys |
| MCP centralization | Not built-in | Higress-hosted MCP + unified auth |
| Per-Agent differentiation | Per-sandbox config | Shared Gateway, per-Consumer routes |
| Dynamic policy | Often rebuild sandbox | Edit allowedConsumers; fast rollout |
| OS sandbox | Landlock + seccomp + netns | Docker today (can combine with NemoClaw) |
| Egress policy | Fine allowlists | Gateway routing tier |

Complementary: NemoClaw excels OS-level single-Agent isolation; Higress excels multi-Agent API/MCP policy.

#### Why Higress

- AI-native gateway (multi-provider LLM routes, limits, fallback; MCP hosting).
- WASM plugins (key-auth hot reload).
- Envoy core (performance, Prometheus/OTel).
- Discovery modes (Nacos, K8s, DNS) for embedded and incluster.

### 3.6 Shared state and MinIO

```
MinIO (S3-compatible)
├── agents/                    # Per-Agent config space
│   ├── alice/
│   │   ├── SOUL.md
│   │   ├── openclaw.json
│   │   └── skills/
│   └── bob/
├── shared/
│   ├── tasks/
│   │   └── task-{id}/
│   │       ├── meta.json
│   │       ├── spec.md       # Manager / Leader
│   │       └── result.md     # Workers
│   └── knowledge/
└── workers/                   # Artifacts
```

Workers are stateless at the container edge: config is pulled from object storage; containers can be recreated like stateless Pods with shared persistence behind them.

## 4. Collaboration flows

### 4.1 Inside a Team

```
Admin: "Ship login feature front + back"
  ↓
Manager: routes to frontend team, @mentions Team Leader
  ↓
Team Leader: splits work
  ├── Subtask 1: login API → @ Worker A
  ├── Subtask 2: login UI → @ Worker B
  └── Subtask 3: integration tests → after 1+2
  ↓
Workers report in Team Room; Leader aggregates
  ↓
Leader @mentions Manager with summary
  ↓
Manager notifies Admin
```

Everything stays in Matrix rooms—Admin can intervene anytime.

### 4.2 Human-in-the-loop

```
[Team Room]
Leader: @alice implement password rules (min 8 chars)
Alice: On it...

Admin observes and intervenes:
Admin: @alice hold on—min 12 chars, mixed case + symbols
Alice: Updated.
Leader: I'll refresh the task spec.
```

No hidden Agent-to-Agent side channels—auditable by design.

## 5. Comparison with NVIDIA NemoClaw

### 5.1 Positioning

| Dimension | NemoClaw | HiClaw |
|-----------|----------|--------|
| Focus | Single-Agent sandbox safety | Multi-Agent **collaboration** orchestration |
| Problem | Run one Agent safely | Many Agents as a structured team |
| Shape | One Agent per sandbox | Manager → Leader → Workers |
| Between Agents | Isolated | Declarative comms matrix + rooms |
| Shared state | Per-sandbox workspace | MinIO + task flow |
| Humans | Single operator | Multi-role, 3-tier Human CRD |
| Config | Blueprint YAML + wizard | CRD-style YAML + reconcile |

### 5.2 Architecture sketches

**NemoClaw**

```
NemoClaw CLI → onboard → OpenShell
    ├── Sandbox A (OpenClaw)
    ├── Sandbox B (Hermes)
    └── Sandbox C (OpenClaw)
No cross-sandbox chat, no shared coordinator.
```

**HiClaw**

```
HiClaw Controller
    ↓
Matrix: Manager ↔ Leaders ↔ Workers; standalone Workers ↔ Manager
MinIO shared state
Higress security
Human tiers in the same rooms
```

### 5.3 Capability matrix

| Capability | NemoClaw | HiClaw |
|------------|----------|--------|
| Lifecycle | Sandbox CRUD/recover | Reconcile + containers/Pods |
| OS sandbox | Strong | Docker (NemoClaw optional) |
| LLM secrets | OpenShell intercept | Gateway + Consumer token |
| MCP | Not centralized | Higress MCP + allowedConsumers |
| Dynamic policy | Rebuild sandboxes often | Edit allowedConsumers |
| Agent-to-Agent | None | Matrix + room topology |
| Delegation | None | Manager → Leader → Worker |
| Teams / Humans | None | Team + Human CRDs |
| Declarative | Single-Agent blueprint | Worker/Team/Human/Manager |
| K8s-native deploy | No | Incluster + Helm |
| Runtimes | OpenClaw, Hermes, … | OpenClaw, QwenPaw, Hermes, ZeroClaw*, NanoClaw* |

\* Roadmap / lightweight options (see project README).

### 5.4 Complementary futures

```
┌────────────────────────────────────┐
│ HiClaw — collaboration layer        │
│ org / comms / delegation / state  │
├────────────────────────────────────┤
│ NemoClaw — sandbox runtime layer    │
│ isolation / routing / policy        │
├────────────────────────────────────┤
│ OpenClaw / QwenPaw / … — Agent engines│
└────────────────────────────────────┘
```

The Worker backend could one day plug NemoClaw under each Worker—HiClaw orchestrates teams; NemoClaw hardens each unit—like Kubernetes and any CRI runtime.

## 6. Stack

| Piece | Choice | Notes |
|-------|--------|------|
| Controller | Go + controller-runtime | Standard kube builder style |
| State | kine (SQLite) / etcd | Embedded vs incluster |
| Comms | Matrix (Tuwunel) | Self-hosted |
| IM UI | Element Web | Browser client |
| Files | MinIO | S3 API |
| AI Gateway | Higress (CNCF Sandbox) | LLM + MCP + consumer auth |
| Runtimes | OpenClaw, QwenPaw, … | From large to lightweight images |
| Skills | skills.sh ecosystem | Large community catalog |
| MCP CLI | mcporter | Calls through Gateway |

## 7. Kubernetes mapping

| Kubernetes | HiClaw | Notes |
|------------|--------|-------|
| Pod | Worker | Smallest schedulable unit; replaceable |
| Deployment | Team | Desired set of collaborating Workers |
| Service | Matrix room | Collaboration “endpoint” abstraction |
| SA + RBAC | Consumer + allowedConsumers | Identity + fine-grained routes |
| CRD | Worker/Team/Human/Manager | Declarative API |
| CR short names | `wk` / `tm` / `hm` / `mgr` | After CRD install |
| Controller | hiclaw-controller | Reconcile loop |
| kubectl apply | hiclaw apply | `apply -f` walks multi-doc YAML in order |

## 8. Deployment modes

See **section 3.3** for how the controller reconciles; this section is only *how you install*.

### 8.1 Embedded (dev / small teams)

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

Rough minimum: 2 CPU, 4 GB RAM, Docker/Podman. You get **`hiclaw-controller`** (infra + controller) plus a separate **`hiclaw-manager`** container; Workers appear as additional containers when created.

### 8.2 In-cluster / Helm (enterprise / cloud)

```bash
# From repository root (chart lives under helm/hiclaw)
helm install hiclaw ./helm/hiclaw
```

You can also install from a published Helm chart once the repo is added. The chart wires **`hiclaw-controller`**, gateway, homeserver, and storage per `values.yaml`; Manager and Worker Pods follow the same CRD API as embedded installs.

## 9. Status and roadmap

- **2026-03-04**: Open sourced, Apache 2.0.
- **Shipped**: OpenClaw/QwenPaw, MCP integration, Team + Human model.
- **In progress**: ZeroClaw (Rust ultra-light), NanoClaw (minimal LOC runtime)—see README for current state.
- **Planning**: Team admin dashboard, deeper incluster/Helm story, optional NemoClaw-style sandbox under Workers.

## 10. Community

- GitHub: https://github.com/higress-group/hiclaw
- Discord: https://discord.gg/NVjNA4BAVw
- License: Apache 2.0
