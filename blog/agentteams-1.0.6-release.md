# HiClaw 1.0.6: Enterprise-Grade MCP Server Management — Zero Credential Exposure, Maximum Tool Access

> Release Date: March 14, 2026

---

## The Credential Security Problem

If you're running AI agents in production, you've probably faced this dilemma:

**"I want my agents to use GitHub, but I don't want to give them my PAT"** — One leaked token and your repositories are compromised.

**"I need workers to call internal APIs, but those keys are too sensitive"** — API keys for billing systems, databases, payment gateways... giving them to agents is just too risky.

**"Different workers need different permissions, but managing this is a nightmare"** — Should the frontend worker have access to the production database? Probably not. But how do you enforce this?

In version 1.0.6, we have a comprehensive solution: **Enterprise-Grade MCP Server Management with Higress AI Gateway + mcporter**.

---

## What's MCP and Why Does It Matter?

**MCP (Model Context Protocol)** is an open standard for exposing APIs as tools that AI agents can discover and call. Think of it as "OpenAPI for AI agents" — instead of manually documenting API endpoints, you define them once as MCP tools, and any MCP-compatible agent can use them immediately.

The beauty of MCP is that it separates **tool definition** from **credential management**. The tool schema says "this API does X with parameters Y", but it doesn't say "here's the API key". That separation is the foundation of secure enterprise deployments.

---

## Introducing mcporter: The Universal MCP CLI

Before diving into HiClaw's integration, let us introduce [**mcporter**](https://github.com/steipete/mcporter) — a powerful MCP toolkit developed by [Peter Steinberger](https://github.com/steipete), the creator of OpenClaw.

mcporter is a TypeScript runtime, CLI, and code-generation toolkit for MCP. Key capabilities:

- **Zero-config discovery**: Auto-discovers MCP servers configured in Cursor, Claude Code, Codex, Windsurf, and VS Code
- **Friendly CLI**: Call any MCP tool with `mcporter call server.tool key=value`
- **Type-safe**: Generate TypeScript clients with full type inference
- **One-command CLI generation**: Turn any MCP server into a standalone CLI tool

```bash
# List all configured MCP servers
mcporter list

# View a server's tools with full parameter schemas
mcporter list github --schema

# Call a tool
mcporter call github.search_repositories query="hiclaw" limit=5
```

In HiClaw 1.0.6, both Manager and Workers use mcporter to interact with MCP servers — but with a crucial security enhancement through the Higress AI Gateway.

---

## MCP and SKILLS: Complementary, Not Competing

Before diving into HiClaw's MCP integration architecture, it's worth clarifying the **relationship between MCP and SKILLS**.

### HiClaw's Open Skills Ecosystem

HiClaw supports an **open SKILLS marketplace** through [skills.sh](https://skills.sh), as well as **enterprise self-hosted SKILLS marketplaces** via Nacos. SKILLS are scenario-oriented, iterable capability packages:

- **Scenario-oriented**: Combine atomic tools into complete business workflows
- **Continuously evolving**: Optimize and improve based on real-world experience
- **Knowledge accumulation**: Include best practices, error handling, and parameter descriptions

### MCP's Role in the Ecosystem

MCP is **not meant to replace SKILLS** — it serves as a **complement to the SKILLS ecosystem**, enabling rapid conversion of existing APIs into standardized agent-usable tools. MCP's core value lies in:

- **Clear constraints and specifications**: More rigorous tool definitions and type constraints
- **Permission governance system**: Reuse MCP's authentication/authorization capabilities (MCP Server-level permission management, tool-level supported in Enterprise Edition)
- **Batch conversion capability**: Especially in enterprise scenarios, Higress-based MCP gateway capabilities enable painless batch conversion of existing APIs into MCP tools with fine-grained management

### mcporter as a Bridge

Through CLI tools like mcporter, HiClaw achieves **re-orchestration and organization of MCP tools**, forming iterable SKILLs:

```
MCP Tools (Atomic Capabilities)
    ↓ mcporter orchestration
SKILLs (Scenario-based Capability Packages)
    ↓ Practical usage
SKILL Iterative Optimization
```

### SKILL + MCP = 1+1 > 2

The best practice for both is:

- **SKILLs handle scenario evolution**: Adapt to actual business scenarios, continuously iterate on skill composition logic and best practices
- **MCP handles fine-grained permission control**: Align with business capabilities for MCP Server and tool permission governance and credential management

This complementary relationship achieves the **SKILL + MCP 1+1 > 2 effect**: Enterprises can enjoy the rich capabilities of the open SKILLS marketplace while maintaining secure, fine-grained permission control over internal APIs through the MCP gateway.

---

## The Architecture: How It All Works

Here's the complete flow when you want to add a new API tool for your workers:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              YOU (Human)                                     │
│                                                                              │
│  "Add a stock index API: GET https://api.finance.com/v1/index?symbol={symbol}"│
│  "Auth via X-API-Key header, here's my key: sk_xxx"                         │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           MANAGER CLAW                                       │
│                                                                              │
│  1. Generates MCP Server YAML config from your description                  │
│  2. Runs setup-mcp-server.sh stock-index "sk_xxx" --yaml-file /tmp/stock.yaml │
│  3. Verifies with mcporter: mcporter call stock-index.get_index symbol=000001.SH │
│  4. Notifies Workers: "New MCP server 'stock-index' is ready"               │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        HIGRESS AI GATEWAY                                    │
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  MCP Server: stock-index-mcp-server                                  │   │
│  │  ├─ Real Credential: sk_xxx (STORED SECURELY, NEVER EXPOSED)        │   │
│  │  ├─ Tool: get_index(symbol: string) → stock index data              │   │
│  │  └─ Authorized Consumers: manager, worker-alice, worker-bob         │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                              │
│  Issues temporary consumer tokens to Workers                                │
│  Tokens can only call authorized MCP servers                                │
│  Real API keys NEVER leave the gateway                                      │
└────────────────────────────────────┬────────────────────────────────────────┘
                                     │
                                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           WORKER CLAW                                        │
│                                                                              │
│  1. Receives notification from Manager                                      │
│  2. Pulls updated mcporter config from MinIO                                │
│  3. Discovers tools: mcporter list stock-index --schema                     │
│  4. Tests tool: mcporter call stock-index.get_index symbol=000001.SH        │
│  5. Generates SKILL.md based on understanding                               │
│  6. Ready to use in future tasks!                                           │
│                                                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │  Worker's View:                                                      │   │
│  │  ├─ Has: Consumer token (like an "ID badge")                        │   │
│  │  ├─ Can do: Call stock-index.get_index via gateway                  │   │
│  │  └─ Cannot do: See the real API key sk_xxx                          │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

Once configured, the Manager can view the list of tools under the MCP Server in the **Higress Console** and perform further permission management:

![Higress MCP Tool Management Interface](https://img.alicdn.com/imgextra/i3/O1CN01armHyk1Lc9XTGz7ZX_!!6000000001319-2-tps-1452-849.png)

Through the console, you can:
- View all tools contained in the MCP Server
- Configure MCP Server-level access permissions for each Consumer (Worker)
- Monitor tool invocation and performance metrics
- Dynamically adjust permission policies without restarting services

> **Permission Management Notes**:
> - **Higress Open Source**: Supports **MCP Server-level** permission management (e.g., Worker A can access the entire `stock-index` MCP Server)
> - **Higress Enterprise Edition**: Supports **tool-level** permission management (e.g., Worker A can only call `stock-index.get_index`, but not `stock-index.update_index`)
> 
> HiClaw 1.0.6 implements MCP Server-level security isolation based on the open-source version, which already meets the permission control requirements of most enterprise scenarios.

**Key Security Principle: Workers never see real credentials.**

Even if a worker is completely compromised, an attacker only gets a consumer token that:
- Can only call specific MCP servers you've authorized
- Can be instantly revoked by the Manager
- Contains no reusable credential material

---

## End-to-End Example: Adding a Custom API

Let's walk through a real scenario. You have an internal billing API and want workers to query customer data.

### Step 1: Describe the API to Manager

In your Matrix room, tell the Manager:

```
You: I want to add our billing API as an MCP tool.
     Endpoint: GET https://billing.internal.company.com/api/v1/customers/{customer_id}
     Auth: Bearer token in Authorization header
     Here's the token: Bearer eyJhbGciOiJSUzI1NiIs...
```

### Step 2: Manager Does the Heavy Lifting

The Manager:

1. **Generates the YAML config**:

```yaml
server:
  name: billing-mcp-server
  config:
    accessToken: ""  # Manager substitutes your real token here
tools:
- name: get_customer
  description: "Get customer details by ID"
  args:
  - name: customer_id
    description: "Customer ID (e.g., CUST-12345)"
    type: string
    required: true
  requestTemplate:
    url: "https://billing.internal.company.com/api/v1/customers/{{.args.customer_id}}"
    method: GET
    headers:
    - key: Authorization
      value: "Bearer {{.config.accessToken}}"
```

2. **Deploys to Higress MCP Gateway**:
```bash
setup-mcp-server.sh billing "eyJhbGciOiJSUzI1NiIs..." --yaml-file /tmp/mcp-billing.yaml
```

3. **Verifies with mcporter**:
```bash
mcporter list billing --schema
mcporter call billing.get_customer customer_id=CUST-12345
```

4. **Notifies relevant Workers**:
```
Manager: @alice @bob New MCP server `billing` has been configured with tools: get_customer.
         Please use your file-sync skill to pull the updated config, then use your mcporter skill to discover and learn the new tools.
```

### Step 3: Workers Auto-Configure

Worker Alice:

```bash
# Pull the updated config from MinIO
hiclaw-sync

# Discover the new tools
mcporter list billing --schema

# Test the tool
mcporter call billing.get_customer customer_id=CUST-12345

# Generate a SKILL for future reference
mkdir -p ~/skills/billing-operations
# ... writes SKILL.md with tool documentation
```

### Step 4: Worker Uses the Tool in Tasks

Later, when you assign a task:

```
You: @alice check if customer CUST-12345 has any overdue invoices

Alice: Let me query the billing system...
       mcporter call billing.get_customer customer_id=CUST-12345
       
       Customer: Acme Corp
       Status: Active
       Overdue: Yes, $4,500 since Jan 15
       
       The customer does have overdue invoices.
```

**Alice never saw the real API token. She only used her consumer token to call through the gateway.**

---

## From Swagger/OpenAPI to MCP Tools

What if you have an existing API documented in Swagger or OpenAPI format? Even easier.

```
You: Here's our product catalog API Swagger spec:
     https://docs.internal.company.com/swagger.json
     Auth via X-API-Key header. Key: prod_cat_xxx

Manager: I'll convert the Swagger spec to MCP tools...
         Found 12 endpoints, creating 12 MCP tools...
         Deployed to Higress as `product-catalog` MCP server.
         
         Testing first tool... ✓
         Testing second tool... ✓
         All tools verified.
         
         @warehouse-team New MCP server `product-catalog` is ready with 12 tools.
```

The Manager can read Swagger/OpenAPI specs and automatically generate the corresponding MCP server configuration.

---

## From curl to MCP Tool

Even simpler — just paste a curl command:

```
You: Add this API call as a tool:
     curl -X GET "https://api.shipping.com/v1/track?tracking_id=ABC123" \
          -H "X-API-Key: ship_xxx"

Manager: Creating MCP server `shipping` with tool `track_package`...
         Deployed and tested. @logistics-team you can now use track_package.
```

---

## Worker-Generated Skills: Self-Improving Documentation

A unique feature of HiClaw 1.0.6 is that Workers don't just use MCP tools — they **document them**.

When a Worker first encounters a new MCP server, it:

1. **Discovers all tools** via `mcporter list --schema`
2. **Tests representative tools** to understand behavior
3. **Generates a SKILL.md** with:
   - Tool descriptions in natural language
   - Example `mcporter call` commands
   - Parameter notes and common patterns
   - Gotchas learned from testing

This SKILL becomes the Worker's permanent reference for that MCP server. Over time, as the Worker uses the tools more, it can **improve the SKILL** based on real-world experience:

- Adding error handling tips
- Documenting rate limits
- Noting which parameters are actually required vs. optional
- Sharing best practices

It's like having an AI that writes its own documentation — and improves it over time.

---

## Security Model: Defense in Depth

Let's be explicit about what Workers can and cannot do:

### What Workers CAN Do
- ✅ Call any MCP server they're authorized for
- ✅ Use tools through the Higress AI Gateway
- ✅ Generate and improve SKILL documentation
- ✅ Work autonomously within their authorized scope

### What Workers CANNOT Do
- ❌ See real API keys, tokens, or credentials
- ❌ Call MCP servers they're not authorized for
- ❌ Extract credentials from the gateway
- ❌ Share credentials with other Workers

### What Happens If a Worker Is Compromised
1. **Attacker gets a consumer token** — only useful within HiClaw
2. **Manager revokes the token instantly** — no credential rotation needed
3. **Real API keys remain safe** — they never left the gateway
4. **Create a new Worker** — back to work in minutes

This is the principle of **credential zero-trust**: agents operate on a need-to-know basis, and they never need to know the actual credentials.

---

## What This Means for Roadmap

This release completes the **"Universal MCP Service Support"** item from our roadmap:

- ✅ **Preset MCP connectors**: GitHub, plus any custom service via YAML config
- ✅ **Custom MCP integration**: Bring any HTTP API, documented via Swagger/curl/YAML
- ✅ **Fine-grained permission control**: Manager authorizes Workers per-MCP-server

**Any MCP-compatible tool can now be safely exposed to Workers with zero credential leakage.**

---

## Other Improvements

### Slash Commands: Cross-Scenario Control

Version 1.0.6 also introduces slash commands that work anywhere — in DM or group chats:

#### /reset — Fix Stuck or Misconfigured Claws

```
You: @alice /reset

Alice: Context cleared. Ready for new tasks.
```

Useful when:
- A claw is stuck in a loop
- A misconfiguration is causing issues
- You want a fresh start on a task

#### /stop — Interrupt Long-Running Tasks

```
You: @bob /stop

Bob: Task interrupted. Here's what I was doing:
     - Analyzed 45% of the codebase
     - Found 3 potential issues
     - Had not yet started the fix
     
     Why did you stop me? Do you want me to change direction?
```

Useful when:
- A task is taking too long
- You want to provide mid-task guidance
- Manager wants to intervene on a Worker's approach

**Manager can use `/stop` on Workers** to provide course corrections without waiting for task completion.

### Optimized File Sync

The file sync design principle has been unified: **"Writer pushes and notifies, receiver pulls on demand"**, with 5-min periodic pull as fallback only. Manager adds on-demand `mc mirror` pull in task/project completion flows to ensure reading fresh Worker results.

---

## Getting Started

Already running HiClaw? Upgrade to the latest version:

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

New to HiClaw? One command gets you started:

```bash
# macOS / Linux
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)

# Windows (PowerShell 7+)
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

After installation, open Element Web at http://127.0.0.1:18088 and tell your Manager to add some MCP tools!

---

## What's Next

We're continuing to improve HiClaw. Coming soon:

- **Team Management Console**: Real-time visualization of all agents, task timelines, and resource monitoring
- **More Worker runtimes**: ZeroClaw (Rust-based, 3.4MB), NanoClaw (minimal OpenClaw alternative)
- **Enhanced MCP discovery**: Auto-import from popular MCP registries

Join our community: [Discord](https://discord.com/invite/NVjNA4BAVw) | [DingTalk](https://qr.dingtalk.com/action/joingroup?code=v1,k1,q3lHf2AY4o0W2aBsoyJE0kgYnGcBFqpBuwDTjJ36iu8=)

---

## Changelog Highlights

### What's New

- **MCP Server Management Skill Enhancement** — Unified `setup-mcp-server.sh` script for runtime MCP server creation/update. Workers get independent mcporter skill with tool discovery and automatic SKILL generation.
- **Slash Commands** — `/reset` to clear context, `/stop` to interrupt tasks. Works in DM and group chats.
- **Optimized file sync** — "Writer pushes and notifies, receiver pulls on demand" design principle.

### Bug Fixes

- Fixed orphaned session write lock cleanup, Remote->Local sync logic, Matrix room preset, mcporter config path compatibility, and more.

See the [full release notes](https://github.com/alibaba/hiclaw/releases/tag/v1.0.6) for details.
