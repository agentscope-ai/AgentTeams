## Controller API Rules

**CRITICAL**: When creating, deleting, or otherwise managing Workers / Teams / Projects / Humans:

- ✅ **ALWAYS USE**: the `hiclaw` CLI (`hiclaw create worker`, `hiclaw get workers`, `hiclaw delete worker`, `hiclaw create team`, `hiclaw manager-state`, etc.) and the helper scripts under `~/skills/*/scripts/`
- **OpenClaw Manager task board**: prefer `hiclaw manager-state` (or `manage-state.sh`, which delegates to it). Set `HICLAW_MANAGER_STATE_IMPL=shell` to force the legacy shell implementation.
- ❌ **NEVER USE**: direct `curl` to `${AGENTTEAMS_CONTROLLER_URL}/api/v1/...` (you will see this URL in env vars and inside `/opt/hiclaw/scripts/lib/container-api.sh` — those are for internal supervisord / startup use only, **NOT** for your turn)

**Why**: The CLI handles SOUL multi-line escaping, retry logic, request validation, and follow-up provisioning. Hand-built curl requests routinely break on shell escaping of multi-line `--soul` content; failed escaping returns 401/400 which look like "token expired" or "bad endpoint" but are actually your own command being parsed wrong. If `hiclaw create worker` appears slow or stuck, run `hiclaw get workers -o json` to confirm the actual worker phase — do **NOT** bypass the CLI.

**Token note**: `AGENTTEAMS_AUTH_TOKEN` / `AGENTTEAMS_AUTH_TOKEN_FILE` are 10-year SA tokens auto-rotated by the platform. A 401 from the controller is almost never a token problem — it is almost always your shell escaping breaking the request. Do not "try a fresh token" as a fix; re-check your command quoting first.
