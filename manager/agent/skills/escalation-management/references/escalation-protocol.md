# Escalation Protocol

## Overview

Structured escalation replaces ad-hoc "notify admin" with tracked, severity-routed, re-escalating issues. Every escalation is recorded in `~/escalations.json` and follows a defined lifecycle.

## Severity Routing

| Severity | Admin notification | Re-escalation interval | Max re-escalations |
|----------|-------------------|----------------------|-------------------|
| CRITICAL | Immediate DM (don't wait for heartbeat) | 1 hour | 3 |
| HIGH | Next heartbeat report (Step 7) | 4 hours | 3 |
| MEDIUM | Daily digest inclusion | 24 hours | 3 |

After max re-escalations, flag as: "needs human intervention, system cannot resolve — escalation {id} has been re-escalated 3 times without acknowledgment".

## When to Escalate

### DO escalate when:

- Worker reports `[BLOCKED:<severity>]` in their task reply
- Task has been stuck for >2× the expected duration with no progress
- Worker explicitly requests human decision or credential
- Security issue detected (credential exposure, unauthorized access)
- Worker stuck in loop (same action repeated without progress)

### Do NOT escalate for:

- Normal workflow questions ("which file should I edit?")
- Transient failures that auto-retry (network timeout, rate limit)
- Tasks progressing normally but slowly
- Questions answerable from existing context or documentation

## Escalation Lifecycle

```
raise → open → acknowledged → resolved
              ↘ (stale) → re-escalated (count++) → ... → max_reached
```

1. **Raise**: Create escalation entry with severity, category, context
2. **Open**: Waiting for admin attention; re-escalation timer running
3. **Acknowledged**: Admin has seen it (stops re-escalation timer)
4. **Resolved**: Issue fixed, escalation closed

## Worker Self-Report Format

Workers are trained (via SOUL.md) to report blockers as:

```
[BLOCKED:<CRITICAL|HIGH|MEDIUM>] <what was tried> — <specific question>
```

When you see this format in a Worker's reply:

1. Parse severity from `[BLOCKED:<severity>]`
2. Parse what_was_tried and question from the text
3. Raise escalation:
   ```bash
   bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh \
     --action raise --task-id {task-id} --severity {severity} \
     --category {infer-from-context} --worker {worker} \
     --summary "{brief description}" --what-tried "{what was tried}" \
     --question "{specific question}"
   ```
4. Route notification per severity (see table above)

## Category Inference

| Worker says | Category |
|-------------|----------|
| "spec is unclear", "multiple interpretations" | `ambiguous_requirement` |
| "error", "exception", "cannot proceed" | `technical` |
| "need API key", "token expired", "access denied" | `needs_credential` |
| "which approach?", "need decision" | `needs_decision` |
| "tried X times", "same error repeatedly" | `stuck_loop` |

## Heartbeat Integration (Step 2c)

During heartbeat, after checking finite tasks (Step 2) and team tasks (Step 2b):

```bash
bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh --action check-stale
```

For each stale item in the output:
- If `stale_reason` is `threshold_exceeded`: re-notify admin with `[RE-ESCALATION] [{severity}] {summary}`
- If `stale_reason` is `max_re_escalations_reached`: flag as critical finding in Step 7 report

Also scan Step 2 Worker replies for `[BLOCKED:...]` patterns and raise escalations for any that don't already have one.

## Admin Notification Template

```
[Escalation] {severity} — {task_id}
Worker: {worker}
Category: {category}
Summary: {summary}
What was tried: {what_was_tried}
Question: {question}
Raised: {created_at}
```

## Resolution

When admin provides the answer or fix:

```bash
bash /opt/hiclaw/agent/skills/escalation-management/scripts/manage-escalations.sh \
  --action resolve --id {esc-id} --resolution "{what was done}"
```

Then relay the resolution to the blocked Worker so they can resume.
