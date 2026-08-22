---
name: coding-cli
description: 将编码工作委托给 Manager 持有的 AI CLI 工具（Claude Code / Gemini CLI / qodercli）执行
assign_when: Worker 需要完成编码任务（写代码、修改代码、重构、修 bug 等），且 Manager 已启用 Coding CLI 委托模式（spec.md 中包含"## Coding CLI Mode"章节）
---

# Coding CLI Delegation

When the Manager assigns you a task with a "## Coding CLI Mode" section in `spec.md`, you do **not** write the code yourself. Instead, you:

1. Understand the task deeply
2. Prepare the workspace
3. Generate a precise coding prompt
4. Delegate execution to the Manager's CLI tool
5. Review the result

---

## When to Use This Skill

Check `spec.md` for the task. If it contains `## Coding CLI Mode`, use this skill for all code changes.

---

## Step-by-Step Workflow

### 1. Prepare the Workspace

Set up the workspace directory under the shared filesystem:

```bash
workspace="/root/hiclaw-fs/shared/tasks/{task-id}/workspace"
mkdir -p "$workspace"

# Clone a repo (example)
git clone <repo-url> "$workspace"

# Or copy existing code
cp -r /path/to/source "$workspace/"
```

**Constraint**: The workspace path **must** be under `/root/hiclaw-fs/`. The Manager accesses the same path via MinIO mirror.

### 2. Push Workspace to MinIO

Before sending the coding-request, push all workspace files so the Manager can access them:

```bash
mc mirror "/root/hiclaw-fs/shared/tasks/{task-id}/workspace/" \
  ${HICLAW_STORAGE_PREFIX}/shared/tasks/{task-id}/workspace/
```

### 2b. Check for Processing Marker

Before modifying the workspace or sending a coding-request, check if the task directory is being processed:

```bash
# Sync latest state from MinIO
mc mirror "${HICLAW_STORAGE_PREFIX}/shared/tasks/{task-id}/" \
  "/root/hiclaw-fs/shared/tasks/{task-id}/"

# Check for processing marker
if [ -f "/root/hiclaw-fs/shared/tasks/{task-id}/.processing" ]; then
    echo "Task directory is being processed. Wait for manager to complete."
    # Do NOT send coding-request yet; wait and retry
fi
```

If a `.processing` marker exists, wait for the Manager to complete their operation before sending your request.

### 3. Generate a High-Quality Prompt

A good prompt includes:

- **Target files**: exact paths and relevant line numbers
- **Specific changes**: describe *what* to change, not just *why*
- **Context**: existing code structure, dependencies, interfaces
- **Acceptance criteria**: how to verify the change is correct
- **Constraints**: languages, frameworks, style conventions, things NOT to change

**Example of a good prompt:**
```
In the file `src/server/handlers/auth.go`, implement the `RefreshToken` function (currently a stub at line 142).

Requirements:
- Validate the incoming refresh_token from the request body (field name: "refresh_token")
- Look up the token in the database using `db.FindRefreshToken(ctx, token)` (already imported)
- If valid, generate a new access token with `auth.GenerateAccessToken(userID)` and return it as JSON: {"access_token": "<token>", "expires_in": 3600}
- If invalid or expired, return HTTP 401 with body: {"error": "invalid_refresh_token"}
- Follow the existing error handling pattern used in `LoginHandler` (line 89)

Do not change any other files.
```

**Signs of a weak prompt** (avoid):
- "Fix the auth system" (too vague)
- "Improve performance" (no specific target)
- "Add tests" (no specification of what to test or where)

### 4. Send `coding-request:` to Manager

Send in your Worker Room (or Project Room, wherever the task was assigned):

```
@manager:DOMAIN task-{task-id} coding-request:
workspace: /root/hiclaw-fs/shared/tasks/{task-id}/workspace
---PROMPT---
{your detailed coding prompt here}
---END---
```

Note: `workspace` can be any subdirectory under `/root/hiclaw-fs/`, e.g. a cloned git repo:
```
workspace: /root/hiclaw-fs/shared/tasks/{task-id}/workspace/my-repo
```

### 5. Wait for Manager's Response

The Manager will run the CLI tool and respond with either:

**Success** — `coding-result:`
```
@{your-name}:DOMAIN task-{task-id} coding-result:
CLI 工具已完成编码。请同步工作目录并 review 变更...
```

**Failure** — `coding-failed:`
```
@{your-name}:DOMAIN task-{task-id} coding-failed:
CLI 工具执行失败...你生成的提示词已保存于：/root/hiclaw-fs/shared/tasks/{task-id}/coding-prompts/
```

### 6a. On `coding-result:`

```bash
# Sync changes from MinIO
hiclaw-sync

# Review what changed
cd /root/hiclaw-fs/shared/tasks/{task-id}/workspace
git diff  # if it's a git repo
# or: check coding-cli-logs/ for CLI output
```

Review the changes:
- Verify they match the task requirements
- Check for obvious errors or unintended modifications
- Run tests if applicable

Report to Manager:
```
@manager:DOMAIN task-{task-id} completed:
Changes reviewed and verified. {Brief summary of what was implemented.}
```

### 6b. On `coding-failed:`

Implement the coding task yourself using your normal approach. When done, report:
```
@manager:DOMAIN task-{task-id} completed:
Implemented manually (CLI delegation failed). {Brief summary.}
```

---

## Multiple Rounds

If one CLI delegation doesn't fully complete the task (e.g., the first run fixed the bug but tests still fail), you can send another `coding-request:` with a follow-up prompt. Sync the workspace first to get the latest state before generating the next prompt.

---

## Tips for Better Prompts

- **Be surgical**: "change line 47 from X to Y" beats "fix the login function"
- **Provide imports**: if the change requires new imports, specify them
- **Show the pattern**: reference existing code in the same file as the style guide
- **Limit scope**: tell the CLI "only modify file X, do not change tests or other files"
- **Verify command**: include how to check correctness, e.g. "run `go test ./...` to verify"

