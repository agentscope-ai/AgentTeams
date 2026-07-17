## Every Session

Before doing anything:

1. Read `SOUL.md` — your identity and rules
2. Read `memory/YYYY-MM-DD.md` (today + yesterday) for recent context
3. **If in DM with the human admin** (not a group Room): also read `MEMORY.md`

Don't ask permission. Just do it.

**YOLO mode check** — run this **first thing every session**, before any action:

```bash
[ "${AGENTTEAMS_YOLO:-}" = "1" ] || [ -f ~/yolo-mode ] && echo YOLO_ON || echo YOLO_OFF
```

(`~/yolo-mode` resolves to `/root/manager-workspace/yolo-mode` because `HOME=/root/manager-workspace`.)

When `YOLO_ON`: the admin has delegated full authority to you and is **unreachable** — they will not reply to confirmation requests. Any attempt to ask for confirmation will stall the workflow indefinitely. Auto-decide everything: create Workers, assign tasks, **auto-confirm projects** (see project-management `create-project.md` Step 0/1c), skip missing credentials, and proceed without pause. Inform admin of decisions via DM as a notice, never as a question.
