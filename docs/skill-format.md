# Skill Format and Contribution Guide

HiClaw skills are agent-facing Markdown packages. A skill must be self-contained enough for an agent to decide when to use it, understand available commands, and follow safe operating steps without reading unrelated documentation.

## Skill Locations

- `manager/agent/skills/<skill-name>/` - built-in Manager skills shared by OpenClaw and QwenPaw Managers.
- `manager/agent/worker-skills/<skill-name>/` - on-demand Worker skills that the Manager can push to selected Workers.
- `manager/agent/<runtime>-worker-agent/skills/<skill-name>/` - runtime-specific Worker built-ins.
- `manager/agent/team-leader-agent/skills/<skill-name>/` - Team Leader skills.

Custom Manager skills in a running installation live under `agents/manager/skills/<skill-name>/` and are discovered automatically.

## Directory Layout

Use one directory per skill:

```text
<skill-name>/
|-- SKILL.md
|-- scripts/       # optional executable helpers
`-- references/    # optional longer docs or API references
```

Keep `SKILL.md` focused on routing and workflow. Put long API references, templates, or examples under `references/`, and put reusable commands under `scripts/`.

## `SKILL.md` Frontmatter

Start every `SKILL.md` with YAML frontmatter:

```yaml
---
name: github-operations
description: Work with GitHub issues and pull requests from a Worker.
assign_when: Give this skill to Workers that need to inspect repositories, triage issues, or submit pull requests.
---
```

Required fields:

- `name` must match the directory name and use lowercase kebab-case.
- `description` should be one short sentence that explains what the skill enables.

Worker skills under `manager/agent/worker-skills/` also require `assign_when`. Describe the Worker role or responsibility that should receive the skill. Do not describe implementation details.

## Writing Rules

- Write agent-facing content in second person: "Use this script..." instead of "The Manager can use this script...".
- Include concrete commands with full paths when path context matters, such as `/opt/hiclaw/agent/skills/.../scripts/<name>.sh`.
- State prerequisites and side effects before destructive or external operations.
- Prefer scripts for repeatable logic instead of long command sequences in Markdown.
- Keep examples minimal and update them when command flags change.

## Adding a Skill

1. Create `manager/agent/skills/<skill-name>/` for a Manager skill, or `manager/agent/worker-skills/<skill-name>/` for a distributable Worker skill.
2. Add `SKILL.md` with frontmatter, usage rules, examples, and links to any references.
3. Add scripts under `scripts/` when the workflow needs repeatable shell logic.
4. For Worker skills, document `assign_when` so the Manager can choose the right Workers.
5. Test scripts directly with representative arguments before opening a PR.

## Review Checklist

- `name` matches the directory name.
- Frontmatter is valid YAML.
- Commands use stable paths and quote user-provided values.
- Agent-facing text uses second person.
- Built-image changes under `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, or `hiclaw-controller/` are recorded in `changelog/current.md`.
