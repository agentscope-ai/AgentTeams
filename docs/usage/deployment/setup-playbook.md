# AgentTeams SETUP Playbook (agent-executable)

English | [中文](../../zh-cn/usage/deployment/setup-playbook.md)

This playbook lets an **agent** drive a fresh macOS/Linux host from an
empty environment to a working AgentTeams instance. It is written as
three parts — collect (one question at a time), plan (review), execute
(each step with a pre-check, an action, a verification, and an explicit
failure-recovery next step). The underlying details live in the
[Local Deployment Guide](local.md); this playbook is the operating
procedure, not a duplicate of it.

## Ground rules

1. **One question at a time.** The agent asks a single question, waits
   for the answer, and records it in the plan (Part 2). It never asks a
   bundle of questions or starts executing mid-collection.
2. **Never accept a secret in chat.** The LLM API key (and any other
   secret) is never typed into a chat, Matrix room, or issue. The human
   places the key in a root-only file and tells the agent the file
   path; the agent reads the value only by passing the file to the
   installer, and never prints it back.
3. **Collect → plan → execute.** Nothing is installed before the plan
   in Part 2 has been reviewed and approved by the human.
4. **Every step is recoverable.** Each execution step states its
   pre-check, the action, how to verify success, and the concrete next
   step when it fails. A failure never ends in "try again" — it names
   what to check and who must act.

## Part 1 — Collect (one question at a time)

Ask these questions in order. Each lists the recommended answer; the
agent should confirm the recommendation with the human rather than
assume it, and record the final value in the plan.

| # | Question | Recommended answer | Recorded as |
|---|---|---|---|
| Q1 | Which container runtime is running on this host, and can the current user run `docker ps` (or `podman ps`) without sudo? | Docker (Docker Desktop on macOS, Docker Engine on Linux), already started. | runtime = docker / podman |
| Q2 | Which AgentTeams version? | The latest **stable release** tag (e.g. `v1.2.x`) — never `latest` for anything you will keep. | `AGENTTEAMS_VERSION` |
| Q3 | Which LLM provider and model? | `qwen` (built-in Alibaba Cloud Model Studio integration) or `openai-compat` with a Base URL that ends in `/v1`. The model ID must exactly match a model the provider accepts. | `AGENTTEAMS_LLM_PROVIDER`, `AGENTTEAMS_OPENAI_BASE_URL` (empty for `qwen`), `AGENTTEAMS_DEFAULT_MODEL` |
| Q4 | Where is the LLM API key? (Never paste it into this conversation.) | The human writes it to `~/.agentteams-llm-key` with `chmod 600` (Step 1, Part 3) and confirms the path. | key file path |
| Q5 | Embedding model? | For `qwen`: `text-embedding-v4`. For a provider without an embedding API: empty (embeddings disabled). | `AGENTTEAMS_EMBEDDING_MODEL` |
| Q6 | Administrator password? | Leave it empty — the installer generates one and prints it at the end; the human copies it from the terminal into a password manager. Never into chat. | (generated) |
| Q7 | Manager runtime? | `qwenpaw`. | `AGENTTEAMS_MANAGER_RUNTIME` |
| Q8 | Default Worker runtime? | `qwenpaw`. | `AGENTTEAMS_DEFAULT_WORKER_RUNTIME` |
| Q9 | Access scope? | Local access only (`AGENTTEAMS_LOCAL_ONLY=1`) for a personal/evaluation host. | `AGENTTEAMS_LOCAL_ONLY` |
| Q10 | Ports? | Defaults: `18080` gateway, `18001` Higress Console, `18088` Element Web, `13000` Dashboard — after the pre-flight confirms none are taken. | `AGENTTEAMS_PORT_*` (changed only on conflict) |
| Q11 | GitHub token? | Skip for now; add a dedicated, narrowly-scoped token later if GitHub MCP is needed. | (unset) |
| Q12 | Dashboard? | Enabled on `13000`. | `AGENTTEAMS_DASHBOARD=1` |
| Q13 | Data volume / workspace directories? | Defaults: volume `agentteams-data`, workspace `${HOME}/agentteams-manager`. The workspace must not live in a temporary directory. | `AGENTTEAMS_DATA_DIR`, `AGENTTEAMS_WORKSPACE_DIR` |
| Q14 | Matrix E2EE? | Disabled (`AGENTTEAMS_MATRIX_E2EE=0`) unless the chosen runtimes and clients are known to support the current E2EE workflow. | `AGENTTEAMS_MATRIX_E2EE` |

## Part 2 — Plan (fill after collection; the human reviews)

The agent fills this template from the recorded answers and the
pre-flight results (Step 0), then stops and waits for approval:

```text
PLAN (AgentTeams SETUP)
Host            : <OS, runtime = docker|podman>
Version         : AGENTTEAMS_VERSION=<tag>
LLM             : provider=<qwen|openai-compat>, base_url=<… or (built-in)>,
                  model=<id>, embedding=<id|disabled>, key file=~/.agentteams-llm-key
Admin           : username=admin, password=<installer-generated>
Runtimes        : manager=<qwenpaw>, default worker=<qwenpaw>
Access          : local-only=<1|0>, ports: gateway=18080 console=18001 element=18088 dashboard=13000
Data            : volume=agentteams-data, workspace=${HOME}/agentteams-manager
E2EE            : <0|1>
Pre-flight      : runtime OK / free disk >= 10 GB / free memory >= 4 GB / ports free
Install command : (the exact non-interactive command from Step 2, with every
                  value substituted — no secrets, the key is read from the file)
```

Nothing in Part 3 runs until this plan is approved.

## Part 3 — Execute (empty environment → working instance)

### Step 0 — Pre-flight (agent-executable)

- **Pre-check:** none (this is the check).
- **Action:** run, from the host:
  ```bash
  docker ps                       # or: podman ps
  df -h "$HOME"                   # need >= 10 GB free
  free -h                         # macOS: top -l 1 | head -10; need >= 4 GB
  lsof -nP -iTCP:18080,18001,18088,13000 -sTCP:LISTEN   # Linux without lsof: ss -lnt
  ```
- **Verify:** the runtime answers; disk and memory meet the minimums;
  none of the four ports (or the chosen alternates) is listening.
- **Failure recovery:**
  - runtime missing/not started → HUMAN: start Docker Desktop (macOS) or
    `systemctl start docker` (Linux); then re-run Step 0.
  - disk < 10 GB or memory < 4 GB → HUMAN: free resources; re-run Step 0.
  - a port is taken → note the owning process from `lsof`/`ss` output,
    pick a new port for that service, record it in the plan, re-check.

### Step 1 — Place the LLM API key (HUMAN ONLY — terminal commands given separately)

The agent does not perform this step and never sees the key. It asks
the human to run, in a terminal (not in chat):

```bash
umask 077
printf '%s' 'paste-your-llm-api-key-here' > ~/.agentteams-llm-key
chmod 600 ~/.agentteams-llm-key
```

- **Pre-check (agent):** the file exists, is non-empty, and is mode 600:
  ```bash
  test -s ~/.agentteams-llm-key
  stat -c '%a' ~/.agentteams-llm-key    # expect 600 (macOS: stat -f '%Lp')
  ```
- **Verify:** both checks pass. The agent never reads or prints the
  content.
- **Failure recovery:** file missing → ask the human once more to
  complete the step (the command above is the complete step); wrong
  mode → `chmod 600`. If the human prefers a different path, record the
  path in the plan instead.

### Step 2 — Install (agent-executable, non-interactive)

- **Pre-check:** Step 1 verified; every value in the plan is filled
  (non-interactive mode fails immediately on a missing required value
  and names it — a failure at this point means the plan is incomplete).
- **Action:** run with every value substituted from the plan (the key
  is read from the file; it is never typed into the command line or
  shown in logs):
  ```bash
  AGENTTEAMS_NON_INTERACTIVE=1 \
  AGENTTEAMS_VERSION=<plan> \
  AGENTTEAMS_LLM_PROVIDER=<plan> \
  AGENTTEAMS_OPENAI_BASE_URL=<plan — empty for qwen> \
  AGENTTEAMS_LLM_API_KEY="$(cat ~/.agentteams-llm-key)" \
  AGENTTEAMS_DEFAULT_MODEL=<plan> \
  AGENTTEAMS_EMBEDDING_MODEL=<plan — empty disables> \
  AGENTTEAMS_MANAGER_RUNTIME=<plan> \
  AGENTTEAMS_DEFAULT_WORKER_RUNTIME=<plan> \
  AGENTTEAMS_LOCAL_ONLY=<plan> \
  AGENTTEAMS_ADMIN_USER=admin \
  bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/<plan>/install/agentteams-install.sh)
  ```
  For a custom (non-built-in) model, also set
  `AGENTTEAMS_MODEL_CONTEXT_WINDOW`, `AGENTTEAMS_MODEL_MAX_TOKENS`,
  `AGENTTEAMS_MODEL_REASONING`, and `AGENTTEAMS_MODEL_VISION` from the
  provider's real specification.
- **Verify:** the installer exits 0 and prints the access URLs and (if
  generated) the administrator password. HUMAN: copy the password from
  the terminal into a password manager right now. The full log is at
  `${HOME}/agentteams-install.log`.
- **Failure recovery:**
  - the installer names a missing/invalid variable → fix that value in
    the plan and re-run (this is a plan defect, not a host problem).
  - `curl`/download failure → check host egress/proxy; alternative:
    clone the repository at the pinned tag and run
    `bash install/agentteams-install.sh` locally.
  - model connectivity check fails → the installer names the failing
    check; in order: re-check the key file (human re-places it), the
    Base URL (ends in `/v1` for `openai-compat`), the model ID, the
    account balance/quota, and the network path to the provider.
  - port conflict → re-run with a free `AGENTTEAMS_PORT_*` from the
    plan.
  - out-of-disk / out-of-memory mid-install → HUMAN; after cleanup,
    re-run (the installer's Step 3 offers upgrade/clean-reinstall
    handling of a partial state).

### Step 3 — Post-install verification (agent-executable)

- **Pre-check:** Step 2 verified.
- **Action:** run the four checks from the
  [Local Deployment Guide](local.md#post-install-verification)
  (substituting the plan's actual ports):
  ```bash
  docker ps --filter name=agentteams-controller
  docker ps --filter name=agentteams-manager
  docker exec agentteams-controller agt get managers
  docker exec agentteams-controller agt get workers
  curl -fsS http://127.0.0.1:18088/ >/dev/null
  curl -fsS http://127.0.0.1:18001/ >/dev/null
  docker exec agentteams-controller curl -fsS http://127.0.0.1:9000/minio/health/live
  ```
- **Verify:** `agentteams-controller` and `agentteams-manager` are
  `Up`; Element Web and Higress Console answer; MinIO reports live.
  `agt get managers` lists the Manager; the worker list may be empty
  before Step 4.
- **Failure recovery:**
  - a container is not `Up` → `docker logs --tail 200 <container>`;
    match the error against the
    [Troubleshooting section](local.md#troubleshooting) (containers do
    not start / remains in provisioning).
  - a curl check fails → confirm the port in the plan; if the port was
    changed, re-run the check with the new port before declaring a
    failure.
  - Manager stays in provisioning → `docker exec agentteams-controller
    agt get managers -o json` plus `docker logs --tail 300
    agentteams-controller`; then the [FAQ](../troubleshooting/faq.md).

### Step 4 — First Worker (agent-executable)

- **Pre-check:** Step 3 verified.
- **Action:** create the first Worker following the
  [Quickstart](../../quickstart.md) (via Element Web or the `agt` CLI
  inside the controller).
- **Verify:** `docker exec agentteams-controller agt get workers` shows
  the Worker as `Running` (or briefly in a provisioning state), and the
  Worker can answer in its team room.
- **Failure recovery:** provisioning stuck → the same log commands as
  Step 3 plus the Worker container's logs; model errors → re-run the
  Step 2 connectivity checklist.

### Step 5 — Handoff (HUMAN)

- Move the generated administrator password into a password manager.
- Keep `~/.agentteams-llm-key` (mode 600) for upgrades, or delete it if
  the key must not persist; the installation config
  `${HOME}/agentteams-manager.env` contains sensitive values — keep its
  permissions tight, never commit it to Git or attach it to an issue.
- Record the outcome in the drill record below.

## Drill record

Honest record of what this playbook has been exercised against:

| Check | Status | Evidence |
|---|---|---|
| Every env variable referenced exists in `install/agentteams-install.sh` | verified (static) | the variable set matches the [Local Deployment Guide](local.md#common-environment-variables) table, which tracks the installer script |
| `uninstall` subcommand, port table, verification commands | verified (static) | match the Local Deployment Guide |
| Empty-environment → working live drill on a Docker host | **pending** | the authoring environment has no container runtime; the drill must be run on a real host, and the results (duration, every failure hit and its recovery path) recorded here |

When the live drill is completed, append: date, host, version, which
steps failed and which recovery path was taken, and any playbook text
that had to be corrected.
