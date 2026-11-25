---
name: file-sync
description: Refresh your private Worker configuration and workspace backup. For shared deliverables, use teamharness-file-sharing and its filesync tool instead.
---

# File Sync

When your coordinator or another Worker notifies you that files have been updated in centralized storage (e.g., config changes, task briefs, shared data, collaboration artifacts), run:

```bash
agentteams-sync
```

This pulls the latest files from centralized storage to your local workspace. OpenClaw automatically detects config changes and hot-reloads within ~300ms.

**When to use**: any time you are told that new files are available, configs have changed, or another agent has written something you need to read.

This synchronizes your private Worker workspace. A successful sync does not
publish a `shared/...` deliverable to the shared collaboration namespace.

For an ordinary file request without a registered Task assignment, use
`teamharness-file-sharing`: write the requested file, call `filesync` with
`action="push"` and its exact workspace-relative path, then verify the same
path with `action="stat"`. If the native tool is unavailable, use the bundled
stdio MCP entrypoint, for example:

```bash
mcporter call --stdio python3 --stdio-arg /opt/agentteams/plugins/teamharness/mcp/server.py filesync --args '{"action":"push","path":"shared/example/README.md"}' --output json
mcporter call --stdio python3 --stdio-arg /opt/agentteams/plugins/teamharness/mcp/server.py filesync --args '{"action":"stat","path":"shared/example/README.md"}' --output json
```

Use the path from the request. Do not substitute `agentteams-sync` or edit this
Skill to make a private backup look like shared publication. For a verified
`task/v1` assignment, submit files through its prepared Task tool instead.
Continue the requested work after a configuration refresh; do not send a
separate sync acknowledgement unless requested.

