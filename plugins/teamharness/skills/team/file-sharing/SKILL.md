---
name: teamharness-file-sharing
description: "Use for TeamHarness shared workspace paths, explicit filesync operations, and shared project/task artifact boundaries."
---

# File Sharing

Use shared workspace paths in task specs and team messages.

Project files belong under `shared/projects/{project-id}/`. Task specs,
deliverables, and results belong under `shared/tasks/{task-id}/`.

Do not expose object storage internals in human-facing messages. Use TeamHarness
filesync tools for explicit shared file operations.

## Shared Paths

Use these paths in task specs and team messages:

```text
shared/projects/{project-id}/meta.json
shared/projects/{project-id}/plan.md
shared/projects/{project-id}/result.md
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/spec.md
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/result.md
```

The Leader owns project files and task specs. Workers own task workspaces,
deliverables, and submitted results.

## Filesync

Use `filesync` when you need an explicit shared file operation.

List a concrete shared directory:

```json
{
  "action": "list",
  "path": "shared/projects/demo-project-001"
}
```

Pull before reading remote shared state:

```json
{
  "action": "pull",
  "path": "shared/tasks/task-001"
}
```

Push after writing project-level files:

```json
{
  "action": "push",
  "path": "shared/projects/demo-project-001"
}
```

Do not ask humans or Workers to inspect storage bucket names, access keys, or
provider-specific prefixes. Use `shared/...` paths in all visible coordination.
