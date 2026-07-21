# HiClaw 1.0.4: Lightweight CoPaw Workers - Significantly Less Memory, Direct Local Environment Access

> Release Date: March 10, 2026

---

## What Do We Mean by "Lightweight Workers"?

If you've used HiClaw, you're probably familiar with the Manager + Worker multi-agent collaboration pattern. A Manager acts as your "AI butler," managing multiple specialized Workers — frontend development, backend development, data analysis...

But in practice, we've received quite a bit of feedback:

**"Each Worker needs to run a full container, the memory pressure is significant"** — The default OpenClaw Worker container takes up about 500MB of memory. If you need to run 4-5 Workers simultaneously, an 8GB server starts to feel tight.

**"Workers run in containers and can't access my local environment"** — Some tasks require operating browsers, accessing local file systems, running desktop applications... These are impossible in an isolated container environment.

In version 1.0.4, we have an answer: **CoPaw Worker**.

---

## What is CoPaw?

[CoPaw](https://github.com/agentscope-ai/CoPaw) is a lightweight Python-based AI Agent runtime with these key features:

- **Lightweight**: Python-based, doesn't need the full Node.js stack, uses only 1/5 the memory of OpenClaw Worker
- **Console-friendly**: Built-in web console for real-time viewing of tool calls, thinking output, and execution process
- **Fast execution**: Native Python startup with quick cold starts
- **Easy to extend**: Tool definitions based on OpenAI SDK, low learning curve

HiClaw 1.0.4 integrates CoPaw into the multi-agent collaboration system by implementing a Matrix Channel and configuration bridge layer. The code footprint is small, but it unlocks many new possibilities.

---

## Manager-Worker Architecture: Dramatically Reducing Agent Integration Complexity

The successful integration of CoPaw Worker fully demonstrates the advantages of HiClaw's Manager-Worker architecture in **reducing the cost of integrating new Agent runtimes**.

### Pain Points of the Traditional Approach

If you want a new Agent runtime (like CoPaw) to reach users, the traditional approach requires:

1. **Support for the full Channel ecosystem**: OpenClaw supports over a dozen messaging channels — Discord, Telegram, Slack, Feishu, DingTalk, WeChat, iMessage... Each channel has different APIs, authentication methods, and message formats
2. **Implement various Channel adapters**: Need to develop, test, and maintain each one individually
3. **Users need to configure each one**: Webhooks, tokens, certificates for every channel...
4. **Fragmented client ecosystem**: Different channels have different clients, inconsistent user experience

This is a massive engineering effort. Many excellent Agent runtimes can't reach users simply because this barrier is too high.

### HiClaw's Solution: Matrix as the Unified Communication Layer

HiClaw's Manager-Worker architecture unifies the communication layer on the Matrix protocol:

```
┌─────────────────────────────────────────────────────────────────┐
│                        HiClaw Manager                            │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐  │
│   │              Tuwunel Matrix Server                       │  │
│   │              (Built-in, ready to use)                    │  │
│   └─────────────────────────────────────────────────────────┘  │
│                              │                                  │
│              ┌───────────────┼───────────────┐                 │
│              ↓               ↓               ↓                 │
│         Discord          Telegram         Slack               │
│         (via bridge)      (via bridge)    (via bridge)        │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                               ↑ Matrix Protocol
                               │
┌──────────────────────────────┴─────────────────────────────────┐
│                        Worker                                   │
│                                                                 │
│   Only need to implement Matrix Channel — one protocol,        │
│   all channels covered                                         │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**For a new Agent runtime, integrating with HiClaw requires just one thing: implement Matrix Channel.**

### Actual Work to Integrate CoPaw into HiClaw

The core code for integrating CoPaw in HiClaw 1.0.4 is just two files:

1. **`matrix_channel.py`** (~450 lines): Implements Matrix protocol communication
2. **`bridge.py`** (~230 lines): Bridges openclaw.json to CoPaw configuration

That's it! CoPaw doesn't need to worry about Discord, Telegram, Slack... It just communicates with Matrix and gets:

- ✅ Reuse all Channel ecosystems supported by Manager
- ✅ Reuse ready-to-use Matrix clients (Element Web built-in, Element/FluffyChat for mobile)
- ✅ Seamless collaboration with other Workers (regardless of runtime)
- ✅ Unified management, monitoring, and scheduling by Manager

**For users, integrating a new Agent runtime has zero learning cost** — the interaction is exactly the same, still conversing through Matrix clients, with Manager automatically handling underlying differences.

### What Does This Mean?

If you're developing a new Agent runtime, or want to connect an existing Agent to the HiClaw ecosystem:

- **Don't need to**: Individually adapt Discord, Telegram, Slack...
- **Only need to**: Implement Matrix protocol (a mature open standard)
- **And you get**: Dozens of messaging channels + ready-to-use clients + multi-agent collaboration

This is the core value of the Manager-Worker architecture: **integrate once, use everywhere**.

---

## Two Deployment Modes, Solving Two Pain Points

### Mode 1: Docker Container Mode — More Memory-Efficient Workers

If you just need more Workers working in parallel without local environment access, **Docker-mode CoPaw Worker is the best choice**:

| Comparison | OpenClaw Worker | CoPaw Worker (Docker) |
|------------|-----------------|----------------------|
| Base Image | Node.js full stack | Python 3.11-slim |
| Memory Usage | ~500MB | ~100MB |
| Startup Speed | Slower | Faster |
| Security | Container isolation | Container isolation |

**80% reduction in memory usage**, with identical security.

This means you can run more Workers on the same hardware. Previously, 8GB of memory could only run 8-10 OpenClaw Workers; now you can run 40+ CoPaw Workers.

**On-demand Console**

CoPaw Workers start without the web console by default to save resources. When you need to debug, just tell the Manager in Element:

```
You: Enable console for alice

Manager: Sure, enabling alice's console...
         Container restarted, console URL: http://localhost:18089
```

The Manager will automatically restart the CoPaw Worker container with the console enabled. No manual scripting needed. When you're done debugging, you can also ask the Manager to disable the console.

### Mode 2: Local Host Mode — Direct Access to Your Computer

Some tasks naturally require local environment access:

- **Browser operations**: Automated testing, web screenshots, data collection
- **Local file access**: Reading files on your desktop, operating local IDEs
- **Running desktop apps**: Automating Figma, Sketch, local database clients

These tasks can't be done in containers because containers are isolated environments.

**CoPaw Worker's local mode is designed for these tasks**:

```bash
# Manager will give you this command to run on your local machine
pip install copaw-worker && copaw-worker --config ... --console-port 8088
```

The Worker runs directly on your local machine with full local access permissions. At the same time, it still communicates with the Manager and other Workers via Matrix, seamlessly integrating into HiClaw's multi-agent collaboration system.

**Architecture Diagram:**

```
┌─────────────────────────────────────────────────────────────┐
│                    HiClaw Manager                            │
│                    (Container Environment)                   │
│                                                             │
│    Worker A (Docker)    Worker B (Docker)                   │
│    Frontend Dev          Backend Dev                        │
└─────────────────────────────────────────────────────────────┘
              ↑ Matrix Communication
              │
┌─────────────┴───────────────────────────────────────────────┐
│                    Your Local Computer                       │
│                                                             │
│    Worker C (CoPaw Local Mode)                              │
│    Browser Ops / Local File Access                          │
└─────────────────────────────────────────────────────────────┘
```

Local mode enables the console by default (`--console-port 8088`), so you can open `http://localhost:8088` to view the Worker's execution process in real-time.

---

## CoPaw Console: Visual Debugging Experience

Whether in Docker mode or local mode, CoPaw Workers can enable a web console.

The console shows real-time:

- **Thinking output**: What the Worker is thinking
- **Tool calls**: Which tools were called and with what parameters
- **Execution results**: What the tools returned
- **Error messages**: Where things went wrong

This is incredibly helpful for debugging and optimizing Agent behavior. Especially when you notice a Worker not behaving as expected, opening the console to check the thinking output often helps quickly identify the problem.

---

## Community-Driven Optimizations

Beyond the major CoPaw Worker feature, 1.0.4 also addresses a series of pain points reported by the community.

### More Controlled Model Switching

Previously, users reported that when switching models, the Manager might "take it upon itself" to modify other configurations, causing unexpected behavior.

1.0.4 extracts Worker model switching into a standalone `worker-model-switch` skill with more focused responsibilities and more predictable behavior. It also fixes the hardcoded model `input` field issue — now it's dynamically set based on whether the model supports vision capabilities.

### Workers No Longer "Chat Among Themselves"

In project group chats, Workers would sometimes have unnecessary conversations, wasting tokens.

1.0.4 optimizes Worker wake-up logic to ensure LLM calls are only triggered when @mentioned. It also fixes an issue where CoPaw MatrixChannel replies weren't carrying sender information, preventing the Manager from ignoring Worker replies and causing duplicate calls.

### AI Identity Awareness

An AI identity section has been added to SOUL.md to ensure Agents clearly know they are AI, not human. This avoids strange identity confusion issues, like Agents pretending to be real users.

```markdown
## My Role

You are an AI assistant powered by HiClaw. You help users complete tasks
through natural language interaction, but you are not a human.
```

### Token Consumption Baseline CI

1.0.4 adds a Token consumption baseline CI workflow to quantitatively analyze each version's token optimization effects.

In key workflows (creating Workers, assigning tasks, multi-Worker collaboration, etc.), CI records token consumption and compares it with the previous version. This enables:

- Quantifying optimization effects
- Detecting unexpected token regressions
- Providing data support for future optimizations

---

## Getting Started

### Installation & Upgrade

Installation and upgrade use the same command — the script will guide you interactively:

**macOS / Linux:**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows (PowerShell 7+):**

```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

During installation, you'll be asked which Worker runtime to use as default:

```
Select default worker runtime:
  1) openclaw (~500MB)
  2) copaw (~150MB, lightweight)

Enter your choice [1-2]:
```

When upgrading, the script will automatically detect your existing installation — just select "in-place upgrade". You'll also be prompted to choose a default Worker runtime:
- **Existing Workers**: Unaffected, continue using their original runtime
- **New Workers**: Will use the default runtime you selected (CoPaw or OpenClaw)

---

## Acknowledgments

Thanks to the [CoPaw team](https://github.com/agentscope-ai/CoPaw) for their work! CoPaw is a well-designed lightweight Agent runtime with an especially excellent console experience. HiClaw's integration with CoPaw through Matrix Channel and configuration bridge layer was smooth, with minimal code required.

If you're interested in CoPaw itself, check out the [CoPaw GitHub repository](https://github.com/agentscope-ai/CoPaw).

---

## Closing Thoughts

The core goal of HiClaw 1.0.4 is to make Workers lighter and more flexible:

- **Lighter**: CoPaw Workers use 80% less memory
- **More flexible**: Local mode unlocks new scenarios like browser automation
- **Easier integration**: Manager-Worker architecture lets new Agent runtimes just implement Matrix protocol

We especially recommend trying CoPaw Workers if you:

- Need to run many Workers simultaneously but have limited memory
- Need Workers to operate browsers or access local files
- Want a lighter-weight Worker debugging experience

**Get Started Now:**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

---

*HiClaw is an open-source project under the Apache 2.0 license. If you find it useful, please consider giving it a Star ⭐ and contributing code!*

**Related Links:**
- [GitHub Repository](https://github.com/alibaba/hiclaw)
- [Changelog v1.0.4](https://github.com/alibaba/hiclaw/blob/main/changelog/v1.0.4.md)
- [CoPaw GitHub](https://github.com/agentscope-ai/CoPaw)
