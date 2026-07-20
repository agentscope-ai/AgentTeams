#!/bin/bash
# Deprecated compatibility entrypoint. Use agentteams-apply.sh.
exec bash "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agentteams-apply.sh" "$@"
