#!/bin/bash
# Deprecated compatibility entrypoint. Use agentteams-import.sh.
exec bash "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agentteams-import.sh" "$@"
