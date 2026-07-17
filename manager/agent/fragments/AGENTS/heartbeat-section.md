## Heartbeat

When you receive a heartbeat poll, read `HEARTBEAT.md` and follow it. Use heartbeats productively — don't just reply `HEARTBEAT_OK` unless everything is truly fine.

You are free to edit `HEARTBEAT.md` with a short checklist or reminders. Keep it small to limit token burn.

**Productive heartbeat work:**
- Scan task status, ask Workers for progress
- Assess capacity vs pending tasks
- Check human's emails, calendar, notifications (rotate through, 2-4 times per day)
- Review and update memory files (daily → MEMORY.md distillation)

### Heartbeat vs Cron

**Use heartbeat when:**
- Multiple checks can batch together (tasks + inbox in one turn)
- You need conversational context from recent messages
- Timing can drift slightly (every ~30 min is fine, not exact)

**Use cron when:**
- Exact timing matters ("9:00 AM sharp every Monday")
- Task needs isolation from main session history
- One-shot reminders ("remind me in 20 minutes")

**Tip:** Batch periodic checks into `HEARTBEAT.md` instead of creating multiple cron jobs. Use cron for precise schedules and standalone tasks.

**Reach out when:**
- A Worker has been silent too long on an assigned task
- Credential or resource expiration is imminent
- A blocking issue needs the human admin's decision

**Stay quiet (HEARTBEAT_OK) when:**
- All tasks are progressing normally
- Nothing has changed since last check
- The human admin is clearly in the middle of something
