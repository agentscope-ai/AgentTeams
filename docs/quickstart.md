# HiClaw Quickstart Guide

This guide walks you through installing HiClaw, creating your first Agent team, and completing your first collaborative task. Each step includes verification checkpoints to confirm everything is working correctly.

## Prerequisites

- Docker installed and running
- An LLM API key (e.g., Qwen, OpenAI)
- (Optional) A GitHub Personal Access Token for GitHub collaboration features

---

## Step 1: Install Manager and Login to IM

**POC Case 1: Manager boots, all services healthy, IM login**

### 1.1 Run the installer

**Option A: One-line install**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

Follow the interactive prompts to configure:
- LLM Provider and API Key
- Admin username and password
- Domain names (press Enter to accept defaults)
- GitHub PAT (optional)

**Option B: Using Make (for developers who cloned the repo)**

```bash
# Minimal install — only LLM key required, all defaults applied
HICLAW_LLM_API_KEY="sk-xxx" make install
```

This builds images locally, mounts the container runtime socket (for direct Worker creation), and saves config to `./hiclaw-manager.env`.

Both methods support environment variable overrides for all settings. See `install/hiclaw-install.sh` header for the full list.

### 1.1a Multi-container layout (v1.1.0+ embedded install)

The default **embedded** install starts two main containers (see [architecture.md](architecture.md)):

| Container | Role |
|-----------|------|
| **`hiclaw-controller`** | Bundles Higress, Tuwunel, MinIO, Element Web, and the Go controller (REST API on port **8090** inside the Docker network). |
| **`hiclaw-manager`** | Lightweight Manager Agent only (OpenClaw **or** QwenPaw when `HICLAW_MANAGER_RUNTIME=copaw`). |

Worker containers (`hiclaw-worker-*`, `hiclaw-copaw-worker-*`, `hiclaw-hermes-worker-*`) are created when you add Workers.

**Declarative CLI (no chat required):** The `hiclaw` binary is available **inside** `hiclaw-controller` and `hiclaw-manager`. For quick checks and provisioning from the host:

```bash
docker exec hiclaw-controller hiclaw create worker --name alice --model qwen3.5-plus
docker exec hiclaw-controller hiclaw get workers
```

For YAML-driven workflows, use `install/hiclaw-apply.sh` (copies files into `hiclaw-manager` and runs `hiclaw apply -f`). Details: [Declarative Resource Management](declarative-resource-management.md).

### 1.2 Login to Element Web

Open http://127.0.0.1:18088 in your browser (direct access port). Alternatively, access via the gateway at http://matrix-client-local.hiclaw.io:18080 if you've added the domain to your `/etc/hosts`.

Login with your admin credentials.

### Verification Checklist

- [ ] **`hiclaw-controller`** is running (embedded stack): `docker ps | grep hiclaw-controller`
- [ ] **`hiclaw-manager`** is running: `docker ps | grep hiclaw-manager`
- [ ] Element Web loads in browser at http://127.0.0.1:18088
- [ ] Login with admin credentials succeeds
- [ ] Higress Console at http://localhost:18001 (gateway **host** port defaults to **18080**; Matrix/Element use that gateway for `*-local.hiclaw.io` hostnames)
- [ ] MinIO is reachable **inside** the controller container (embedded install does **not** publish MinIO console on the host by default): `docker exec hiclaw-controller curl -sf http://127.0.0.1:9000/minio/health/live`
- [ ] (OpenClaw Manager only) OpenClaw control UI at http://127.0.0.1:18888

---

## Step 2: Create Worker Alice

**POC Case 2: Create Worker via Matrix conversation**

### 2.1 Chat with Manager

**Option A: Via Element Web (GUI)**

In Element Web, start a direct message (DM) with the `manager` user.

Send:
> Please create a new Worker named alice for frontend development tasks. She should have access to GitHub MCP.

**Option B: Via CLI (make replay)**

```bash
make replay TASK="Please create a new Worker named alice for frontend development tasks. She should have access to GitHub MCP."
```

This sends the message via the Matrix API and waits for the Manager's reply in the terminal.

### 2.2 Wait for Manager Response

The Manager Agent will:
1. Register an `alice` Matrix account
2. Create a Higress consumer `worker-alice` with key-auth credentials
3. Generate Alice's configuration files in MinIO
4. Create a Matrix Room (you, Manager, and Alice)
5. Start the Worker (direct creation or install command, depending on your request and whether the container runtime socket is mounted)

### 2.3 Start Worker Alice

There are two ways to start the Worker:

**Option A: Direct Creation (Local Deployment)**

If you asked the Manager to "create it directly", the Manager will automatically create and start the Worker container on the host machine via the mounted container runtime socket. No manual steps needed.

> This requires `make install` (which mounts the socket automatically) or manually mounting the Docker/Podman socket when starting the Manager container.

**Option B: Docker Run Command (Remote or Manual Deployment)**

If the Manager doesn't have access to the container runtime socket, it will reply with a `docker run` command. Copy and run it on the target host:

```bash
docker run -d --name hiclaw-worker-alice \
  -e HICLAW_WORKER_NAME=alice \
  -e HICLAW_FS_ENDPOINT=http://<MANAGER_HOST>:9000 \
  -e HICLAW_FS_ACCESS_KEY=<ACCESS_KEY> \
  -e HICLAW_FS_SECRET_KEY=<SECRET_KEY> \
  hiclaw/worker-agent:latest
```

The Manager will provide all the specific values in its reply.

### Verification Checklist

- [ ] Alice's Room appears in Element Web (3 members: you, manager, alice)
- [ ] Higress Console shows `worker-alice` consumer (http://localhost:18001)
- [ ] MinIO has `agents/alice/SOUL.md` file (accessible via MinIO Console or `mc ls`)
- [ ] Worker container running: `docker ps | grep hiclaw-worker-alice`

---

## Step 3: Assign Task to Alice

**POC Case 3: Assign task in Room, Worker completes**

### 3.1 Send task in Alice's Room

Open Alice's Room in Element Web and send:

> Alice, please create a simple README.md for a hello-world project. Include the project name, description, and usage instructions. Save the result to the shared task folder.

### 3.2 Observe task execution

Watch the Room as:
1. Manager receives and relays the task
2. Task metadata and spec appear in MinIO (`shared/tasks/{task-id}/meta.json` and `spec.md`)
3. Alice works on the task
4. Alice writes the result (`shared/tasks/{task-id}/result.md`)
5. Alice notifies completion in the Room
6. Manager updates `meta.json` status to `completed`

### Verification Checklist

- [ ] Manager creates task `meta.json` and `spec.md` in MinIO
- [ ] Alice acknowledges and begins working
- [ ] Alice posts progress updates in Room
- [ ] Result file appears in MinIO shared tasks
- [ ] Alice notifies completion in Room
- [ ] Task `meta.json` status updated to `completed`

---

## Step 4: Human Intervenes Mid-Task

**POC Case 4: Human sends supplementary instructions**

### 4.1 Assign a new task

In Alice's Room, send:

> Alice, write a Python script that prints 'Hello, World!' and save it as hello.py.

### 4.2 Send supplementary instruction

While Alice is working, send an additional instruction:

> Additional requirement: the script should also accept a command line argument for the name, so it prints 'Hello, <name>!' instead.

### 4.3 Observe incorporation

Alice and Manager should incorporate both the original and supplementary requirements.

### Verification Checklist

- [ ] Manager relays both original and supplementary instructions
- [ ] Alice acknowledges the additional requirement
- [ ] Final result includes both original and supplementary features

---

## Step 5: Observe Heartbeat

**POC Case 5: Heartbeat triggers Manager inquiry**

### 5.1 Assign a longer task

Send a task that takes some time to complete.

### 5.2 Wait for heartbeat cycle

The Manager Agent runs a heartbeat check periodically (triggered by OpenClaw's built-in heartbeat mechanism). During the heartbeat:
- Manager checks each Worker's Room for recent activity
- For Workers with assigned tasks, Manager asks for status
- The inquiry is visible in the Room

### Verification Checklist

- [ ] Manager sends a status inquiry message in Alice's Room
- [ ] Alice responds with current progress
- [ ] Human admin can see the entire exchange in the Room

---

## Step 6: Create Worker Bob and Collaborate

**POC Case 6: Multi-Worker collaboration**

### 6.1 Create Worker Bob

In your DM with Manager, send:

> Create a new Worker named bob for backend development. He should have access to GitHub MCP.

### 6.2 Install Bob

Follow the same process as Alice (Step 2).

### 6.3 Assign collaborative task

In your DM with Manager, send:

> I need Alice and Bob to collaborate: Alice should create the frontend HTML page, and Bob should create the backend API. They should coordinate via shared files in MinIO.

### Verification Checklist

- [ ] Bob's Room appears in Element Web (3 members)
- [ ] Higress Console shows `worker-bob` consumer
- [ ] Manager splits task between Alice and Bob
- [ ] Both Workers communicate progress in their respective Rooms
- [ ] Shared coordination files appear in MinIO

---

## Step 7: GitHub Operations via MCP

**POC Case 7: GitHub code operations**

> **Note**: This step requires a GitHub PAT to be configured during Manager installation.

### 7.1 Assign GitHub task

In Alice's Room, send:

> Alice, please perform these GitHub operations: 1) Read the README.md of our test repo, 2) Create a branch named 'feature/alice-update', 3) Create a new file docs/quickstart-update.md, 4) Create a Pull Request.

### 7.2 Observe MCP tool calls

Alice uses `mcporter` to call the GitHub MCP Server hosted by Higress. The MCP Server holds the GitHub PAT centrally -- Alice never sees it.

### Verification Checklist

- [ ] Alice reports reading the repo contents
- [ ] Alice reports creating the branch
- [ ] Alice reports creating the file
- [ ] Alice reports creating the PR
- [ ] Verify the PR exists on GitHub

---

## Step 8: Multi-Worker GitHub Collaboration

**POC Case 8: Alice and Bob collaborate on GitHub**

### 8.1 Assign collaborative GitHub task

In your DM with Manager, send:

> Alice and Bob should collaborate on the test repo: Alice creates branch 'feature/alice-docs' and adds docs/alice.md, Bob creates branch 'feature/bob-api' and adds src/bob.py. Both should create separate PRs.

### Verification Checklist

- [ ] Alice creates her branch and file
- [ ] Bob creates his branch and file
- [ ] Two separate PRs exist on GitHub
- [ ] Both Workers report completion in their respective Rooms

---

## Step 9: Dynamic MCP Permission Control

**POC Case 9: MCP permission revoke and restore**

### 9.1 Revoke Alice's GitHub access

In your DM with Manager, send:

> Revoke Alice's access to the GitHub MCP Server.

### 9.2 Verify revocation

Ask Alice to perform a GitHub operation. She should get a 403 error.

### 9.3 Restore access

In your DM with Manager, send:

> Restore Alice's access to the GitHub MCP Server.

### 9.4 Verify restoration

Ask Alice to perform a GitHub operation again. It should succeed.

### Verification Checklist

- [ ] Manager confirms revocation
- [ ] Alice gets 403 when trying GitHub operations
- [ ] Manager confirms restoration
- [ ] Alice can perform GitHub operations again

---

## Congratulations!

You have successfully completed all 10 verification steps for HiClaw. Your Agent team is fully operational with:

- IM-based communication (Matrix)
- Human-in-the-loop oversight
- Multi-Worker collaboration
- Centralized credential management
- MCP-based external tool integration
- Dynamic permission control

--

## Uninstall

To completely remove HiClaw and all its data:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/higress-group/hiclaw/main/install/hiclaw-install.sh) uninstall
```

This matches `install/hiclaw-install.sh uninstall`: it stops and removes **`hiclaw-manager`**, all **`hiclaw-worker-*`** (and other worker) containers, **`hiclaw-controller`** (embedded Higress / Tuwunel / MinIO / Element Web), optional **`hiclaw-docker-proxy`**, the **`hiclaw-data`** Docker volume, your **`hiclaw-manager.env`** file, the workspace directory, the **`hiclaw-net`** network, and the install log.
