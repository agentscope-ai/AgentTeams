# HiClaw v1.1.0: From Personal Tool to Enterprise-Grade Multi-Agent Platform

> Release Date: April 24, 2026

---

v1.1.0 is the biggest release since HiClaw was open-sourced. We rewrote the entire control plane, introduced a third agent runtime, upgraded all underlying engines, and ensured a smooth upgrade path from v1.0.9. This post covers the thinking behind these changes and what they mean for you.

---

## Agent Upgrades: Three Runtimes, Fully Verified

HiClaw has supported multiple agent runtimes from day one — different tasks suit different agents, and that's the basic premise of multi-agent collaboration. In v1.1.0, all three runtimes received significant upgrades.

### New Hermes Runtime: A Self-Improving Autonomous Coding Agent

We introduced **Hermes** ([hermes-agent](https://github.com/NousResearch/hermes-agent), developed by Nous Research) as a third Worker runtime.

Hermes is not another chatbot. It's an **autonomous coding agent** that can independently plan, execute, and iterate on complex software tasks inside an isolated container. More importantly, Hermes has a **self-improvement loop**: after completing tasks, it automatically creates reusable Skills; Skills improve themselves during use; cross-session FTS5 memory retrieval lets the agent know your project better the longer you use it. In Nous Research's words, it's "the agent that grows with you."

### Leader + Worker: Deterministic Agents Direct Autonomous Agents

With Hermes on board, an interesting architectural pattern emerged: **use more deterministic agents as Leaders to direct Hermes for heavy lifting**.

We didn't invent this — the community has been discussing and practicing it. A widely cited [Dev.to article](https://dev.to/ggondim/how-i-built-a-deterministic-multi-agent-dev-pipeline-inside-openclaw-and-contributed-a-missing-4ool) documented two months of exploring autonomous coding agent orchestration, concluding with: **"Deterministic orchestration, where LLMs do creative work and YAML workflows handle the plumbing."** AWS discussed the [Agents as Tools pattern](https://dev.to/aws/build-multi-agent-systems-using-the-agents-as-tools-pattern-jce) — bringing hierarchical delegation into multi-agent orchestration. In academia, [EvoAgent](https://arxiv.org/html/2604.20133) proposed a three-layer delegation routing mechanism enabling agents to autonomously acquire and continuously optimize skills.

This pattern lands naturally in HiClaw:

- **Manager (agent/QwenPaw runtime)** acts as the Leader — responsible for task decomposition, worker scheduling, progress monitoring — tasks requiring **determinism** and **predictability**
- **Hermes Worker** acts as the executor — handling actual code writing, debugging, project-level tasks — tasks requiring **autonomy** and **creativity**
- The Manager doesn't write code for Hermes, and Hermes doesn't need to understand the team's scheduling logic — each does what it's best at

Workers can switch runtimes at any time:

```bash
hiclaw update worker --runtime hermes  # Container recreated; Matrix account, rooms, and data preserved
```

Multi-agent collaboration is fully supported — Hermes Workers participate in team projects alongside agent and QwenPaw Workers, with cross-runtime `m.mentions` message delivery and autonomous YOLO mode for unattended execution.

### openclaw and QwenPaw Upgrades

The underlying engines also received major upgrades:

- **openclaw** upgraded to `2026.4.14`, bringing Matrix private-network SSRF fixes, structured debug logging (`HICLAW_MATRIX_DEBUG=1`), and gateway Control UI port unification
- **QwenPaw** upgraded to `1.0.2`
- **openclaw-base image** rebased from `higress/all-in-one` (~1.79 GB) to `higress/ubuntu:24.04` (~103 MB), **shrinking all downstream images by ~1.7 GB**

These may sound like "infrastructure changes," but they directly impact agent stability — Matrix connection races, room join failures, Control UI inaccessibility, and other intermittent issues from v1.0.x are all resolved in this upgrade.

### Full E2E Integration Test Coverage

Importantly: **all of the above upgrades passed HiClaw's multi-agent end-to-end integration tests.** We didn't just bump dependency versions and hope for the best — Worker creation, team collaboration, message delivery, YOLO mode, and cross-runtime communication all have automated test coverage for each runtime. The upgrade is safe.

---

## From Personal to Enterprise: Kubernetes-Native Architecture

The most fundamental architectural change in v1.1.0 is the introduction of **hiclaw-controller**, a Kubernetes-native control plane.

### Why Rewrite the Control Plane?

v1.0.x used an "all-in-one" container — Manager, Higress gateway, Matrix server, MinIO, Element Web all bundled in one image. Fine for personal use, but:

- **Poor restart isolation**: Any component issue restarts the entire container, interrupting all agents
- **No horizontal scaling**: Manager can only run one instance
- **Resource waste**: An agent-only container carries 1.7GB of infrastructure baggage
- **No multi-tenancy**: No tenant isolation or resource quotas

### Two Deployment Modes, One Codebase

v1.1.0 supports two deployment modes sharing the same Controller code:

**Embedded Mode (Individual Developers)**

```bash
# One command install, no Kubernetes cluster needed
bash -c "$(curl -fsSL https://get.hiclaw.ai)"
```

Under the hood, a lightweight embedded kube-apiserver + kine presents as a `hiclaw-controller` container + a `hiclaw-manager` container. No external Kubernetes cluster required — the deployment experience is as simple as v1.0.x.

**Helm Chart Mode (Enterprise Production)**

```bash
helm install hiclaw ./helm/hiclaw -n hiclaw
```

The same Controller runs in a real Kubernetes cluster, providing:
- **Leader Election HA**: Multi-replica deployment with lease-based automatic failover
- **Agent Pod Template**: Inject nodeSelectors, tolerations, imagePullSecrets via ConfigMap overlay — no Controller code changes needed
- **Multi-Tenant Isolation**: Pluggable credential provider sidecar (`hiclaw-credential-provider`) with per-worker `accessEntries` scoping object storage paths
- **CRD-Based Management**: `kubectl get workers` works natively; `hiclaw` CLI and `kubectl` are fully interchangeable

### Declarative Reconciliation: The Foundation of Stability

Regardless of mode, the core is **declarative configuration reconciliation** (Controller-Reconciler pattern):

```
Worker CR (desired state)  →  Controller observes diff  →  Reconciles to match
Team CR                    →  Matrix rooms, gateway routes  →  Reconciles to match
Manager CR                 →  Containers, config files  →  Reconciles to match
```

This means:
- **Any component failure auto-recovers** — Controller reconciles every 5 minutes, correcting configuration drift
- **Tokens no longer rotate** — Previously, every reconcile regenerated Matrix access tokens and gateway secrets, causing frequent agent restarts and message loss. In v1.1.0, tokens are persisted and reused
- **Config files no longer overwritten** — `AGENTS.md`, `SOUL.md`, `HEARTBEAT.md` are managed by their respective authoritative writers, not overwritten by the reconciliation mirror

These may seem like details, but stability is built from details. Both personal and enterprise users benefit from the same reconciliation mechanism.

### Auto-Migration from v1.0.9

Upgrading is straightforward: `hiclaw-controller` detects v1.0.9's `workers-registry.json` on first boot and automatically migrates Worker data to CRD resources. Runtime, model, skills, MCP servers, team membership — all preserved, zero configuration.

---

## hiclaw CLI: Replacing Shell Scripts with Go

If you look at HiClaw's language breakdown, you'll notice an interesting trend:

> **Go (38%) > Shell (35%) > Python (13%)**

In the v1.0.x era, Shell was the largest language. Dozens of `setup-*.sh`, `create-*.sh`, `entrypoint-*.sh` scripts formed HiClaw's operation layer. This worked early on, but the problems grew:

### Script Pain Points

**1. Agents "read" scripts, wasting tokens**

This is a cost source many overlook. When an agent needs to create a Worker, it might `cat create-worker.sh` to check parameters, try running it, find wrong parameters, read it again... A simple "create Worker" operation could consume **20+ LLM calls**, half spent exploring the script interface.

**2. Inadequate testing**

Shell scripts are hard to unit test. Change one line, and nobody knows what other paths are affected. Some v1.0.x bugs — "defaults not taking effect", "parameters ignored" — originated here.

**3. Unstable output formats**

The same script might produce completely different output formats in different situations. Agent fails to parse, retries, wastes more tokens.

### hiclaw CLI Improvements

The `hiclaw` CLI, rewritten in Go, solves these problems:

```bash
# Create a worker
hiclaw create worker --name alice --model qwen-max

# List workers
hiclaw get workers

# Worker lifecycle management
hiclaw worker sleep alice   # Graceful stop
hiclaw worker wake alice    # On-demand wake

# Declarative configuration
hiclaw apply worker -f alice.yaml
```

- **Structured output**: JSON / YAML / table formats — zero parse failures for agents
- **Clear parameters**: `--help` is self-documenting — no need for agents to read source code
- **Comprehensive testing**: Go unit tests + integration tests covering every command
- **Built into Controller**: `hiclaw` CLI is pre-installed in the Controller container; admins can `docker exec` directly

In practice, creating a Worker with `hiclaw` CLI dropped from **22 LLM rounds to under 10** — token costs halved.

---

## More Improvements

### 1.7 GB Image Shrink

The Manager image no longer bundles Higress, Matrix, MinIO, or Element Web — only the pure agent runtime. Infrastructure services run in the separate `hiclaw-embedded` image. From 1.79 GB to 103 MB.

### First-Boot Experience

Fresh installs automatically deliver a welcome/onboarding message to the admin DM. The installer waits until delivery completes. Your first interaction is smooth.

### Pluggable Gateway & Storage

Controller delegates gateway and storage operations through provider interfaces. Alibaba Cloud OSS, AWS S3, MinIO — switch backends anytime without changing Controller code.

---

## Upgrade Guide

Upgrading from v1.0.9 is as simple as re-running the installer:

```bash
bash -c "$(curl -fsSL https://get.hiclaw.ai)"
```

The installer automatically detects v1.0.9 state, migrates data to CRDs, and pulls new images. All Worker configurations are preserved.

---

*HiClaw is an open-source multi-agent collaboration platform under the [AgentScope](https://github.com/agentscope-ai) project, built on Higress, Matrix, and OpenClaw. Star us on [GitHub](https://github.com/agentscope-ai/HiClaw) and join the [Discord](https://discord.gg/n6mV8xEYUF) community.*
