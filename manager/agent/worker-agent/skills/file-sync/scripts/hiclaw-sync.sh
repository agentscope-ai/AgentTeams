#!/bin/bash
# Deprecated compatibility entrypoint. Use agentteams-sync.sh.
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agentteams-sync.sh" "$@"
