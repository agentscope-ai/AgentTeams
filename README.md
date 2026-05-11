<h1 align="center">
    <img src="https://img.alicdn.com/imgextra/i2/O1CN01hTYQMO28B3H9qP7RV_!!6000000007893-2-tps-1490-392.png" alt="HiClaw"  width="290" height="72.5">
  <br>
</h1>

[English](./README.md) | [中文](./README.zh-CN.md) | [日本語](./README.ja-JP.md)

<p align="center">
  <a href="https://deepwiki.com/higress-group/hiclaw"><img src="https://img.shields.io/badge/DeepWiki-Ask_AI-navy.svg?logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACwAAAAyCAYAAAAnWDnqAAAAAXNSR0IArs4c6QAAA05JREFUaEPtmUtyEzEQhtWTQyQLHNak2AB7ZnyXZMEjXMGeK/AIi+QuHrMnbChYY7MIh8g01fJoopFb0uhhEqqcbWTp06/uv1saEDv4O3n3dV60RfP947Mm9/SQc0ICFQgzfc4CYZoTPAswgSJCCUJUnAAoRHOAUOcATwbmVLWdGoH//PB8mnKqScAhsD0kYP3j/Yt5LPQe2KvcXmGvRHcDnpxfL2zOYJ1mFwrryWTz0advv1Ut4CJgf5uhDuDj5eUcAUoahrdY/56ebRWeraTjMt/00Sh3UDtjgHtQNHwcRGOC98BJEAEymycmYcWwOprTgcB6VZ5JK5TAJ+fXGLBm3FDAmn6oPPjR4rKCAoJCal2eAiQp2x0vxTPB3ALO2CRkwmDy5WohzBDwSEFKRwPbknEggCPB/imwrycgxX2NzoMCHhPkDwqYMr9tRcP5qNrMZHkVnOjRMWwLCcr8ohBVb1OMjxLwGCvjTikrsBOiA6fNyCrm8V1rP93iVPpwaE+gO0SsWmPiXB+jikdf6SizrT5qKasx5j8ABbHpFTx+vFXp9EnYQmLx02h1QTTrl6eDqxLnGjporxl3NL3agEvXdT0WmEost648sQOYAeJS9Q7bfUVoMGnjo4AZdUMQku50McDcMWcBPvr0SzbTAFDfvJqwLzgxwATnCgnp4wDl6Aa+Ax283gghmj+vj7feE2KBBRMW3FzOpLOADl0Isb5587h/U4gGvkt5v60Z1VLG8BhYjbzRwyQZemwAd6cCR5/XFWLYZRIMpX39AR0tjaGGiGzLVyhse5C9RKC6ai42ppWPKiBagOvaYk8lO7DajerabOZP46Lby5wKjw1HCRx7p9sVMOWGzb/vA1hwiWc6jm3MvQDTogQkiqIhJV0nBQBTU+3okKCFDy9WwferkHjtxib7t3xIUQtHxnIwtx4mpg26/HfwVNVDb4oI9RHmx5WGelRVlrtiw43zboCLaxv46AZeB3IlTkwouebTr1y2NjSpHz68WNFjHvupy3q8TFn3Hos2IAk4Ju5dCo8B3wP7VPr/FGaKiG+T+v+TQqIrOqMTL1VdWV1DdmcbO8KXBz6esmYWYKPwDL5b5FA1a0hwapHiom0r/cKaoqr+27/XcrS5UwSMbQAAAABJRU5ErkJggg==" alt="DeepWiki"></a>
  <a href="https://discord.com/invite/NVjNA4BAVw"><img src="https://img.shields.io/badge/Discord-Join_Us-blueviolet.svg?logo=discord" alt="Discord"></a>
</p>

**HiClaw is an open-source collaborative multi-agent runtime platform. It enables multiple Agents to collaborate in a controlled and auditable room, with full human visibility and intervention capabilities throughout the process..**

Built on a **Manager-Workers architecture**, HiClaw features a Manager that centrally orchestrates multiple Workers, focusing on collaboration scenarios between humans and Agents, as well as among Agents within enterprise environments.

HiClaw does not compete with other xxClaw projects. Instead of implementing Agent logic itself, it orchestrates and manages multiple Agent containers (including the Manager and numerous Workers).

## Key Features

- 🧬 **Manager-Workers Architecture**: Eliminates the need for human oversight of individual Worker Claws by enabling Agents to manage other Agents.

- 🤝 **Multi-Runtime Collaboration**: OpenClaw, QwenPaw, and Hermes Workers coexist in the same IM room. Use deterministic agents (OpenClaw/QwenPaw) as Leaders to orchestrate tasks, and Hermes Workers for autonomous code execution — each runtime does what it's best at.

- 📦 **MinIO Shared File System**: Introduces a shared file system for inter-Agent information exchange, significantly reducing token consumption in multi-Agent collaboration scenarios.

- 🔐 **Higress AI Gateway**: Centralizes traffic management and mitigates credential-related risks, alleviating user concerns about security vulnerabilities in the native Lobster framework.

- ☎️ **Element IM Client + Tuwunel IM Server (both Matrix protocol-based)**: Eliminating DingTalk/Lark integration overhead and enterprise approval workflows. Enables rapid user onboarding to experience the "delight" of model services within an IM environment, while maintaining compatibility with native OpenClaw IM integration.

## News

- **2026-04-24**: [English](blog/hiclaw-1.1.0-release.md) | [中文](blog/zh-cn/hiclaw-1.1.0-release.md) — HiClaw v1.1.0: Kubernetes-native control plane, Hermes autonomous coding agent runtime, 1.7 GB image shrink, hiclaw CLI replaces shell scripts.
- **2026-04-14**: [English](blog/hiclaw-k8s-native-multi-agent-collaboration.md) | [中文](blog/zh-cn/hiclaw-k8s-native-multi-agent-collaboration.zh-CN.md) — Deep dive: HiClaw as a Kubernetes-native multi-agent collaboration orchestration system.
- **2026-04-03**: [English](docs/declarative-resource-management.md) | [中文](docs/zh-cn/declarative-resource-management.md) — HiClaw 1.0.9: Kubernetes-style declarative resource management (YAML for Worker, Team, Human); Worker Template Marketplace; Manager QwenPaw runtime; Nacos Skills Registry and more.
- **2026-03-14**: [English](blog/hiclaw-1.0.6-release.md) | [中文](blog/zh-cn/hiclaw-1.0.6-release.md) — HiClaw 1.0.6: enterprise-grade MCP Server management, zero credential exposure.
- **2026-03-10**: [English](blog/hiclaw-1.0.4-release.md) | [中文](blog/zh-cn/hiclaw-1.0.4-release.md) — HiClaw 1.0.4: QwenPaw (formerly CoPaw) Worker support, 80% less memory.
- **2026-03-04**: [English](blog/hiclaw-announcement.md) | [中文](blog/zh-cn/hiclaw-announcement.md) — HiClaw open sourced.

## Why HiClaw

- **Enterprise-Grade Security**: Worker Agents operate with consumer tokens only. Real credentials (API keys, GitHub PATs) stay in the gateway — Workers can't see them, and neither can attackers.

- **Fully Private**: Matrix is a decentralized, open protocol. Host it yourself, federate with others if you want. No vendor lock-in, no data harvesting.

- **Human-in-the-Loop by Default**: Every Matrix room includes you, the Manager, and the relevant Workers. Watch everything. Jump in anytime. No black boxes.

- **Zero Configuration IM**: Built-in Matrix server means no bot applications, no API approvals, no waiting. Just open Element Web and start chatting.

- **One Command Setup**: `curl | bash` and you're done — AI gateway, Matrix server, file storage, web client, and Manager Agent.

- **Skills Ecosystem**: Workers pull from [skills.sh](https://skills.sh) (80,000+ community skills) on demand. Safe because Workers can't access real credentials.

## Quick Start

**Prerequisites**: Docker Desktop (Windows/macOS) or Docker Engine (Linux).

**Resources**: 2 CPU cores + 4 GB RAM minimum. For multiple Workers, 4 cores + 8 GB recommended.

### Install

**macOS / Linux:**
```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows (PowerShell 7+ recommended):**
```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

The installer walks you through:
1. Choose your LLM provider (OpenAI-compatible APIs supported)
2. Enter your API key
3. Select network mode (local-only or external access)
4. Wait for setup to complete

### Access

Open http://127.0.0.1:18088 in your browser and log in to Element Web. The Manager will greet you and explain how to create your first Worker.

**Mobile**: Use any Matrix client (Element, FluffyChat) and connect to your server address.

**That's it.** No bot applications. No external services. Your AI team runs entirely on your machine.

## Upgrade

```bash
# Upgrade to latest (preserves all data)
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)

# Upgrade to specific version
HICLAW_VERSION=v1.0.5 bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

## Uninstall

**macOS / Linux:**
```bash
bash <(curl -fsSL https://raw.githubusercontent.com/higress-group/hiclaw/main/install/hiclaw-install.sh) uninstall
```

**Windows (PowerShell):**
```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; $s=$wc.DownloadString('https://raw.githubusercontent.com/higress-group/hiclaw/main/install/hiclaw-install.ps1'); & ([scriptblock]::Create($s)) uninstall
```

This removes all HiClaw containers (Manager, Workers, docker-proxy), Docker volume, network, env file, workspace directory, and install log.

## Install on Kubernetes (Helm)

For shared / production deployments you can install HiClaw on any Kubernetes cluster via the official Helm chart. The default profile bundles the Higress AI gateway, Tuwunel (Matrix), MinIO and the HiClaw controller — no external dependencies required.

**Prerequisites**

- Kubernetes 1.24+ (kind / minikube / k3s / managed K8s — all work)
- Helm 3.7+
- A default StorageClass (for the Tuwunel + MinIO PVCs)

**Install (OpenAI / OpenAI-compatible)**

```bash
helm repo add higress.io https://higress.io/helm-charts
helm repo update

helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

For non-OpenAI providers that expose an OpenAI-compatible API, also set `llmBaseUrl`:

```bash
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.llmBaseUrl=https://your-provider.example.com/v1 \
  --set credentials.defaultModel=your-model-name \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

<details>
<summary>Using Qwen (通义千问) instead</summary>

```bash
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set credentials.llmApiKey=<your-qwen-api-key> \
  --set credentials.llmProvider=qwen \
  --set credentials.defaultModel=qwen3.5-plus \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

</details>

| Value | Required | Description |
|---|---|---|
| `credentials.llmApiKey` | yes | API key for your LLM provider |
| `gateway.publicURL` | yes | Public URL where users will reach Element Web (e.g. `http://localhost:18080` for port-forward, or `https://hiclaw.example.com` for an Ingress) |
| `credentials.adminPassword` | recommended | Matrix admin password; auto-generated if left empty (you'll have to read it back from the Secret) |
| `credentials.llmProvider` | no | LLM provider name, defaults to `openai-compat` |
| `credentials.defaultModel` | no | Default model, defaults to `gpt-5.4` |
| `credentials.llmBaseUrl` | no | OpenAI-compatible base URL (e.g. `https://api.deepseek.com/v1`). Leave empty for official OpenAI API |
| `manager.runtime` | no | Manager agent runtime: `openclaw` (default), `copaw`, or `hermes` |
| `worker.defaultRuntime` | no | Default Worker runtime: `openclaw` (default), `copaw`, or `hermes` |

<details>
<summary>Using alternative runtimes (QwenPaw Manager + Hermes Workers)</summary>

```bash
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace --devel \
  --set manager.runtime=copaw \
  --set worker.defaultRuntime=hermes \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.llmBaseUrl=https://your-provider.example.com/v1 \
  --set credentials.defaultModel=your-model-name \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

The image for each component is automatically selected based on the runtime (`hiclaw-manager` / `hiclaw-manager-copaw` for Manager; `hiclaw-worker` / `hiclaw-copaw-worker` / `hiclaw-hermes-worker` for Workers).

</details>

**Multi-Region Image Registry**

The default `global.imageRegistry` points to the China region (`higress-registry.cn-hangzhou.cr.aliyuncs.com/higress`). If you are deploying outside China, override it for faster image pulls:

| Region | Registry |
|---|---|
| China (default) | `higress-registry.cn-hangzhou.cr.aliyuncs.com/higress` |
| North America | `higress-registry.us-west-1.cr.aliyuncs.com/higress` |
| Southeast Asia | `higress-registry.ap-southeast-7.cr.aliyuncs.com/higress` |

```bash
# Example: deploy from the North America registry
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set global.imageRegistry=higress-registry.us-west-1.cr.aliyuncs.com/higress \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

For all configurable values (gateway/storage providers, image tags, resources, persistence, etc.) see [`helm/hiclaw/values.yaml`](helm/hiclaw/values.yaml).

**Access**

```bash
kubectl port-forward -n hiclaw-system svc/higress-gateway 18080:80
```

Then open http://localhost:18080 in your browser and log in to Element Web. For an actual cluster, configure an Ingress / LoadBalancer / DNS record pointing at `svc/higress-gateway` and set `gateway.publicURL` accordingly.

**Upgrade**

```bash
helm repo update
helm upgrade hiclaw higress.io/hiclaw -n hiclaw-system --reuse-values
```

**Uninstall**

```bash
helm uninstall hiclaw -n hiclaw-system
kubectl delete namespace hiclaw-system
```

For the Kubernetes-native architecture (CRDs, controller, declarative `Worker` / `Team` / `Human` resources), see [docs/k8s-native-agent-orch.md](docs/k8s-native-agent-orch.md).

## How It Works

### Manager as Your AI Chief of Staff

```
You: Create a Worker named alice for frontend development

Manager: Done. Worker alice is ready.
         Room: Worker: Alice
         Tell alice what to build.

You: @alice implement a login page with React

Alice: On it... [a few minutes later]
       Done. PR submitted: https://github.com/xxx/pull/1
```

<p align="center">
  <img src="https://img.alicdn.com/imgextra/i4/O1CN01wHWaJQ29KV3j5vryD_!!6000000008049-0-tps-589-1280.jpg" width="240" />
  &nbsp;&nbsp;&nbsp;&nbsp;
  <img src="https://img.alicdn.com/imgextra/i2/O1CN01q9L67J245mFT0fPXH_!!6000000007340-0-tps-589-1280.jpg" width="240" />
</p>
<p align="center">
  <sub>① Manager creates a Worker and assigns tasks</sub>
  &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;
  <sub>② You can also direct Workers directly in the room</sub>
</p>

### Security Model

```
Worker (consumer token only)
    → Higress AI Gateway (holds real API keys, GitHub PAT)
        → LLM API / GitHub API / MCP Servers
```

Workers see only their consumer token. The gateway handles all real credentials. The Manager knows what Workers are doing but never touches the actual keys.

### Human in the Loop

Every Matrix Room includes you, the Manager, and relevant Workers:

```
You: @bob wait, change the password rule to minimum 8 chars
Bob: Got it, updated.
Alice: Frontend validation updated too.
```

No hidden agent-to-agent calls. Everything is visible and intervenable.

## Multi-Runtime Collaboration

HiClaw supports three Worker runtimes that can **coexist in the same IM room**, collaborating on tasks together:

- **OpenClaw** (Node.js) — General-purpose agent with rich skills ecosystem, ideal for task orchestration and tool calling
- **QwenPaw** (Python) — Lightweight runtime, suited for browser automation and quick tasks
- **Hermes** ([hermes-agent](https://github.com/NousResearch/hermes-agent)) — Autonomous coding agent with terminal sandbox, self-improving skills, and persistent memory

Each runtime excels at different tasks. A common pattern: use deterministic agents (OpenClaw/QwenPaw) as Leaders to decompose and assign work, and Hermes Workers for autonomous code execution. All runtimes communicate via Matrix `m.mentions` in the same room — fully visible, fully intervenable.

```bash
# Switch any worker's runtime in place
hiclaw update worker --runtime hermes
```

## Architecture

```
┌───────────────────────────────────────────────┐
│            hiclaw-controller                  │
│  Higress │ Tuwunel │ MinIO │ Element Web      │
└──────────────────┬────────────────────────────┘
                   │ Matrix + HTTP Files
┌──────────────────┴──────────┐
│     hiclaw-manager-agent     │
│     Manager (OpenClaw/       │
│       QwenPaw)               │
└──────────────────┬──────────┘
                   │
┌──────────────────┼────────────────────────────┐
│                  │                            │
▼                  ▼                            ▼
Worker Alice    Worker Bob              Worker Charlie
(OpenClaw)      (QwenPaw)               (Hermes)
```

| Component | Role |
|-----------|------|
| hiclaw-controller | Kubernetes-native control plane, reconciles Worker/Team/Manager CRs |
| Higress AI Gateway | LLM proxy, MCP Server hosting, credential management |
| Tuwunel (Matrix) | Self-hosted IM server for all Agent + Human communication |
| Element Web | Browser client, zero setup |
| MinIO | Centralized file storage, Workers are stateless |

## HiClaw vs OpenClaw Native

| | OpenClaw Native | HiClaw |
|---|---|---|
| Deployment | Single process | Distributed containers |
| Agent creation | Manual config + restart | Conversational |
| Credentials | Each agent holds real keys | Workers only hold consumer tokens |
| Human visibility | Optional | Built-in (Matrix Rooms) |
| Mobile access | Depends on channel setup | Any Matrix client, zero config |
| Monitoring | None | Manager heartbeat, visible in Room |

## Documentation

| | |
|---|---|
| [docs/quickstart.md](docs/quickstart.md) | Step-by-step guide |
| [docs/architecture.md](docs/architecture.md) | System architecture deep dive |
| [docs/manager-guide.md](docs/manager-guide.md) | Manager configuration |
| [docs/worker-guide.md](docs/worker-guide.md) | Worker deployment |
| [docs/development.md](docs/development.md) | Contributing and local dev |

## Troubleshooting

```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/manager-agent.log
```

See [docs/zh-cn/faq.md](docs/zh-cn/faq.md) for common issues.

### Reporting Bugs

Export your Matrix message logs and let an AI tool analyze them against the codebase before filing an issue — this helps us fix bugs much faster.

```bash
# Export debug logs (Matrix messages + agent sessions, PII auto-redacted)
python scripts/export-debug-log.py --range 1h
```

Then open the HiClaw repo in Cursor, Claude Code, or similar AI tool and ask:

> "Read the JSONL files in debug-log/. Analyze the Matrix message logs and agent session logs together. Cross-reference with the HiClaw codebase to identify the root cause of [describe your bug]."

Include the AI's analysis in your [bug report](https://github.com/alibaba/hiclaw/issues/new?template=bug_report.yml).

You can also let the AI tool submit the issue or PR directly. Install [GitHub CLI](https://cli.github.com/), run `gh auth login` to authenticate in your browser, then add the [OpenClaw GitHub skill](https://github.com/openclaw/openclaw/blob/main/skills/github/SKILL.md) to your AI coding tool (Cursor, Claude Code, etc.). After that, just ask it to file the issue or open a PR based on its analysis.

## Build & Test

```bash
make build          # Build all images
make test           # Build + run all integration tests
make test-quick     # Smoke test only
```

## Other Commands

```bash
make replay TASK="Create a Worker named alice for frontend development"
make uninstall
make help
```

## Community

- [Discord](https://discord.gg/NVjNA4BAVw)
- [GitHub Issues](https://github.com/alibaba/hiclaw/issues)

## License

Apache License 2.0
