# HiClaw: OpenClaw Evolved — Safer, Easier, Your AI Team in 5 Minutes

> Published: February 27, 2026

---

## Sound familiar?

As a heavy OpenClaw user, I know firsthand how powerful it is — one Agent can write code, check emails, operate GitHub. But when you start tackling more complex projects, the cracks show:

**Security keeps you up at night**: Every Agent needs its own API Key. GitHub PATs and LLM keys are scattered everywhere. The CVE-2026-25253 vulnerability in January 2026 made it clear — this "self-hackable" architecture trades convenience for real risk.

**One Agent wearing too many hats**: Frontend, backend, docs — all in one. The `skills/` directory becomes a mess, `MEMORY.md` fills up with unrelated context, and every session loads a pile of irrelevant noise.

**Multi-Agent coordination without good tooling**: Manual config, manual task assignment, manual progress sync. You want to focus on decisions, not babysit AI.

**Mobile experience is painful**: Want to direct Agents from your phone? Integrating with Slack or Teams bots can take days or weeks.

If any of this resonates, **HiClaw** was built for you.

---

## What is HiClaw?

**HiClaw = OpenClaw, evolved.**

The core innovation is the **Manager Agent** — your AI chief of staff. It doesn't do the work directly; it manages a team of Worker Agents on your behalf.

```
┌─────────────────────────────────────────────────────┐
│                  Your Local Environment              │
│  ┌───────────────────────────────────────────────┐  │
│  │         Manager Agent (AI Chief of Staff)     │  │
│  │                    ↓ manages                  │  │
│  │    Worker Alice    Worker Bob    Worker ...   │  │
│  │    (frontend)      (backend)                  │  │
│  └───────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────┘
         ↑
    You (human admin)
    Make decisions, skip the babysitting
```

---

## Architecture: OpenClaw Gets an Upgrade

OpenClaw's design is like a complete organism: a **brain** (LLM), a **central nervous system** (pi-mono), and **eyes and a mouth** (various Channels). But in the original design, the brain and sensory organs are "external" — you configure the LLM provider and message channels yourself.

HiClaw performs an "organ transplant," turning those external components into **built-in organs**:

```
┌────────────────────────────────────────────────────────────────────┐
│                         HiClaw All-in-One                          │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │                     OpenClaw (pi-mono)                       │  │
│  │                    Central Nervous System                    │  │
│  └──────────────────────────────────────────────────────────────┘  │
│           ↑                              ↑                         │
│  ┌────────────────┐              ┌────────────────┐                │
│  │  Higress AI    │              │   Tuwunel      │                │
│  │  Gateway       │              │   Matrix       │                │
│  │  (brain access)│              │   Server       │                │
│  │                │              │  (sensory org) │                │
│  │  Switch LLM    │              │                │                │
│  │  providers &   │              │  Element Web   │                │
│  │  models freely │              │  (built-in UI) │                │
│  └────────────────┘              └────────────────┘                │
└────────────────────────────────────────────────────────────────────┘
```

### LLM Access: Higress AI Gateway

**The brain is no longer external — it's managed through an AI Gateway**:

- **One endpoint, many models**: Switch between Alibaba Cloud Qwen, OpenAI, Claude, and more from the Higress console
- **Centralized credentials**: Configure API Keys once, shared across all Agents
- **Least-privilege access**: Each Worker gets call permissions only — never touches the real API Key

### Communication: Built-in Matrix Server

**The sensory organs are built-in too**:

- **Tuwunel Matrix Server**: Ready-to-use messaging server, zero configuration
- **Element Web included**: Open a browser and start chatting
- **Mobile-friendly**: Supports Element, FluffyChat, and other cross-platform clients
- **Zero integration cost**: No Slack/Teams bot approval process needed

> 💡 Think of it this way: vanilla OpenClaw is like a desktop PC — you buy your own GPU (LLM) and monitor (Channel) and install drivers yourself. HiClaw is a laptop — everything's integrated, open the lid and get to work.

---

## Multi-Agent System: Your AI Jarvis

On top of the component integration, HiClaw ships an **out-of-the-box Multi-Agent system** — Manager Agent coordinates Worker Agents, just like Tony Stark's Jarvis.

### On-demand, two modes

The system is **opt-in** — use it how you want:

**Mode 1: Talk directly to Manager**
- Simple tasks go straight to Manager, it handles them
- Great for quick questions and simple operations

**Mode 2: Manager delegates to Workers**
- Complex tasks get broken down and assigned to specialized Workers
- Each Worker has isolated Skills and Memory
- No cross-contamination between Workers

### Collaboration: Supervisor + Swarm

From the Manager-Worker perspective, this is a **Supervisor architecture** — Manager as the central coordinator. But because it's built on Matrix group chat rooms, it also has **Swarm** characteristics.

**Shared context, no repeated briefings**: Every Agent sees the full group chat history. Alice says "I'm working on the login page," Bob automatically knows what the frontend is doing and can align the API design.

**Anti-stampede design**: Agents only trigger LLM calls when @mentioned — irrelevant messages don't wake them up, keeping costs predictable.

**Artifacts don't pollute context**: File exchanges and code snippets flow through the underlying **MinIO shared filesystem**, not the group chat, so context stays clean.

### Security: Manager Manages, But Can't Leak

In vanilla OpenClaw, every Agent holds real API Keys. If one gets compromised or accidentally outputs credentials, you're exposed.

HiClaw's solution: **Workers never hold real credentials**:

```
┌──────────────┐      ┌──────────────────┐      ┌─────────────┐
│   Worker     │─────►│  Higress AI      │─────►│  LLM API    │
│  (holds only │      │  Gateway         │      │  GitHub API │
│  Consumer    │      │  (credentials    │      │  ...        │
│  Token)      │      │   centralized)   │      │             │
└──────────────┘      └──────────────────┘      └─────────────┘
```

- Workers hold only a Consumer Token (like a badge — grants access, not keys)
- Real API Keys and GitHub PATs live in the AI Gateway
- **Even if a Worker is compromised, the attacker gets nothing useful**

Manager is equally locked down: it knows what tasks Workers are doing, but has no access to API Keys or GitHub PATs. Its job is coordination, not execution.

| Dimension | Vanilla OpenClaw | HiClaw |
|-----------|-----------------|--------|
| Credential ownership | Each Agent holds its own | Workers hold Consumer Token only |
| Leak surface | Agent can output credentials directly | Manager has no access to real credentials |
| Attack surface | Every Agent is an entry point | Only Manager needs hardening |

### Human in the Loop: Transparent, Always Interruptible

Compared to OpenClaw's native Sub Agent system, HiClaw's Multi-Agent system is not just easier to use — it's **fully transparent**:

```
┌─────────────────────────────────────────────────────────────┐
│                  Matrix Project Room                        │
│                                                             │
│  You: Build a login page                                    │
│                                                             │
│  Manager: Got it, delegating...                             │
│           → @alice frontend page                            │
│           → @bob backend API                                │
│                                                             │
│  Alice: Working on the login component...                   │
│  Bob: API interface defined...                              │
│                                                             │
│  You: @bob wait, password must be at least 8 chars          │  ← intervene anytime
│                                                             │
│  Bob: Updated...                                            │
│  Alice: Got it, frontend validation updated too             │
│                                                             │
│  Manager: Task complete, please review                      │
└─────────────────────────────────────────────────────────────┘
```

**Core advantages**:
- **Full visibility**: All Agent collaboration happens in the Matrix room
- **Intervene anytime**: Spot an issue, @mention the Agent directly
- **Natural interaction**: Like working with a team in a group chat

### Manager's Core Capabilities

| Capability | Description |
|------------|-------------|
| **Worker lifecycle management** | "Create a frontend Worker" → auto-configures, assigns skills |
| **Automatic task delegation** | You state the goal, Manager breaks it down and assigns it |
| **Heartbeat monitoring** | Periodically checks Worker status, alerts you if something's stuck |
| **Project room setup** | Creates Matrix Rooms for projects, invites relevant participants |

### Worker Skills: A Safe Open Ecosystem

OpenClaw has a great open skills ecosystem at [skills.sh](https://skills.sh) — 80,000+ community skills you can install in one command: Higress WASM plugins, PR reviews, changelog generation...

But in vanilla OpenClaw, **you might hesitate to use them freely**. A public SKILL.md you haven't audited could trick an Agent into outputting credentials or running dangerous commands — because the Agent holds your API Keys.

**In HiClaw, it's a different story.** Every Worker runs in an isolated container and is designed to hold no real credentials:

```
What can a Worker see?
✅ Task files, code repos, its own working directory
✅ Consumer Token (like a keycard — grants API access only)
❌ Your LLM API Key
❌ Your GitHub PAT
❌ Any sensitive credentials
```

Even if a skill tries to steal credentials, there's nothing to steal. Workers can **freely pull skills from the public registry as needed**.

HiClaw ships Workers with a built-in `find-skills` skill. When a Worker encounters a task requiring specialized knowledge, it searches and installs the right skill automatically:

```
Manager assigns: "Build a Higress WASM Go plugin"
                  ↓
Worker finds it lacks the right tools
                  ↓
skills find higress wasm
  → alibaba/higress@higress-wasm-go-plugin  (3.2K installs)
                  ↓
skills add alibaba/higress@higress-wasm-go-plugin -g -y
                  ↓
Skill installed — Worker has full plugin dev scaffolding and workflow
```

**If you prefer a private registry**, HiClaw supports that too — choose Manual mode during install and set your private Skills Registry URL. You can also tell Manager to use a specific registry when creating Workers at any time.

As long as your private registry implements the same API as skills.sh, Workers switch seamlessly. The usage pattern is identical either way.

### Mobile Experience

HiClaw's built-in Matrix server supports multiple clients:

- **Works right after install**: No Slack/Teams bot setup needed
- **Direct from your phone**: Download any Matrix client (Element, FluffyChat, etc.)
- **Real-time push notifications**: Not buried in a "bot" folder
- **Full visibility**: You, Manager, and Workers all in the same Room

> 💡 Supported clients: Element, FluffyChat, and other major Matrix clients — iOS, Android, and Web.

---

## Get Started in 5 Minutes

### Step 1: Install

**macOS / Linux:**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows (PowerShell 7+):**

```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

> ⚠️ Windows users need **PowerShell 7+** and **Docker Desktop** first.

What the installer does:
- **Cross-platform**: bash on Mac/Linux, PowerShell on Windows — consistent experience
- **Smart mirror selection**: Picks the nearest image registry based on your timezone
- **Docker-based**: All components run in containers, OS differences abstracted away
- **Minimal config**: Just one LLM API Key required, everything else is optional

After install, you'll see:

```
=== HiClaw Manager Started! ===

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ★ Open the following URL in your browser to start:                           ★
                                                                                
    http://127.0.0.1:18088/#/login
                                                                                
  Login with:                                                                   
    Username: admin
    Password: [auto-generated password]
                                                                                
  After login, start chatting with the Manager!                                 
    Tell it: "Create a Worker named alice for frontend dev"                     
    The Manager will handle everything automatically.                           
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

> 💡 **No hosts file edits needed**: `*-local.hiclaw.io` resolves to `127.0.0.1` automatically.

### Step 2: Log in and start chatting

1. Open the URL shown after install (e.g. `http://127.0.0.1:18088`)
2. Log in with the credentials shown during install
3. You'll see a "Manager" conversation waiting for you

### Step 3: Create your first Worker

```
You: Create a Worker named alice for frontend development

Manager: On it...
         Worker alice created. Room: !xxx:matrix-local.hiclaw.io
         You can assign tasks to alice directly in the "Worker: Alice" room
```

### Step 4: Assign a task

```
You: @alice please build a simple login page using React

Alice: On it...
       [a few minutes later] Done! Code committed to GitHub, PR: https://github.com/xxx/pull/1
```

### Step 5: Check progress from your phone

1. Download a Matrix client (Element, FluffyChat — iOS/Android/Web)
2. At login, choose "Other server" and enter your Matrix server address
3. Monitor Worker progress and intervene anytime

---

## Solo Founder Playbook: One Person, One Team

Say you want to build a SaaS product — from idea to launch to growth. Traditionally you'd need product, design, engineering, QA, marketing... But now:

```
You: Create 4 Workers:
     - alex: product manager
     - sam: full-stack developer
     - taylor: content & marketing
     - jordan: data analyst

Manager: Done. 4 Workers created, each with independent skills and memory.

You: Kick off project: AI Writing Assistant MVP, launch target this week

Manager: Got it, planning now...

         [Creates project room, invites all Workers]
         [Drafts project plan]
         
         Here's the plan, please confirm:
         
         Phase 1: Product Definition
         - task-001: Competitive research & PRD (alex)
         
         Phase 2: Development
         - task-002: Tech stack & architecture (sam, depends on task-001)
         - task-003: Core feature development (sam, depends on task-002)
         - task-004: Product review (alex, depends on task-003)
         
         Phase 3: Launch Prep
         - task-005: Launch assets (taylor, depends on task-001)
         
         Confirm to start assigning.

You: Confirmed

Manager: [Sets project status to active]
         
         @alex New task [task-001]: Competitive research & PRD
         Goal: Output PRD defining MVP feature scope
         Spec: ~/hiclaw-fs/shared/tasks/task-001/spec.md
         Report back in the project room when done

[2 hours later]

alex: @manager task-001 complete
      PRD done: core features are AI autocomplete, multi-model switching, history
      Result: ~/hiclaw-fs/shared/tasks/task-001/result.md

Manager: [Reads result.md, status: SUCCESS]
         [Updates plan, marks task-001 complete]
         
         @sam New task [task-002]: Tech stack & architecture
         PRD reference: ~/hiclaw-fs/shared/tasks/task-001/result.md
         
         @taylor New task [task-005]: Launch assets
         Product positioning: ~/hiclaw-fs/shared/tasks/task-001/result.md

[4 hours later]

sam: @manager task-002 complete
     Stack: Next.js + Vercel + Supabase
     Estimated 8 hours for core development

Manager: [Updates plan, assigns task-003]
         
         @sam Continue with core feature development [task-003]

[8 hours later]

sam: @manager task-003 complete, deployed to Vercel
     Demo: https://xxx.vercel.app

Manager: [Updates plan, assigns task-004]
         
         @alex Please review [task-004]: Product acceptance
         Test URL: https://xxx.vercel.app
         Verify feature completeness against PRD

[Issue found during review]

alex: @manager task-004 needs revision
      Issue: multi-model switching has no onboarding — users don't know how to pick
      Suggestion: add a model comparison page
      Result: ~/hiclaw-fs/shared/tasks/task-004/result.md
      Status: REVISION_NEEDED → back to task-003

Manager: [Reads result.md, status: REVISION_NEEDED]
         [Creates revision task task-006]
         
         @sam task-004 review found issues, revision needed
         
         Revision task [task-006]: Add model selection onboarding page
         Feedback: ~/hiclaw-fs/shared/tasks/task-004/result.md
         Report back when done

[Revision complete]

sam: @manager task-006 complete, onboarding page added

Manager: [task-006 done, task-004 re-review passed]
         [All development tasks complete]
         
         @you MVP development complete!
         - Deployed: https://xxx.vercel.app
         - Product review passed
         - Launch assets ready
         
         Project plan: ~/hiclaw-fs/shared/projects/proj-xxx/plan.md

You: Ready to launch. @taylor Product Hunt tomorrow

taylor: Got it, scheduled. Assets are ready.

[Launch day — Manager auto-monitors progress]

Manager: @you Product Hunt launch update
         Current rank: #3
         Upvotes: 423
         Comments: 87
         
         @jordan please set up analytics tracking

jordan: On it, configuring GA4 + custom events...

[Data ready]

jordan: @manager tracking setup complete
        Dashboard: https://analytics.google.com/xxx
        
        Day 1 data:
        - Signups: 1,247
        - Day 2 retention: 34%
        - AI autocomplete usage: 78%
        - Multi-model switching usage: 23%

Manager: @you Project "AI Writing Assistant MVP" — Day 1 Report
         
         Key metrics:
         - Day 1 signups: 1,247
         - Day 2 retention: 34%
         - Feature usage: autocomplete 78%, model switching 23%
         
         Insight: model switching usage is low
         Suggestion: @alex investigate and optimize the onboarding flow

[And so it goes — Manager runs the whole loop: plan → assign → monitor → coordinate → report]
```

**What did Manager actually do?**

| Phase | Manager's role |
|-------|---------------|
| **Project planning** | Breaks goal into tasks, identifies dependencies |
| **Task assignment** | @mentions Workers with task context |
| **Progress tracking** | Updates plan on completion, triggers next steps |
| **Issue handling** | Review fails → auto-creates revision task |
| **Status reporting** | Proactively updates you at key milestones |
| **Risk flagging** | Spots anomalies in data, suggests optimizations |

**You make the decisions. Manager handles the rest.**

---

## Open Source

- **GitHub**: https://github.com/higress-group/hiclaw
- **Docs**: https://github.com/higress-group/hiclaw/tree/main/docs
- **Community**: Join our Discord / DingTalk / WeChat group

---

## Closing Thoughts

HiClaw is an evolution of OpenClaw — not a replacement, an upgrade.

We kept everything that makes OpenClaw great (natural language interaction, Skills ecosystem, MCP tooling) while fixing the security and usability pain points.

If you're a:
- **Solo developer** who wants to do a team's worth of work alone
- **Power OpenClaw user** who wants a safer, smoother experience
- **Solo founder** who needs AI teammates to share the load

HiClaw is for you.

**Start now:**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

---

*HiClaw is open source under the Apache 2.0 license. If you find it useful, a Star ⭐ and contributions are always welcome!*
