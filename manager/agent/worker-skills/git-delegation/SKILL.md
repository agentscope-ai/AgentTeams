---
name: git-delegation
description: 将所有 git 操作委托给 Manager 执行。Worker 无法直接访问 git credentials，因此任何需要认证的 git 操作（clone、push、fetch 等）都需要通过此机制委托给 Manager。
assign_when: Worker 需要执行任何 git 操作
---

# Git Delegation

This skill allows you to delegate **any git operation** to the Manager. Since Workers don't have access to git credentials, all git commands that require authentication must be delegated.

## What Can Be Delegated

**Any git command**, including but not limited to:

- `git clone` - Clone repositories
- `git fetch` / `git pull` - Fetch/pull from remote
- `git push` - Push to remote
- `git checkout` / `git switch` - Switch branches
- `git branch` - Manage branches
- `git add` / `git commit` - Stage and commit
- `git merge` - Merge branches
- `git rebase` - Rebase commits (including interactive)
- `git cherry-pick` - Cherry-pick commits
- `git reset` / `git revert` - Undo changes
- `git stash` - Stash changes
- `git tag` - Manage tags
- `git submodule` - Manage submodules
- And any other git command...

**If git can do it, you can delegate it.**

---

## When to Use This Skill vs github-operations

| Task | Use git-delegation | Use github-operations |
|------|-------------------|----------------------|
| Clone repository | ✅ | |
| Pull/fetch changes | ✅ | |
| Read/write files | ✅ | |
| Create/switch branches | ✅ | |
| Commit/push code | ✅ | |
| Rebase/cherry-pick | ✅ | |
| Manage tags | ✅ | |
| Create PR | | ✅ |
| Review/merge PR | | ✅ |
| Comment on PR | | ✅ |
| Create/update Issue | | ✅ |

---

## Message Format

Send a `git-request:` message to the Manager.

**First, get your actual Matrix domain:**
```bash
echo $HICLAW_MATRIX_DOMAIN
# e.g.: matrix-local.hiclaw.io:18080
```

Then send (substitute the real domain — do NOT write `$HICLAW_MATRIX_DOMAIN` literally):

```
@manager:matrix-local.hiclaw.io:18080 task-{task-id} git-request:
workspace: /root/hiclaw-fs/shared/tasks/{task-id}/workspace/{repo-name}
operations:
  - git clone https://github.com/org/repo.git
  - git checkout -b feature-auth
  - git add .
  - git commit -m "feat: add authentication"
  - git push origin feature-auth
---CONTEXT---
{What you're trying to accomplish}
---END---
```

**Fields:**
- `workspace`: Directory to work in (parent dir for clone, repo dir for other ops)
- `operations`: List of git commands to execute (literally what to run)
- `context`: (Optional) What you're trying to accomplish - helps Manager understand intent

---

## Step-by-Step Workflow

### 1. Check for Processing Marker

Before making changes, check if the task directory is being processed:

```bash
# Sync from MinIO
mc mirror "${HICLAW_STORAGE_PREFIX}/shared/tasks/{task-id}/" \
  "/root/hiclaw-fs/shared/tasks/{task-id}/"

# Check for processing marker
if [ -f "/root/hiclaw-fs/shared/tasks/{task-id}/.processing" ]; then
    echo "Task directory is being processed. Wait for manager to complete."
fi
```

### 2. Prepare Your Request

Write out the git commands you want executed:

```
@manager:DOMAIN task-20260225 git-request:
workspace: /root/hiclaw-fs/shared/tasks/task-20260225/workspace
operations:
  - git clone https://github.com/higress-group/hiclaw.git
  - cd hiclaw && git checkout -b feature-xyz
---CONTEXT---
Starting work on feature XYZ
---END---
```

### 3. Wait for Response

**Success** — `git-result:`
```
@alice:DOMAIN task-20260225 git-result:
Git operations completed successfully.
Cloned to: /root/hiclaw-fs/shared/tasks/task-20260225/workspace/hiclaw
Created branch: feature-xyz
Run `hiclaw-sync` to sync.
```

**Failure** — `git-failed:`
```
@alice:DOMAIN task-20260225 git-failed:
Git operation failed: {error message}
{Suggestion for how to fix}
```

### 4. Sync and Continue

After receiving `git-result:`:

```bash
# Sync from MinIO
hiclaw-sync

# Now you can work locally
cd /root/hiclaw-fs/shared/tasks/task-20260225/workspace/hiclaw

# Read files, modify files, etc.
cat src/main.py
# ... make changes ...

# When ready to commit, sync to MinIO first
mc mirror "/root/hiclaw-fs/shared/tasks/task-20260225/" \
  "${HICLAW_STORAGE_PREFIX}/shared/tasks/task-20260225/" --overwrite
```

### 5. Commit and Push

After making local changes:

```
@manager:DOMAIN task-20260225 git-request:
workspace: /root/hiclaw-fs/shared/tasks/task-20260225/workspace/hiclaw
operations:
  - git add .
  - git commit -m "feat: implement feature XYZ"
  - git push origin feature-xyz
---CONTEXT---
Completed implementation of feature XYZ
---END---
```

---

## Local vs Delegated Operations

You can run **any git command that doesn't need authentication** locally:

```bash
# These work locally (no auth needed):
git status
git log
git diff
git branch
git diff --staged

# These require delegation (need auth):
git clone https://github.com/...
git push
git fetch
git pull
```

---

## Advanced Operations

### Interactive Rebase

```
@manager:DOMAIN task-20260225 git-request:
workspace: /root/hiclaw-fs/shared/tasks/task-20260225/workspace/hiclaw
operations:
  - git rebase -i HEAD~3
---CONTEXT---
Squashing the last 3 commits into one
---END---
```

### Cherry-pick

```
@manager:DOMAIN task-20260225 git-request:
workspace: /root/hiclaw-fs/shared/tasks/task-20260225/workspace/hiclaw
operations:
  - git cherry-pick abc123def
---CONTEXT---
Cherry-picking fix from main branch
---END---
```

### Merge with Strategy

```
@manager:DOMAIN task-20260225 git-request:
workspace: /root/hiclaw-fs/shared/tasks/task-20260225/workspace/hiclaw
operations:
  - git merge feature-xyz --no-ff -m "Merge feature XYZ"
---CONTEXT---
Merging feature branch with merge commit
---END---
```

---

## Critical Rules

### Plan First, Send Once

**Before sending a `git-request:`, plan ALL operations in your head first.** Think through the entire workflow — clone, branch, file creation, commit, push — and include everything in a SINGLE request. Do NOT send partial requests and iterate.

Bad (multiple requests, trial-and-error):
```
# Request 1: just clone
git-request: clone repo...
# Wait... then Request 2: create branch
git-request: checkout branch...
# Wait... then Request 3: add files and push
git-request: add, commit, push...
```

Good (one complete request):
```
git-request:
workspace: /root/hiclaw-fs/shared/tasks/{task-id}/workspace
operations:
  - git clone /path/to/repo.git
  - cd repo && git config user.name "alice"
  - cd repo && git config user.email "alice@hiclaw.local"
  - cd repo && git checkout -b feature/my-branch
  - mkdir -p repo/docs
  - 'cat > repo/docs/file.md << "EOF"
file content here
EOF'
  - cd repo && git add docs/file.md
  - cd repo && git commit -m "feat: add file"
  - cd repo && git push origin feature/my-branch
---CONTEXT---
Complete workflow: clone, branch, create file, commit, push
---END---
```

### Wait Silently After Sending

After sending a `git-request:`, the Manager needs time to execute the operations. **Do NOT**:
- Send follow-up messages asking "is it done?"
- Send duplicate or revised `git-request:` messages
- Try alternative approaches while waiting
- Spawn subagents to work around the wait

Simply wait for the `git-result:` or `git-failed:` response. The Manager will reply when done.

### Workspace Path Convention

Always use this path pattern:
```
/root/hiclaw-fs/shared/tasks/{task-id}/workspace/{repo-name}
```

- For `git clone`: set workspace to the **parent** directory (without repo name)
- For all other operations: set workspace to the **repo** directory (with repo name)
- **Never** use `/tmp` or other arbitrary paths — the Manager syncs via MinIO and needs the workspace under the shared tasks directory

### Include Non-Git Commands When Needed

The operations list can include shell commands like `mkdir -p`, `cat > file`, `cd dir &&` prefixes. This lets you do everything in one round-trip:

```
operations:
  - git clone https://github.com/org/repo.git
  - cd repo && git checkout -b feature/xyz
  - mkdir -p repo/src
  - 'cat > repo/src/main.py << "EOF"
print("hello")
EOF'
  - cd repo && git add .
  - cd repo && git commit -m "feat: add main"
  - cd repo && git push origin feature/xyz
```

---

## Tips

1. **One request, all commands**: Include clone, branch, file creation, commit, and push in a single request
2. **Provide context**: Help the Manager understand what you're trying to accomplish
3. **Handle errors**: If `git-failed:` is returned, read the error and send ONE corrected request
4. **Sync before and after**: Always sync to/from MinIO when exchanging workspace with Manager
5. **Use github-operations for PRs**: After pushing, use the MCP tools to create/manage PRs

