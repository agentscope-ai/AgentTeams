---
name: teamharness-file-sharing
description: "Use for TeamHarness shared workspace paths, filesync, artifact publication, and Matrix room file boundaries, including artifact publish_file and task/project deliverables. Do not use for message routing or project state changes."
---

# File Sharing

Use shared workspace paths in task specs and team messages.

Project files belong under `shared/projects/{project-id}/`. Task specs,
deliverables, and results belong under `shared/tasks/{task-id}/`.

Do not expose object storage internals in human-facing messages. Use TeamHarness
filesync tools for explicit shared file operations.

## Shared Files vs Room Files

`shared/...` paths are durable collaboration state. They let Leaders, Workers,
and recovery flows find the same project/task files.

Matrix room files are UI-visible file events. The room Files panel is populated
from Matrix `m.file` events, not from text that merely mentions a `shared/...`
path.

DingTalk, Feishu, WeChat, and other non-Matrix requester channels do not receive
TeamHarness `artifact publish_file` room files. For those requester reports,
mention the `shared/...` artifact paths and say they are available from the
shared workspace or platform object-storage view. Do not tell the requester to
open a chat file card unless the file was actually published to a Matrix room.

For important task outputs, write the files under `shared/tasks/{task-id}/` and
list them in `taskflow submit_task` `deliverables`. Worker task deliverables
must not include `shared/projects/...` paths. For important project reports,
the Leader writes `shared/projects/{project-id}/result.md` from accepted
project state before publishing it.

Use `artifact publish_file` when an explicit extra workspace file must become a
Matrix room file outside the task/project submit flow. The `path` can be any
workspace-relative file path, not only `shared/...`, but it must not be absolute
or escape the workspace. Always pass an explicit Matrix room `target` or
`roomId`. When publishing files for a requester report that was just sent with
`message` to a Matrix room, pass the returned Matrix `messageId` as
`parentEventId`.

Do not manually send sensitive files. TeamHarness refuses obvious sensitive
paths and text content by default, and reports the publish status in
`publishedArtifacts`.

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
task deliverables, and task results.

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
