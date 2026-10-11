---
name: coding-cli-management
description: "Execute AI coding CLI tools (Claude Code / Gemini CLI / qodercli / Qwen Code / OpenCode) on behalf of Workers. Use when a Worker sends a coding-request: message, asking the delegating agent (Manager, team Leader, or an authorized Worker) to run coding operations in their workspace."
---

# Coding CLI Management

This skill enables the delegating agent (Manager, team Leader, or an authorized Worker) to execute AI coding CLI tools (claude/gemini/qodercli/qwen/opencode) on behalf of Workers. Workers generate precise prompts; the delegating agent runs the CLI in the Worker's workspace and returns the result.

## Config File

Path: `~/coding-cli-config.json` — this is the **delegating agent's own** config file (the agent running this skill), not a shared one.

```json
{
  "enabled": true,
  "cli": "claude",
  "confirmed_at": "2026-02-23T10:00:00Z"
}
```

| `enabled` | `cli` | Meaning |
|-----------|-------|---------|
| `false`   | any   | Admin declined; use normal task flow |
| `true`    | `"claude"` / `"gemini"` / `"qodercli"` / `"qwen"` / `"opencode"` | Active — use this CLI |

Optional budget fields (consumed by `run-coding-cli.sh` for `qwen` only — the other runners have no equivalent and ignore them):

| Field | Type / default | Meaning |
|-------|----------------|---------|
| `max_session_turns` | integer, default absent / `0` (flag not passed) | Caps the qwen run at N model turns (`--max-session-turns <N>`); a run that reaches the cap terminates at the budget |
| `sandbox` | boolean, default `false` | Adds `--sandbox` (Docker-backed) to the qwen run. Requires docker at the execution site — leave off where docker is unavailable (as in many agent containers) |

The config path can be overridden with the `CODING_CLI_CONFIG` environment variable (the verify script uses this for its turn-budget case).

**Per-run governance flags (qwen).** `run-coding-cli.sh` accepts optional per-run control flags for the qwen runner (other runners warn and ignore them — they have no equivalent surface):

| Flag | Maps to | Effect |
|---|---|---|
| `--model <name>` | `-m` | per-run model override — the task-level tier of two-tier model control (platform default from the runner's settings, per-task switch here) |
| `--allowed-tools <csv>` | `--allowed-tools` | tool allowlist — a **code-level** permission boundary, independent of prompt discipline |
| `--allowed-mcp <csv>` | `--allowed-mcp-server-names` | MCP server allowlist — scenario isolation (e.g. only this task's toolchain MCP) |
| `--safe-mode` | `--safe-mode` | disable all customizations (hooks, extensions, skills, MCP, QWEN.md) — clean-environment runs |
| `--bare` | `--bare` | minimal mode |

Use the runner's own init frame (`-o json` prints a `tools` array and `permission_mode`) to size an allowlist from the actual capability list rather than from memory. All five were field-verified on qwen 0.25.0 (safe-mode banner, emptied MCP list, per-run model override observed in one run).

**Updates.** Re-run the npm install command to upgrade in place, or use the runner's built-in `qwen update` self-update command (see `qwen update --help` for its channel behavior). Either way, re-run `verify-coding-cli.sh` afterwards (upgrade discipline above).

**Runner notes (Qwen Code / OpenCode).** Headless invocations used by `run-coding-cli.sh`: `qwen --yolo "<prompt>"` and `opencode run --auto --dir <workspace> "<prompt>"`. Config surfaces: `~/.qwen` (`settings.json` `security.auth`, or provider env) and `~/.config/opencode` plus `~/.local/share/opencode/auth.json` (credentials stored by `opencode auth login`) or provider env. Both runners were verified with a headless round trip before inclusion. Qwen Code has **no fixed pin**: deployments choose their own npm channel tag (Section below, stable by default); `0.25.0` is the most recent field-verified reference (2026-10-08, full verify suite), and `0.24.7` the previous one. Re-run `verify-coding-cli.sh` whenever the version actually used changes.

- --json-file <path> — optional structured output: the final result JSON is written to <path> for machine audit; default off, raw stream still tees to the run log (the audit source of truth when the flag is absent).

**Version channels and upgrades (Qwen Code).** qwen-code iterates fast (several hundred npm versions published) and ships three channels as dist-tags: `latest` (stable), `nightly` (dated + commit, published daily), and `preview`. Install or upgrade with npm (Node.js **24 LTS** — floor 22.20+ / 24.5+, OpenSSL 3.5 generation; see the Node-generation note in the install matrix), choosing the channel per the user's preference — stable by default, nightly only when the user explicitly wants newer behavior:

```bash
npm install -g @qwen-code/qwen-code@latest    # stable
npm install -g @qwen-code/qwen-code@nightly   # nightly
```

Re-running the same command upgrades in place; `qwen --version` confirms the result. **Upgrade discipline: after any version change — including a channel switch — re-run `scripts/verify-coding-cli.sh`; a channel switch is a pin promotion and needs the same verification.** The official standalone installer and Homebrew are fine for interactive use; the verify discipline above is defined for the npm channel.

**Install matrix and network environment.** Three official installers; pick by what the execution site has (a Node.js generation requirement applies to the npm channel, below):

| Installer | Requires | Version selection |
|---|---|---|
| npm: `npm install -g @qwen-code/qwen-code@<tag>` | Node.js **24 LTS** (floor 22.20+ / 24.5+, OpenSSL 3.5 generation — see note) | channel tag (`latest` / `nightly` / `preview`) — the route this skill's verify discipline is defined for |
| official standalone script: `curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh \| bash` (Windows: `irm …install-qwen-standalone.ps1 \| iex`) | bash, no Node | the build the script ships; re-run to refresh |
| Homebrew: `brew install qwen-code` | brew | brew's current release |

npm and the release assets cover Linux x64/arm64, macOS x64/arm64, and Windows. Restart the terminal after a standalone install so the PATH entry takes effect.

**Node generation matters on restrictive links.** The npm package runs on Node.js; early Node 22.x (22.0–22.19) bundles OpenSSL 3.0, whose TLS client fingerprint is reset by some middle boxes, while Node 22.20+, 24.5+ and 26 bundle OpenSSL 3.5 and pass. **Use Node 24 LTS as the target** — Node 25 is EOL (2026-06), do not provision it; the 22-series needs 22.20+, the 24-series 24.5+. Field-verified 2026-10 (the cctechstudio WAN line RSTs the 3.0 generation but not 3.5). The filter matches the full ClientHello fingerprint (stack + ALPN + extensions), not the OpenSSL version alone: in the same environment an h2-enabled curl was reset while a Node runtime — and even a plain `openssl s_client` — connected. If in doubt, verify with the runtime that will actually make the connection, and check the Node/OpenSSL generation first (`node -p "process.versions.openssl"`).

**Model configuration.** `qwen` authenticates from `~/.qwen/settings.json` — `security.auth` with `selectedType` (`openai` / `openai-responses` / `anthropic` / `qwen-oauth` / `gemini` / `vertex-ai`) plus `apiKey` and, for OpenAI-compatible endpoints, `baseUrl` — the form headless delegation uses on self-hosted or gateway-backed deployments (or the `OPENAI_API_KEY` environment variable). The interactive `qwen auth` flow was removed in 0.25.0, so file/env configuration is the only path; `--auth-type <type>` selects the scheme per run. Any OpenAI-compatible endpoint is a valid `baseUrl` — including a **transparent local proxy**, which is the standard lever for keeping the model plane working behind a restrictive link. Per-run overrides: `-m <model>` to pick a model and `-o text|json|stream-json` for machine-readable output.

**Restricted-network note.** If installing or reaching the model endpoint fails with connection resets while plain `curl` from the same machine succeeds, suspect middle-box filtering by TLS-stack fingerprint rather than a plain outage; running a current Node (OpenSSL 3.5 generation) or routing the model plane through the local proxy above is the working fix. (Field-verified 2026-10; see #1340 notes 4/5 for the full deployment matrix.)

**OpenCode is a Bun binary, not Node.** Its TLS client is Bun's (a BoringSSL-lineage stack), not Node's OpenSSL, so on the restrictive lines above OpenCode's *own* connection can be reset by the middle box even where a Node/OpenSSL-3.5 runner (Qwen Code) on the same machine passes. Field-verified 2026-10-10 on the cctechstudio WAN line: `qwen` (Node 24) reached the endpoint and completed a task while `opencode 1.18.35` was RST ("socket connection closed unexpectedly") on the identical link, at the same moment — the only variable being the client's TLS stack. For OpenCode the "use a current Node" advice does not apply (it is not Node, and its TLS stack is not user-selectable); the working lever is the **transparent local relay/proxy** in front of the model endpoint — point OpenCode's `baseURL` (or `OPENAI_BASE_URL`) at the same-machine proxy that holds a working TLS stack.

**Unattended semantics — read before enabling.**
- `qwen --yolo` automatically approves **all** tool calls — file edits and shell commands included, with no further prompts.
- `opencode run --auto` auto-approves permissions, but **explicit deny rules are still enforced**.
- **Neither changing the working directory nor adding a timeout establishes a permission boundary.** The workspace and the timeout shape scope and duration only; the runner keeps every capability of the delegating agent's process.

---

## Execution boundary

The CLI process runs **as the delegating agent** (Manager, team Leader, or an authorized Worker). It therefore inherits that agent's filesystem visibility and its environment — including any credentials present as environment variables — and nothing in this skill changes that.

When deciding whether to enable CLI delegation, assess at least:

- **Workspace access scope** — the run is pinned to the task workspace, but a runner in auto-approve mode can reach anything the delegating agent's account can.
- **Other teams' files** — anything on the shared filesystem the delegating agent can read or write (e.g. other tasks' directories).
- **Delegating agent's credentials** — API keys and tokens in the agent's environment are visible to the CLI process.
- **Mounted container sockets** — a mounted `docker.sock` (or equivalent) gives the CLI effective host-control capability.
- **Admin-plane capability** — any orchestration/admin CLI or API the delegating agent can use is equally available to the runner.

Mitigations, in three tiers:

1. **Approval tiers (native).** Qwen Code: `--approval-mode` levels (plan / default / auto-edit / auto / yolo); lower tiers keep prompts on destructive shell and outbound calls. OpenCode: permission rules with `allow` / `ask` / `deny` actions, including command-prefix granularity (e.g. deny `rm ` while allowing `git ` / `npm `); `opencode agent create` can produce a scoped agent whose frontmatter denies everything not allowed.
2. **Native sandbox (opt-in).** Qwen Code ships a Docker-backed sandbox (`--sandbox` / `QWEN_SANDBOX=1`) that runs shell/write/edit tools inside a sandbox image — **it is NOT auto-enabled by `--yolo`**; an un-sandboxed yolo run prints a warning to stderr, which lands in the run log. It requires docker at the execution site; where there is none (as in many agent containers) the flag must stay off. OpenCode has no native sandbox — its boundary is tier 1 plus tier 3.
3. **Isolated execution environment (the baseline for skip-permissions-class flags).** Industry practice (Anthropic's own guidance for `--dangerously-skip-permissions`, and public incident reports from unsandboxed auto runs) is to run such sessions only in isolated environments — a dedicated container/VM/ephemeral runner — with budgets (`--max-session-turns` / wall time) so a stuck run cannot burn the host. In that case the delegating agent's ambient credentials and sockets should not be mounted into the environment at all.

No new approval subsystem is introduced by this skill.

Note: the pre-existing claude/gemini cases already run skip-permissions-class flags; per vendor guidance those are only sound inside an isolated environment, and headless runs should carry a turn cap (claude `--max-turns`).

---

## Persistence and re-install after worker recreation

The CLI binary and its `npm install -g` payload live in the worker container's
**ephemeral layer** (the image layer at `$npm prefix`, e.g. `/usr/local/lib/node_modules`) —
**not** in the persistent agent home. A worker **recreation** (image upgrade, a spec
change, or any controller-driven container rebuild, e.g. an LLM-runtime parameter
change) wipes that layer, so the CLI disappears even though the delegation config
and the auth settings survive:

- `~/coding-cli-config.json` is in the delegating agent's own home → it survives.
- The CLI binary (npm -g) is in the **image layer** → it does **not** survive.
- `~/.qwen/settings.json` (auth) sits in the worker's agent home, but the install
  surface writes it **container-local** (no forced MinIO push) → it MAY be lost on
  a recreation. Do not assume it survives.

Consequence: after any worker recreation, re-run the install endpoint
(`POST /api/v1/workers/{name}/coding-cli/{cli}/install`) before delegating again,
then **probe** the settings — if the auth is gone, re-apply it via the settings
endpoint. `run-coding-cli.sh`
fails fast with an actionable re-install message (exit `125`) if the configured
binary is missing on PATH, so a wiped install is self-diagnosing rather than a
confusing mid-run "command not found".

## Step 1: First-Time Detection (before assigning a coding task)

Run when `~/coding-cli-config.json` does not exist — **or after any worker
recreation** (the detect step re-establishes what is actually on PATH):

```bash
bash /opt/agentteams/agent/skills/coding-cli-management/scripts/detect-available-cli.sh
```

**If no CLIs are available** (`available` array is empty):
```bash
echo '{"enabled":false,"cli":null,"confirmed_at":"'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"}' \
  > ~/coding-cli-config.json
```
Proceed with normal task assignment (Worker codes on their own).

**If CLIs are available**, ask the admin via the primary channel or Matrix DM — **in the language the admin used**:
> I found the following AI coding CLI tools available: [list]. Would you like to enable CLI delegation mode? Workers will generate coding prompts, and I'll use the CLI tool to make the code changes. Note: the CLI runs unattended — `qwen --yolo` auto-approves all tool calls, and `opencode --auto` still enforces explicit deny rules; the workspace and the timeout are not permission boundaries. On a local or shared machine, consider the sandbox tier for Qwen Code (`--sandbox`, Docker-backed, off by default) and turn/wall budgets; on OpenCode there is no native sandbox, so prefer an isolated execution environment or tight deny rules. Reply with the tool name (claude/gemini/qodercli/qwen/opencode) to enable, or 'no' to have workers code on their own.

On admin reply:
- Tool name (`claude` / `gemini` / `qodercli` / `qwen` / `opencode`):
  ```bash
  echo '{"enabled":true,"cli":"<chosen-tool>","confirmed_at":"'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"}' \
    > ~/coding-cli-config.json
  ```
- `"no"` or decline:
  ```bash
  echo '{"enabled":false,"cli":null,"confirmed_at":"'$(date -u +%Y-%m-%dT%H:%M:%SZ)'"}' \
    > ~/coding-cli-config.json
  ```

---

## Step 2: Assigning a Coding Task (CLI mode enabled)

When `coding-cli-config.json` has `enabled: true`:

1. **Ensure the Worker has the `coding-cli` skill.** Query its Worker CR:
   ```bash
   agt get workers <worker> -o json | jq '.skills'
   ```
   If `coding-cli` is missing, distribute it:
   ```bash
   bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh \
     --worker <worker-name> --skill coding-cli
   ```

2. **Add a "Coding CLI Mode" section to spec.md** (see template below).

---

## Step 3: Handling a `coding-request:` Message

When a Worker sends a message containing `coding-request:` (in their Worker Room or a Project Room):

**Parse the message:**
```
task-{task-id} coding-request:
workspace: /root/agentteams-fs/shared/tasks/{task-id}/workspace
---PROMPT---
{prompt content}
---END---
```

**Execute:**

```bash
# 1. Sync workspace from MinIO
task_id="task-YYYYMMDD-HHMMSS"
workspace="/root/agentteams-fs/shared/tasks/${task_id}/workspace"
mc mirror "${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/${task_id}/" "/root/agentteams-fs/shared/tasks/${task_id}/"

# 2. Check for processing marker (task coordination)
bash /opt/agentteams/agent/skills/task-coordination/scripts/check-processing-marker.sh "$task_id"
if [ $? -ne 0 ]; then
    # Another process is working on this task
    echo "Task ${task_id} is being processed by another operation. Retry later."
    exit 1
fi

# 3. Create processing marker (TTL in minutes; must outlive the longest
#    in-flight run or a second actor can enter the workspace mid-run —
#    75 = 60-min run + sync/review slack)
bash /opt/agentteams/agent/skills/task-coordination/scripts/create-processing-marker.sh "$task_id" "manager" 75

# 4. Save prompt to file
timestamp=$(date +%Y%m%d-%H%M%S)
prompt_dir="/root/agentteams-fs/shared/tasks/${task_id}/coding-prompts"
mkdir -p "$prompt_dir"
prompt_file="$prompt_dir/${timestamp}.txt"
cat > "$prompt_file" << 'PROMPT_EOF'
{extracted prompt content}
PROMPT_EOF

# 5. Get configured CLI
cli=$(jq -r '.cli' ~/coding-cli-config.json)

# 6. Run CLI
# --timeout is the wall-clock bound for the run. Delegated coding runs are
# routinely hour-plus (field data), so size for the long tail, not the common
# case. Keep the .processing marker TTL above (step 3) longer than this.
bash /opt/agentteams/agent/skills/coding-cli-management/scripts/run-coding-cli.sh \
  --cli "$cli" \
  --workspace "$workspace" \
  --prompt-file "$prompt_file" \
  --timeout 3600
exit_code=$?

# 7. Remove processing marker
bash /opt/agentteams/agent/skills/task-coordination/scripts/remove-processing-marker.sh "$task_id"

# 8. On success (exit 0): push changes to MinIO
if [ "$exit_code" -eq 0 ]; then
    mc mirror "/root/agentteams-fs/shared/tasks/${task_id}/workspace/" "${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/${task_id}/workspace/" --overwrite
fi
```

**On success** — send to Worker in the same Room:
```
@{worker}:DOMAIN task-{task-id} coding-result:
CLI 工具已完成编码。请同步工作目录并 review 变更：
  agentteams-sync
变更记录：/root/agentteams-fs/shared/tasks/{task-id}/workspace/coding-cli-logs/
```

**On failure** (exit ≠ 0 or timeout) — see Step 4.

---

## Step 4: Handling Failure

**Notify Worker** in the task Room:
```
@{worker}:DOMAIN task-{task-id} coding-failed:
CLI 工具执行失败（exit code: {code}）。请自行完成编码任务。
你生成的提示词已保存于：/root/agentteams-fs/shared/tasks/{task-id}/coding-prompts/
```

**Notify Human Admin** via primary channel (see channel-management skill "Sending Messages to Primary Channel"):
```
Worker {worker-name} 的编码委托任务 {task-id} 中，{cli} 工具执行失败。

错误信息：{last lines from log file}

建议检查：
- ~/.{cli}/ 凭证是否有效（token 是否过期）
- /host-share/.{cli}/ 软链是否正常（ls -la /root/.{cli}）
- {cli} binary 是否在容器内可用（which {cli}）
- qwen 凭证在 ~/.qwen（settings.json 的 security.auth 或模型环境变量）；opencode 凭证在 ~/.config/opencode 或 ~/.local/share/opencode/auth.json（opencode auth login 存储 / provider 配置）
```

**Record in config** (optional, for heartbeat diagnostics):
```bash
jq --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg tid "$task_id" \
   '.last_failure = {task_id: $tid, failed_at: $ts}' \
   ~/coding-cli-config.json > /tmp/cfg.json && \
mv /tmp/cfg.json ~/coding-cli-config.json
```

---

## Spec.md Coding CLI Mode Template

Append to the end of spec.md when CLI mode is enabled:

```markdown
## Coding CLI Mode

本任务涉及代码修改。请使用 **Coding CLI 委托模式** 完成：

1. 克隆/准备代码到工作目录：`/root/agentteams-fs/shared/tasks/{task-id}/workspace/`
2. 推送到 MinIO：`mc mirror /root/agentteams-fs/shared/tasks/{task-id}/workspace/ ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/workspace/`
3. 根据你的理解和 `coding-cli` skill 生成编码提示词，发送给我
4. 等待我执行 CLI 工具并返回结果
5. Sync 拉取变更：`agentteams-sync`
6. Review 变更并报告完成

如收到 `coding-failed:`，请自行完成编码工作。
```
