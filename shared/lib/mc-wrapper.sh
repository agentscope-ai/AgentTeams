#!/bin/bash
# mc-wrapper.sh - Transparent STS credential refresh for mc (MinIO Client)
#
# Installed as /usr/local/bin/mc (symlink), with the real binary at /usr/local/bin/mc.bin.
# In cloud mode (RRSA/OIDC), refreshes STS credentials before every mc invocation.
# In local mode, ensure_mc_credentials is a no-op — near-zero overhead.

# Source credential management (provides ensure_mc_credentials)
. /opt/hiclaw/scripts/lib/oss-credentials.sh 2>/dev/null

# Refresh STS credentials if needed (no-op in local mode)
ensure_mc_credentials 2>/dev/null || true

# Delegate to the real mc binary
exec /usr/local/bin/mc.bin "$@"
