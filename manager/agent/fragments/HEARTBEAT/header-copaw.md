## Manager Heartbeat Checklist (QwenPaw Runtime)

This checklist is for the QwenPaw Manager runtime. Every send into a Matrix **group** room (Worker room, Leader room, project room) or admin notification **must** go through **`copaw channels send`** via the shell tool (see **QwenPaw Message CLI Reference** at the end).

**Hard rules (heartbeat sends — same intent as AGENTS.md Gotchas):**
- **Workers do not see admin DMs.** Status pings, overdue triggers, and task follow-ups belong in the correct Matrix room via `copaw channels send`, using the room id from the task entry, project `meta.json`, or `hiclaw get workers -o json`. Do not rely on admin-facing text alone.
- **`--target-session`** is the literal Matrix room id (no `room:` prefix). **`--target-user`** is the full Matrix id of the Worker or Team Leader you @mention in `--text`. Use single-quoted `--text '...'` so `@` mentions survive the shell.
