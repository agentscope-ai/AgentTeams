#!/bin/bash
# Deprecated compatibility entrypoint. Use agentteams-verify.sh.
exec bash "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/agentteams-verify.sh" "$@"
