# Create Human

## Overview

Import a real human account into HiClaw. The script registers a Matrix account, configures permissions based on the specified level, and optionally sends a welcome email.

## Prerequisites

- SMTP configured in environment (for email notifications):
  - `HICLAW_SMTP_HOST`, `HICLAW_SMTP_PORT`, `HICLAW_SMTP_USER`, `HICLAW_SMTP_PASS`, `HICLAW_SMTP_FROM`
- For Level 2: target teams must already exist in `teams-registry.json`
- For Level 3: target workers must already exist in `workers-registry.json`

## Script Usage

```bash
# Level 1: Admin-equivalent access
bash /opt/hiclaw/agent/skills/human-management/scripts/create-human.sh \
  --matrix-id "@john:domain" --name "John Doe" --level 1 \
  --email john@example.com

# Level 2: Team-scoped access
bash /opt/hiclaw/agent/skills/human-management/scripts/create-human.sh \
  --matrix-id "@jane:domain" --name "Jane Smith" --level 2 \
  --teams alpha-team,beta-team --workers standalone-dev \
  --email jane@example.com

# Level 3: Worker-only access
bash /opt/hiclaw/agent/skills/human-management/scripts/create-human.sh \
  --matrix-id "@bob:domain" --name "Bob" --level 3 \
  --workers alice,charlie \
  --email bob@example.com
```

## What the Script Does

1. Registers Matrix account (username from `--matrix-id`, random password)
2. Configures permissions based on level (inclusive — higher includes lower):

### Level 1 (Admin equivalent)
- Adds human to Manager's `groupAllowFrom` + `dm.allowFrom`
- Adds human to ALL Team Leaders' `groupAllowFrom`
- Adds human to ALL Workers' `groupAllowFrom`
- Invites to all Team Rooms and Worker Rooms

### Level 2 (Team-scoped)
- Adds human to specified Team Leaders' `groupAllowFrom`
- Adds human to all Workers under those teams' `groupAllowFrom`
- Adds human to specified standalone Workers' `groupAllowFrom`
- Invites to specified Team Rooms and corresponding Worker Rooms

### Level 3 (Worker-only)
- Adds human to specified Workers' `groupAllowFrom`
- Invites to specified Worker Rooms

3. Updates `humans-registry.json`
4. Pushes updated `openclaw.json` files to MinIO
5. Notifies affected agents to `file-sync`
6. Sends welcome email (if `--email` provided and SMTP configured)

## Welcome Email Content

```
Subject: Welcome to HiClaw - Your Account Details

Hi {display_name},

Your HiClaw account has been created:

  Username: {matrix_user_id}
  Password: {generated_password}
  Login URL: {element_web_url}

Please log in and change your password immediately.

— HiClaw
```

## After Creation

- Human can log into Element Web and start chatting with permitted agents
- Agents will recognize the human via their `groupAllowFrom` list
- Human messages are treated as authorized requests within their permission scope
