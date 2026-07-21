# HiClaw: Kubernetes-native multi-agent collaboration orchestration

When a single AI Agent runs out of context window and tooling headroom for real-world complexity, the obvious next step is “multi-agent”—multiple autonomous Agents working in parallel. But **running several runtimes is not the same as having a team**. Without explicit organization there is no stable delegation; without communication policy there is no controlled, auditable messaging between roles; without shared state and a unified gateway strategy it is hard to plug in LLMs and MCP safely.

HiClaw aims to make multi-agent **collaboration** a **declarable, reconcilable, operable** control plane: it borrows Kubernetes ideas—declarative APIs, controller reconcile loops, CRD-style resources—and treats Worker, Team, Human, and Manager as first-class specs. `hiclaw-controller` continuously converges real infrastructure and communication topology to what you declare in YAML.

This post summarizes HiClaw’s role, core components, and trade-offs so you can place it in your mental map of cloud-native AI infrastructure.

---

## 1. Orchestration vs collaboration: two different “multi-agent” problems

“Multi-agent” in the wild often mixes two goals:

- **Orchestration**: who starts, stops, and upgrades Agents; how images and resources are managed—the heart of **lifecycle and runtime isolation**.
- **Collaboration**: reporting lines, who may @mention whom, how work is split and rolled up, where shared artifacts live—the heart of **org structure, communication boundaries, and shared state**.

Kubernetes addresses the first (how workloads run at scale). HiClaw does not replace OpenClaw / CoPaw-style runtimes; it adds **collaboration semantics** on top: Team membership, which Matrix rooms exist, how three-tier Human permissions map into generated gateway and Agent policy.

---

## 2. The control plane: declarative CRDs and reconcile

HiClaw uses a single API version: `apiVersion: agentteams.io/v1beta1`. The four core kinds are:

| Kind | In one line |
|------|-------------|
| **Worker** | Smallest execution unit: container/Pod + Matrix account + MinIO space + Gateway consumer |
| **Team** | One Team Leader plus members; drives Leader Room, Team Room, DM topology |
| **Human** | Real user + permission tier (L1/L2/L3); drives room invites and Agent `@mention` allow lists |
| **Manager** | Coordinator Agent—model, persona/skills packages, heartbeat/idle policy—same reconcile loop as other CRs |

A minimal Worker excerpt (see repo CRDs for the full schema):

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: qwen3.5-plus
  runtime: openclaw          # or copaw
  skills: [github-operations]
  mcpServers: [github]
  # state: Running | Sleeping | Stopped — desired lifecycle, enforced by controller
  # channelPolicy — allow/deny extras on top of default group/DM rules
```

`Team` requires `leader` and `workers`; optional `peerMentions`, `channelPolicy`, and `admin` (team-scoped human admin). **The Team Room does not include the Manager**—coordination stops at the Leader, matching the product rule that the Manager talks only to the Leader, not through the team.

For declarative apply, hosts often use `install/hiclaw-apply.sh` to copy YAML into the Manager container and run `hiclaw apply -f`. The CLI calls the REST API **in YAML document order** (`/api/v1/workers`, `/teams`, `/humans`, `/managers`) and **does not** topologically sort dependencies—you should define a `Team` before a `Human` that references `accessibleTeams`, for example. That is the same mental model as applying multiple Kubernetes manifests in a chosen order.

After CRDs are installed, kubectl short names are `wk`, `tm`, `hm`, `mgr`.

---

## 3. Runtime topology: Matrix rooms, not bespoke RPC

HiClaw uses **Matrix** as the collaboration transport instead of ad-hoc pairwise RPC:

- Conversations live in **rooms**, which fits multi-party presence, human oversight, and audit trails.
- Human-in-the-loop needs no separate channel: the same client can @mention any authorized role.

**Tuwunel** can run as the bundled homeserver for simpler single-box installs.

Alongside chat, **MinIO** (S3-compatible) holds per-Agent config, skills, and shared task specs/results. Containers stay effectively stateless; recreating a Worker does not erase “logical state” as long as desired state remains in object storage.

---

## 4. Security: secrets stay at the gateway; Agents hold consumer tokens

LLM and MCP secrets should not sprawl across every Agent container. **Higress** (Envoy-based, CNCF Sandbox) keeps upstream credentials on the gateway; Workers carry only revocable **consumer tokens**, with **per-route `allowedConsumers`** (and related policy) for per-Agent authorization.  
That parallels Kubernetes: Pods use ServiceAccounts; real power comes from RBAC and policy, not from embedding cluster-admin keys in the Pod.

MCP calls use the same pattern: centralized endpoints, gateway auth and credential injection, and **mcporter**-style clients so “who may call which MCP” is policy-driven and auditable—not one PAT per sandbox.

---

## 5. Coexistence with strong single-Agent sandboxes

**NVIDIA NemoClaw** emphasizes **OS-level isolation and credential routing for one Agent**. HiClaw emphasizes **multi-role collaboration and org-level policy**. They are complementary:

- NemoClaw: lock **one** Agent in a hard sandbox—great for security-sensitive single-task execution surfaces.
- HiClaw: place **many** Agents in a declared org and communication graph—great for “work like a team” control planes.

The Worker backend abstraction allows swapping or augmenting runtimes under collaboration orchestration—similar to Kubernetes plugging different runtimes behind CRI.

---

## 6. Embedded vs incluster

Typical paths:

- **Embedded**: local container stack; state may flow through kine (SQLite-backed etcd compatibility) into the controller; Workers often run as Docker containers.
- **Incluster**: CRs live in Kubernetes etcd; the controller runs as a Deployment; Workers are Pods; the repo ships a **Helm chart** (e.g. `helm/hiclaw`) for dependent components.

Both modes share the same reconcile semantics; they differ mainly in state store and process orchestration backends.

---

## 7. Closing

HiClaw lifts multi-agent collaboration from scripts and conventions to **CRD-style declarative APIs plus continuous reconcile**: organization, messaging policy, gateway-side LLM/MCP rules, and shared storage can be governed on one operational path. If you already run Kubernetes, the model will feel familiar; if you are sketching next-gen Agent platforms, treat HiClaw as the **collaboration orchestration plane** and leave inference and tooling to pluggable runtimes and gateway implementations.

For full field references, room topology, and operational commands, see `docs/declarative-resource-management.md` and `docs/k8s-native-agent-orch.md` in the repository.

---

**License:** Apache 2.0 · **Repository:** https://github.com/higress-group/hiclaw
