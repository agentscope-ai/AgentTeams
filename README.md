🕴️<h1 align="center">
    <img src="https://img.alicdn.com/imgextra/i2/O1CN01hTYQMO28B3H9qP7RV_!!6000000007893-2-tps-1490-392.png" alt="HiClaw"  width="290" height="72.5">
  <br>
</h1>

[English](./README.md) | [中文](./README.zh-CN.md) | [日本語](./README.ja-JP.md)

<p align="center">
  <a href="https://deepwiki.com/higress-group/hiclaw"><img src="https://img.shields.io/badge/DeepWiki-Ask_AI-navy.svg?logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACwAAAAyCAYAAAAnWDnqAAAAAXNSR0IArs4c6QAAA05JREFUaEPtmUtyEzEQhtWTQyQLHNak2AB7ZnyXZMEjXMGeK/AIi+QuHrMnbChYY7MIh8g01fJoopFb0uhhEqqcbWTp06/uv1saEDv4O3n3dV60RfP947Mm9/SQc0ICFQgzfc4CYZoTPAswgSJCCUJUnAAoRHOAUOcATwbmVLWdGoH//PB8mnKqScAhsD0kYP3j/Yt5LPQe2KvcXmGvRHcDnpxfL2zOYJ1mFwrryWTz0advv1Ut4CJgf5uhDuDj5eUcAUoahrdY/56ebRWeraTjMt/00Sh3UDtjgHtQNHwcRGOC98BJEAEymycmYcWwOprTgcB6VZ5JK5TAJ+fXGLBm3FDAmn6oPPjR4rKCAoJCal2eAiQp2x0vxTPB3ALO2CRkwmDy5WohzBDwSEFKRwPbknEggCPB/imwrycgxX2NzoMCHhPkDwqYMr9tRcP5qNrMZHkVnOjRMWwLCcr8ohBVb1OMjxLwGCvjTikrsBOiA6fNyCrm8V1rP93iVPpwaE+gO0SsWmPiXB+jikdf6SizrT5qKasx5j8ABbHpFTx+vFXp9EnYQmLx02h1QTTrl6eDqxLnGjporxl3NL3agEvXdT0WmEost648sQOYAeJS9Q7bfUVoMGnjo4AZdUMQku50McDcMWcBPvr0SzbTAFDfvJqwLzgxwATnCgnp4wDl6Aa+Ax283gghmj+vj7feE2KBBRMW3FzOpLOADl0Isb5587h/U4gGvkt5v60Z1VLG8BhYjbzRwyQZemwAd6cCR5/XFWLYZRIMpX39AR0tjaGGiGzLVyhse5C9RKC6ai42ppWPKiBagOvaYk8lO7DajerabOZP46Lby5wKjw1HCRx7p9sVMOWGzb/vA1hwiWc6jm3MvQDTogQkiqIhJV0nBQBTU+3okKCFDy9WwferkHjtxib7t3xIUQtHxnIwtx4mpg26/HfwVNVDb4oI9RHmx5WGelRVlrtiw43zboCLaxv46AZeB3IlTkwouebTr1y2NjSpHz68WNFjHvupy3q8TFn3Hos2IAk4Ju5dCo8B3wP7VPr/FGaKiG+T+v+TQqIrOqMTL1VdWV1DdmcbO8KXBz6esmYWYKPwDL5b5FA1a0hwapHiom0r/cKaoqr+27/XcrS5UwSMbQAAAABJRU5ErkJggg==" alt="DeepWiki"></a>
  <a href="https://discord.com/invite/NVjNA4BAVw"><img src="https://img.shields.io/badge/Discord-Join_Us-blueviolet.svg?logo=discord" alt="Discord"></a>
</p>

**HiClaw is an open-source Collaborative Multi-Agent OS for transparent, human-in-the-loop task coordination via Matrix rooms.**

Built with a **Manager-Workers architecture**, HiClaw lets you coordinate multiple Worker Agents through a Manager Agent to complete complex tasks — all conversations are visible in Matrix rooms, and you can intervene at any time.

Think of it as your AI team in a chat room: tell the Manager what you need, it spins up Workers, and you watch everything happen in real-time.

## Key Features

- 🧬 **Manager-Workers Architecture**: Eliminates the need for human oversight of individual Worker Claws by enabling Agents to manage other Agents.

- 🦞 **Customizable Agents**: Each Agent supports flexible configurations including OpenClaw, Copaw, NanoClaw, ZeroClaw, and enterprise-built Agents—scaling from individual "shrimp farming" to full-scale "shrimp farm" operations..

- 📦 **MinIO Shared File System**: Introduces a shared file system for inter-Agent information exchange, significantly reducing token consumption in multi-Agent collaboration scenarios.

- 🔐 **Higress AI Gateway**: Centralizes traffic management and mitigates credential-related risks, alleviating user concerns about security vulnerabilities in the native Lobster framework.

- ☎️ **Element IM Client + Tuwunel IM Server (both Matrix protocol-based)**: Eliminating DingTalk/Lark integration overhead and enterprise approval workflows. Enables rapid user onboarding to experience the "delight" of model services within an IM environment, while maintaining compatibility with native OpenClaw IM integration.

## News

- **2026-03-14**: HiClaw 1.0.6 — enterprise-grade MCP Server management, zero credential exposure. [Blog](blog/hiclaw-1.0.6-release.md)
- **2026-03-10**: HiClaw 1.0.4 — CoPaw Worker support, 80% less memory. [Blog](blog/hiclaw-1.0.4-release.md)
- **2026-03-04**: HiClaw open sourced. [Announcement](blog/hiclaw-announcement.md)

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

**Windows (PowerShell 5+):**
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

## Architecture

```mermaid
graph TB
    subgraph Manager Container["hiclaw-manager"]
        HG[Higress Gateway + Console]
        TW[Tuwunel Matrix Homeserver]
        MO[MinIO]
        EW[Element Web]
        MA[Manager Agent<br/>OpenClaw]
    end

    subgraph Worker Container A["hiclaw-worker"]
        WA[Worker Agent<br/>OpenClaw]
    end

    subgraph Worker Container B["hiclaw-copaw-worker"]
        WB[Worker Agent<br/>CoPaw]
    end

    Human[Human Admin] --> EW
    Human --> TW
    MA --> TW
    MA --> HG
    MA <-->|sync| MO
    WA --> HG
    WA <-->|sync| MO
    WB --> HG
    WB <-->|sync| MO
```

### Component Dependency Graph (from Makefile + Dockerfiles)

```mermaid
graph LR
    A[higress/all-in-one] --> B[openclaw-base image]
    B --> C[hiclaw-manager image]
    B --> D[hiclaw-worker image]

    E[higress/tuwunel] --> C
    F[higress/minio] --> C
    G[higress/mc] --> C
    H[higress/element-web] --> C
    I[hiclaw-controller image] --> C

    G --> D

    J[higress/python:3.11-slim] --> K[hiclaw-copaw-worker image]
    G --> K
```

Derived from:
- `/home/runner/work/hiclaw/hiclaw/Makefile` (`build*` targets and image args)
- `/home/runner/work/hiclaw/hiclaw/openclaw-base/Dockerfile`
- `/home/runner/work/hiclaw/hiclaw/manager/Dockerfile`
- `/home/runner/work/hiclaw/hiclaw/worker/Dockerfile`
- `/home/runner/work/hiclaw/hiclaw/copaw/Dockerfile`

| Component | Role |
|-----------|------|
| Higress AI Gateway | LLM proxy, MCP Server hosting, credential management |
| Tuwunel (Matrix) | Self-hosted IM server for all Agent + Human communication |
| Element Web | Browser client, zero setup |
| MinIO | Centralized file storage, Workers are stateless |
| OpenClaw | Agent runtime with Matrix plugin and skills |

## HiClaw vs OpenClaw Native

| | OpenClaw Native | HiClaw |
|---|---|---|
| Deployment | Single process | Distributed containers |
| Agent creation | Manual config + restart | Conversational |
| Credentials | Each agent holds real keys | Workers only hold consumer tokens |
| Human visibility | Optional | Built-in (Matrix Rooms) |
| Mobile access | Depends on channel setup | Any Matrix client, zero config |
| Monitoring | None | Manager heartbeat, visible in Room |

## Roadmap

### ✅ Released

- ~~**CoPaw** — Lightweight agent runtime~~ [Released in 1.0.4](blog/hiclaw-1.0.4-release.md): ~150MB memory usage (vs ~500MB for OpenClaw), plus local host mode for browser automation.
- ~~**Universal MCP Service Support** — MCP server integration~~ [Released in 1.0.6](blog/hiclaw-1.0.6-release.md): Any MCP server can be safely exposed to Workers through the gateway. Workers access tools using only Higress-issued tokens; real credentials never leave the gateway.

### In Progress

#### Lightweight Worker Runtimes

- **ZeroClaw** — Rust-based ultra-lightweight runtime, 3.4MB binary, <10ms cold start.
- **NanoClaw** — Minimal OpenClaw alternative, <4000 LOC, container-based isolation.

Goal: Reduce per-Worker memory from ~500MB to <100MB.

### Planned

#### Team Management Center

A built-in dashboard for observing and controlling your Agent Teams — real-time observation, active interruption, task timeline, resource monitoring.

---

## Documentation

| | |
|---|---|
| [docs/quickstart.md](docs/quickstart.md) | Step-by-step guide |
| [docs/architecture.md](docs/architecture.md) | System architecture deep dive |
| [docs/manager-guide.md](docs/manager-guide.md) | Manager configuration |
| [docs/worker-guide.md](docs/worker-guide.md) | Worker deployment |
| [docs/development.md](docs/development.md) | Contributing and local dev |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Contributor workflow and PR checklist |

## Changelog

| | |
|---|---|
| [changelog/current.md](changelog/current.md) | Unreleased changes |
| [changelog/](changelog/) | Versioned release changelogs |

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
