---
name: human-management
description: Use when admin requests adding a human user to the system, changing human permissions, removing human access, or managing human accounts.
---

# Human Management

Import real human accounts into HiClaw with configurable permission levels. Humans use their own Matrix client (Element) to communicate with agents.

## Permission Levels (inclusive — higher includes all lower)

| Level | Can talk to | Use case |
|-------|------------|----------|
| 1 | Manager + all Team Leaders + all Workers (= Admin equivalent) | CTO, tech lead |
| 2 | Specified Team Leaders + their Workers + specified standalone Workers | Team member, PM |
| 3 | Specified Workers only | External collaborator |

## Quick Create

```bash
bash /opt/hiclaw/agent/skills/human-management/scripts/create-human.sh \
  --matrix-id "@john:domain" --name "John Doe" \
  --level 2 --teams alpha-team --workers standalone-dev \
  --email john@example.com
```

> Full workflow: read `references/create-human.md`

## Gotchas

- **Humans don't need containers, MinIO, or Higress** — they only need Matrix room access and groupAllowFrom entries
- **Email is optional but recommended** — if provided, a welcome email with credentials is sent automatically
- **Matrix account is registered automatically** — username from `--matrix-id`, random password generated
- **Level 1 humans get access to everything** — `--teams` and `--workers` are ignored
- **Level 3 humans ignore `--teams`** — only `--workers` matters
- **Changing permission level requires recalculating all groupAllowFrom** — use `create-human.sh` with `--update` flag

## Operation Reference

| Admin wants to... | Read | Key script |
|---|---|---|
| Add a human user | `references/create-human.md` | `scripts/create-human.sh` |
| List/query humans | — | `scripts/manage-humans-registry.sh --action list` |
| Remove human access | — | `scripts/manage-humans-registry.sh --action remove` |
