---
name: git-delegation-management
description: Execute git operations on behalf of Workers who don't have git credentials. Use when a Worker sends a git-request: message to clone, push, pull, commit, rebase, or perform any git operation.
---

# Git Delegation Management

This skill enables the Manager to execute **any git operation** on behalf of Workers. Workers cannot access git credentials, so they delegate all git operations to the Manager.

## Prerequisites

The Manager has access to:
- Host's `.gitconfig` via `/host-share/.gitconfig` (symlinked to `/root/.gitconfig`)
- Git credentials (SSH keys, credential helpers) configured on the host

This allows git operations to use the correct author name, email, and authentication.

---

## Handling `git-request:` Messages

When a Worker sends a message containing a **properly formatted** `git-request:` block (with `workspace:` and `operations:` fields), execute the requested operations.

```
task-{task-id} git-request:
workspace: /root/hiclaw-fs/shared/tasks/{task-id}/workspace/{repo-name}
operations:
  - git clone https://github.com/org/repo.git
  - git checkout -b feature-auth
  - git add .
  - git commit -m "feat: add authentication"
  - git push origin feature-auth
---CONTEXT---
{description of what they're trying to accomplish}
---END---
```

**Extract:**
- `task-id`: Task identifier
- `workspace`: Path to work in (for clone: parent directory; for other ops: repo directory)
- `operations`: List of git commands to execute (literally what to run)
- `context`: (Optional) What the Worker is trying to accomplish

---

## Execution Flow

### 1. Sync and Check Processing Marker

```bash
task_id="task-YYYYMMDD-HHMMSS"
workspace="/root/hiclaw-fs/shared/tasks/${task_id}/workspace/{repo-name}"

# Sync from MinIO
mc mirror "${HICLAW_STORAGE_PREFIX}/shared/tasks/${task_id}/" \
  "/root/hiclaw-fs/shared/tasks/${task_id}/"

# Check for processing marker
bash /opt/hiclaw/agent/skills/task-coordination/scripts/check-processing-marker.sh "$task_id"
if [ $? -ne 0 ]; then
    # Respond with git-failed: explaining the conflict
    exit 1
fi

# Create processing marker
bash /opt/hiclaw/agent/skills/task-coordination/scripts/create-processing-marker.sh "$task_id" "manager" 15
```

### 2. Execute Git Commands

Navigate to the workspace and execute the git commands:

```bash
cd "$workspace"

# Execute each git command
git clone https://github.com/org/repo.git
git checkout -b feature-auth
# ... etc

# Log output for debugging
```

You know how to use git. Execute the commands the Worker requests. If something goes wrong (merge conflict, authentication failure, etc.), handle it appropriately.

### 3. Cleanup and Respond

```bash
# Remove processing marker
bash /opt/hiclaw/agent/skills/task-coordination/scripts/remove-processing-marker.sh "$task_id"

# Sync to MinIO
mc mirror "/root/hiclaw-fs/shared/tasks/${task_id}/" \
  "${HICLAW_STORAGE_PREFIX}/shared/tasks/${task_id}/" --overwrite
```

**On success** — send to Worker:
```
@{worker}:DOMAIN task-{task-id} git-result:
Git operations completed successfully.
{Summary of what was done - commits, pushes, branches created, etc.}
Run `hiclaw-sync` to sync.
```

**On failure** — send to Worker:
```
@{worker}:DOMAIN task-{task-id} git-failed:
Git operation failed: {error message}
{Suggestion for how to fix it, if applicable}
```

---

## What Operations Can Be Delegated

**Any git operation**, including but not limited to:

| Category | Commands |
|----------|----------|
| Repository | `git clone`, `git init` |
| Branches | `git branch`, `git checkout`, `git switch` |
| Remote | `git remote`, `git fetch`, `git pull`, `git push` |
| Commits | `git add`, `git commit`, `git reset`, `git revert` |
| History | `git log`, `git show`, `git diff` |
| Rebase | `git rebase`, `git rebase -i` |
| Cherry-pick | `git cherry-pick` |
| Merge | `git merge` |
| Stash | `git stash` |
| Tags | `git tag` |
| Submodules | `git submodule` |
| Config | `git config` (local to repo) |

If git can do it, the Worker can delegate it.

---

## Error Handling

When git operations fail:

1. **Read the error message** and understand what went wrong
2. **Try to fix it** if it's a simple issue (e.g., set upstream, configure user locally)
3. **Report to Worker** if it requires their action (e.g., merge conflicts, rebasing decisions)
4. **Escalate to admin** if it's a credential or permission issue

Common issues:
- Merge conflicts → Ask Worker to resolve locally
- Authentication failure → Check `/host-share/.gitconfig` and credential helper
- Branch divergence → Worker may need to pull/rebase first

---

## Integration with Task Coordination

Always use the `.processing` marker to prevent conflicts when both Worker and Manager might modify the workspace.

---

## Gotchas

- **Wait for the actual `git-request:` block** — if a Worker says "Let me prepare the git-request" or "I'll delegate git operations", that is a preview, not a request. Do NOT execute any git operations until you receive a message with the structured `workspace:` + `operations:` fields.
- **Execute git-request immediately when received** — when a message contains `git-request:` with `workspace:` and `operations:` fields, you MUST execute the git commands yourself and reply with `git-result:`. Workers cannot run git operations — that's why they delegated to you. Never "wait for the Worker to complete" after receiving a git-request; the Worker is waiting for YOUR git-result.
- **Duplicate git-request after you already executed** — if you already completed the git operations for a task and the Worker sends a `git-request:` for the same operations, reply with `git-result:` confirming the operations were already completed. Do not re-execute or re-verify the repo state.
- **Ignore superseded requests** — if a Worker sends multiple `git-request:` messages for the same task before you respond, only execute the LAST one (it is the most complete). Earlier requests were likely incomplete or had wrong paths.
- **Never delete or overwrite the remote repo** — the remote URL (bare repo or GitHub) is shared across phases and Workers. Never `rm -rf` a remote path, never `git init --bare` over an existing repo.
- **git-result is not task completion** — after sending `git-result:`, the Worker still needs to verify the outcome and report completion (e.g., PHASE_DONE). Do NOT treat a successful git-result as the Worker's task completion signal. Wait for the Worker's explicit completion report before advancing to the next phase.
