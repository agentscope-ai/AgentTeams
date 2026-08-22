# Enable Peer Mentions Between Workers

By default, Workers only accept @mentions from Manager and admin — not from each other. This prevents infinite mutual-mention loops.

Only enable when admin **explicitly** requests Workers to trigger each other directly (e.g., async handoffs without Manager relay).

## Command

```bash
bash /opt/hiclaw/agent/skills/worker-management/scripts/enable-peer-mentions.sh \
    --workers alice,bob,charlie
```

The script:
1. Adds each Worker to every other Worker's `groupAllowFrom`
2. Pushes updated `openclaw.json` to MinIO
3. Sends Matrix @mention to each Worker to run `hiclaw-sync`

## Critical: Brief Workers afterward

Remind them **not to @mention each other in celebration or acknowledgment messages** — only when they have blocking information that cannot go through Manager. Uncontrolled inter-worker @mentions cause response loops.
