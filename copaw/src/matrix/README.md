# AgentTeams CoPaw Overlay

**Shared overlay files used by both Manager and Worker to enhance CoPaw v1.0.0.**

## Overview

This directory contains AgentTeams-enhanced files that replace CoPaw's built-in modules during Docker image build. These enhancements are already submitted as PRs to CoPaw upstream but not yet released in v1.0.0.

## Files

### `channel.py` + `__init__.py` + `relations.py` + `outbound_policy.py` + `paths.py`
Replaces `copaw/app/channels/matrix/` with enhanced Matrix channel:

- **Unified implementation (Phase 7):** Manager and Worker load the same overlay; Worker installs a thin `custom_channels` shim pointing at `copaw.app.channels.matrix.channel`.
- **E2EE Support**: End-to-end encryption via matrix-nio + libolm
- **History Buffering**: Accumulates non-mentioned messages in group rooms for context
- **Smart Mention Handling**: Strips @mention prefix for slash commands, supports display names
- **Markdown Rendering**: Converts Markdown to HTML using markdown-it-py
- **DM Detection**: Reliable DM vs group room detection via Matrix API
- **Typing Indicators**: Auto-renewal during long-running operations
- **Threads + ledger**: MSC3440 thread relations and persisted recent-event ledger
- **OutboundFilterPolicy**: Team Leader DM preamble suppression, NO_REPLY, assignment reroute

### `config.py` (**frozen**, X7.4)
Replaces `copaw/config/config.py` with enhanced configuration system:

- **OneBot v11 Support**: `OneBotConfig` for NapCat/go-cqhttp/Lagrange
- **Matrix Mention Pills**: `mention_pill_in_body` and `outbound_structured_mentions`
- **Agent Order Persistence**: `agent_order` field for UI state
- **Video Analysis**: `view_video` builtin tool

## Installation

Both Manager and Worker Dockerfiles replace the built-in CoPaw files:

```dockerfile
# Manager: manager/Dockerfile.copaw
COPY --from=copaw-worker src/matrix/ /tmp/matrix-overlay/
RUN SITE=$(python3 -c "import copaw; import os; print(os.path.dirname(copaw.__file__))") \
    && rm -rf "$SITE/app/channels/matrix" \
    && cp -a /tmp/matrix-overlay "$SITE/app/channels/matrix" \
    && if [ -f /tmp/matrix-overlay/config.py ]; then \
         cp /tmp/matrix-overlay/config.py "$SITE/config/config.py"; \
       fi

# Worker: copaw/Dockerfile
COPY src/matrix/ /tmp/matrix-overlay/
RUN COPAW_SITE=/opt/venv/copaw/lib/python3.11/site-packages/copaw \
    && rm -rf "$COPAW_SITE/app/channels/matrix" \
    && cp -a /tmp/matrix-overlay "$COPAW_SITE/app/channels/matrix" \
    && if [ -f /tmp/matrix-overlay/config.py ]; then \
         cp /tmp/matrix-overlay/config.py "$COPAW_SITE/config/config.py"; \
       fi
```

## Configuration

Manager and Worker use identical code but different configurations:

```json
{
  "channels": {
    "matrix": {
      "enabled": true,
      "homeserver": "http://localhost:8080",
      "access_token": "...",
      "encryption": true,
      "allow_from": ["@manager:hs.example"],
      "group_allow_from": ["@worker1:hs.example", "@worker2:hs.example"],
      "history_limit": 50,
      "vision_enabled": true,
      "mention_pill_in_body": false,
      "outbound_structured_mentions": true
    },
    "onebot": {
      "enabled": false,
      "ws_host": "0.0.0.0",
      "ws_port": 6199
    }
  }
}
```

## Status

**Pending upstream release (CoPaw v1.1.0+).**

Once the PRs are merged and released:
1. Update `copaw/pyproject.toml` to use the new version
2. Remove the overlay logic from both Dockerfiles
3. Delete this directory

## Maintenance

When updating overlay files:

1. Edit files in `copaw/src/matrix/`
2. Rebuild both images: `make build-manager-copaw build-copaw-worker`
3. Both Manager and Worker will use the updated versions
