---
name: github-operations
description: 管理 GitHub Pull Request 和 Issue。包括创建/更新/合并 PR、添加评论、管理 Issue 等。注意：文件读写、分支操作、代码提交等请使用 git-delegation 技能。
assign_when: Worker 需要管理 Pull Request（创建、审查、合并、评论）或 Issue（创建、更新、评论、标签）
---

# GitHub PR & Issue Management

## Overview

This skill allows you to manage **Pull Requests and Issues** via the centralized MCP Server. For git operations (clone, pull, push, commit, branch), use the **git-delegation** skill instead.

## Environment Variables

No environment variables needed — `mcporter` reads its config from the default path `./config/mcporter.json` automatically.

| Task | Use This Skill | Use git-delegation |
|------|---------------|-------------------|
| Create/update/close PR | ✅ | |
| Review/merge PR | ✅ | |
| Comment on PR/Issue | ✅ | |
| Create/update/close Issue | ✅ | |
| Clone repository | | ✅ |
| Pull/fetch changes | | ✅ |
| Read/write files | | ✅ |
| Create branches | | ✅ |
| Commit/push code | | ✅ |

---

## How to Call GitHub Tools

Use `mcporter` CLI to call MCP Server tools:

```bash
# Method 1: key=value format (recommended for simple args)
mcporter \
  call mcp-github.<TOOL_NAME> key1=value1 key2=value2

# Method 2: JSON format with --args flag (for complex objects)
mcporter \
  call mcp-github.<TOOL_NAME> --args '{"key1":"value1","key2":"value2"}'
```

**IMPORTANT**:
- Always place `--config` BEFORE the `call` subcommand
- For JSON arguments, use the `--args` flag (not bare JSON string)
- Use `mcp-github.<tool_name>` selector format

### Output

mcporter returns JSON output. Parse with `jq` for clean results.

---

## Pull Request Operations

### create_pull_request

Create a new Pull Request:

```bash
mcporter \
  call mcp-github.create_pull_request --args '{
    "owner": "higress-group",
    "repo": "agentteams",
    "title": "Add new feature",
    "body": "## Summary\n- Change 1\n- Change 2\n\n## Test plan\n- [ ] Test A\n- [ ] Test B",
    "head": "feature-branch",
    "base": "main"
  }'
```

### list_pull_requests

List PRs in a repository:

```bash
mcporter \
  call mcp-github.list_pull_requests owner=higress-group repo=agentteams state=open
```

### get_pull_request

Get details of a specific PR:

```bash
mcporter \
  call mcp-github.get_pull_request owner=higress-group repo=agentteams pull_number=1
```

### update_pull_request

Update PR title, body, or state:

```bash
mcporter \
  call mcp-github.update_pull_request --args '{
    "owner": "higress-group",
    "repo": "agentteams",
    "pull_number": 1,
    "title": "Updated title",
    "body": "Updated description"
  }'
```

### merge_pull_request

Merge a PR:

```bash
mcporter \
  call mcp-github.merge_pull_request owner=higress-group repo=agentteams pull_number=1 merge_method=squash
```

### get_pull_request_files

List files changed in a PR:

```bash
mcporter \
  call mcp-github.get_pull_request_files owner=higress-group repo=agentteams pull_number=1
```

### get_pull_request_status

Get CI status of a PR:

```bash
mcporter \
  call mcp-github.get_pull_request_status owner=higress-group repo=agentteams pull_number=1
```

### request_reviewers

Request reviewers for a PR:

```bash
mcporter \
  call mcp-github.request_reviewers --args '{
    "owner": "higress-group",
    "repo": "agentteams",
    "pull_number": 1,
    "reviewers": ["reviewer-username"]
  }'
```

---

## PR Comments & Reviews

### add_issue_comment (general PR comments)

Add a general comment to a PR (PRs are also issues):

```bash
mcporter \
  call mcp-github.add_issue_comment owner=higress-group repo=agentteams issue_number=1 body="LGTM! Great work."
```

### list_issue_comments

List comments on a PR:

```bash
mcporter \
  call mcp-github.list_issue_comments owner=higress-group repo=agentteams issue_number=1
```

### create_pull_request_review_comment

Comment on a specific code line in a PR:

```bash
# First get the latest commit SHA from the PR
PR_DATA=$(mcporter \
  call mcp-github.get_pull_request owner=higress-group repo=agentteams pull_number=1)
COMMIT_SHA=$(echo "$PR_DATA" | jq -r '.head.sha')

# Create a review comment
mcporter \
  call mcp-github.create_pull_request_review_comment --args '{
    "owner": "higress-group",
    "repo": "agentteams",
    "pull_number": 1,
    "body": "Consider using a helper function here.",
    "commit_id": "'"$COMMIT_SHA"'",
    "path": "src/utils/helpers.ts",
    "line": 42,
    "side": "RIGHT"
  }'
```

### get_pull_request_comments

List all review comments on a PR:

```bash
mcporter \
  call mcp-github.get_pull_request_comments owner=higress-group repo=agentteams pull_number=1
```

### get_pull_request_reviews

List reviews on a PR:

```bash
mcporter \
  call mcp-github.get_pull_request_reviews owner=higress-group repo=agentteams pull_number=1
```

---

## Issue Operations

### create_issue

Create a new issue:

```bash
mcporter \
  call mcp-github.create_issue owner=higress-group repo=agentteams title="Bug report" body="Description..."
```

### list_issues

List issues in a repository:

```bash
mcporter \
  call mcp-github.list_issues owner=higress-group repo=agentteams state=open
```

### get_issue

Get details of a specific issue:

```bash
mcporter \
  call mcp-github.get_issue owner=higress-group repo=agentteams issue_number=1
```

### update_issue

Update issue title, body, state, or labels:

```bash
mcporter \
  call mcp-github.update_issue --args '{
    "owner": "higress-group",
    "repo": "agentteams",
    "issue_number": 1,
    "title": "Updated title",
    "state": "closed"
  }'
```

### add_issue_comment

Add a comment to an issue:

```bash
mcporter \
  call mcp-github.add_issue_comment owner=higress-group repo=agentteams issue_number=1 body="Comment text"
```

### list_issue_comments

List comments on an issue:

```bash
mcporter \
  call mcp-github.list_issue_comments owner=higress-group repo=agentteams issue_number=1
```

---

## Labels

### list_labels

List labels in a repository:

```bash
mcporter \
  call mcp-github.list_labels owner=higress-group repo=agentteams
```

### get_label

Get details of a label:

```bash
mcporter \
  call mcp-github.get_label owner=higress-group repo=agentteams name=bug
```

---

## Search Operations

### search_issues

Search issues and PRs:

```bash
mcporter \
  call mcp-github.search_issues q="is:issue is:open repo:agentscope-ai/AgentTeams"
```

### search_code

Search code in repositories:

```bash
mcporter \
  call mcp-github.search_code q="function handleAuth repo:agentscope-ai/AgentTeams"
```

### search_repositories

Search repositories:

```bash
mcporter \
  call mcp-github.search_repositories query="agentteams language:go"
```

### search_users

Search users:

```bash
mcporter \
  call mcp-github.search_users q="john location:shanghai"
```

---

## Utility Operations

### get_me

Get current authenticated user:

```bash
mcporter \
  call mcp-github.get_me
```

### list_notifications

List notifications:

```bash
mcporter \
  call mcp-github.list_notifications all=false
```

### list_teams

List teams in an organization:

```bash
mcporter \
  call mcp-github.list_teams
```

### list_team_members

List members of a team:

```bash
mcporter \
  call mcp-github.list_team_members org=higress-group team_slug=core-team
```

---

## Known Issues

- `create_pull_request_review`: Has a template processing bug. For adding PR review comments, use:
  - **General PR comment**: Use `add_issue_comment`
  - **Comment on specific code line**: Use `create_pull_request_review_comment`

---

## Important Notes

- **Transport**: The MCP Server uses HTTP transport (configured in mcporter-servers.json)
- **Auth**: HTTP endpoint requires Authorization header with Bearer token (auto-configured)
- **Rate limits**: GitHub API rate limits apply. If you get 403 responses, wait and retry
- **Permissions**: Your MCP access is controlled by the Manager. If you get 403 from the MCP Server, the Manager may need to re-authorize your access

---

## Typical Workflow

1. **Clone & modify** → Use `git-delegation` skill
2. **Push changes** → Use `git-delegation` skill
3. **Create PR** → Use `create_pull_request` (this skill)
4. **Review PR** → Use `add_issue_comment`, `create_pull_request_review_comment` (this skill)
5. **Merge PR** → Use `merge_pull_request` (this skill)

