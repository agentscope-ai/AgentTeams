---
name: escalation-management
description: Use when a Worker reports a blocker via [BLOCKED:...] format, when a task is stuck beyond threshold, when a decision needs admin attention, or when checking stale escalations during heartbeat.
---

# Escalation Management

Structured severity-routed escalation for Worker blockers. Replaces ad-hoc "notify admin" with tracked, re-escalating issues.

## Severity Levels

| Level | When to use | Routing |
|-------|-------------|---------|
| `CRITICAL` | Work fully stopped, data loss risk, security issue | Immediate admin DM |
| `HIGH` | Blocked on external input, needs human decision | Next heartbeat report |
| `MEDIUM` | Degraded but workaround exists, non-urgent question | Daily digest inclusion |

## Categories

- `ambiguous_requirement` — spec is unclear, multiple valid interpretations
- `technical` — unexpected error, cannot proceed after trying
- `needs_credential` — missing API key, token, or access
- `needs_decision` — architectural choice requiring human judgment
- `stuck_loop` — agent stuck repeating same action without progress

## Worker Self-Report Format

Workers report blockers using this format (trained via SOUL.md):

```
[BLOCKED:<CRITICAL|HIGH|MEDIUM>] <what was tried> — <specific question>
```

Example: `[BLOCKED:HIGH] Tried refreshing the GitHub token 3 times — Is the PAT expired? Need a new one.`

## Gotchas

- **Don't escalate recoverable errors** — transient failures that auto-retry are not escalations
- **Don't escalate normal workflow questions** — "which file should I edit?" is not a blocker
- **Only auto-create from explicit `[BLOCKED:...]` markers** — free-text "I'm stuck" triggers a follow-up question, not an escalation
- **Always use `manage-escalations.sh`** — never edit `escalations.json` manually
- **Re-escalation is automatic** — unacknowledged escalations re-notify admin on schedule (CRITICAL: 1h, HIGH: 4h, MEDIUM: 24h)
- **Max 3 re-escalations** — after that, flag as "needs human intervention, system cannot resolve"

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read | Key command |
|---|---|---|
| Raise a new escalation | `references/escalation-protocol.md` | `scripts/manage-escalations.sh --action raise` |
| Check stale escalations (heartbeat) | `references/escalation-protocol.md` | `scripts/manage-escalations.sh --action check-stale` |
| Resolve an escalation | this file | `scripts/manage-escalations.sh --action resolve` |
| List open escalations | this file | `scripts/manage-escalations.sh --action list` |

## Quick Commands

```bash
# Raise escalation
bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh \
  --action raise --task-id T --severity HIGH --category technical \
  --worker W --summary "..." --question "..."

# Check stale (for heartbeat)
bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh \
  --action check-stale

# Resolve
bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh \
  --action resolve --id esc-YYYYMMDD-HHMMSS

# List open
bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh \
  --action list
```
