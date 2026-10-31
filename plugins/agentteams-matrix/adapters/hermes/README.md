# agentteams-matrix — Hermes Adapter

This adapter projects the bundle contract (`../../contract.yaml`) onto the
Hermes runtime as a platform plugin (`kind: platform` in `plugin.yaml`).
The plugin runs the bundle's runtime-neutral core in-process: one
`core.transport.NioTransport` per bot, built on the client from
`core.transport.build_client` (client-injection path — the adapter keeps
the client reference for the joined-members DM probe, read receipts,
display names and media upload).

Payload (installed files):

```text
plugin.yaml          # kind: platform, env surface below
adapter.py           # register(ctx) entry point + the adapter
__init__.py          # package-style import of register
core/                # the bundle's runtime-neutral core (copied from the bundle root)
```

## Supported Hermes versions

Minimum supported Hermes runtime: **0.22.5**, declared as
`requires_hermes: ">=0.22.5"` in `plugin.yaml` (the plugin loader blocks
loading on a runtime below the minimum). The API surface this adapter
imports — `gateway.platforms._shared`, `gateway.platforms.event`,
`PluginContext.register_platform`, and dynamic `Platform` members
resolved through `platform_registry` — first exists at 0.22.5 and does
not exist at `v2026.4.16`. The surface is verified against the real SDK
source at `v0.22.5` (it also holds at `v2026.9.24`) by
`plugins/tests/agentteams-matrix/test_adapter_hermes_sdk.py`, which runs
on every CI pass.

**Boundary:** the current Worker image pins
`HERMES_GIT_REF=v2026.4.16` (`hermes/Dockerfile`), which is **below the
minimum**. This adapter is not compatible with the current Worker image
and must not be claimed to be; the runtime pin update is carried
separately in issue #1460, and until it lands the adapter requires a
Hermes runtime at or above 0.22.5.

## Runtime requirement: matrix-nio

The core transport needs `matrix-nio`; E2EE additionally needs the
crypto backend: `matrix-nio >= 0.26` provides it through `vodozemac`
(the `e2e` extra), older releases through `olm`. Install in the
environment that runs the gateway:

```bash
pip install "matrix-nio[e2e]"
```

(On `matrix-nio < 0.26` the extra additionally needs the system
`libolm`, e.g. `apt/dnf install libolm-dev`.) Without any importable
backend the adapter degrades by design: it logs

```text
AgentTeams Matrix: no Matrix E2EE backend (vodozemac/olm) — falling back
to non-encrypted mode. Install matrix-nio[e2e]; on matrix-nio < 0.26
also install libolm (apt/dnf install libolm-dev).
```

and continues unencrypted. `install.sh` deliberately does not pip
install this: the host Python environment is owned by the packaging
line, and the installer must not touch the host venv.

## Configuration (env-driven)

| Env var | Required | Purpose |
|---------|----------|---------|
| `AGENTTEAMS_MATRIX_SERVER` | yes | Matrix homeserver URL |
| `AGENTTEAMS_MATRIX_USER_ID` | yes | Bot Matrix user ID (MXID) |
| `AGENTTEAMS_MATRIX_TOKEN` | yes | Bot access token (secret; `password: true`) |
| `AGENTTEAMS_MATRIX_DEVICE_ID` | no | Device ID for login |
| `AGENTTEAMS_MATRIX_STATE_DIR` | no | State dir (sync token / E2EE keys / media cache); default `<HERMES_HOME>/state/agentteams_matrix` |
| `AGENTTEAMS_MATRIX_ENCRYPTION` | no | `1/true/yes` enables E2EE (degrades without the backend) |
| `AGENTTEAMS_MATRIX_HISTORY_LIMIT` | no | Group history buffer size per room (default 50) |
| `AGENTTEAMS_MATRIX_SYNC_TIMEOUT_MS` | no | Sync long-poll timeout (min 1000, default 30000) |
| `AGENTTEAMS_MATRIX_ALLOWED_USERS` | no | Comma-separated MXID allowlist |
| `AGENTTEAMS_MATRIX_ALLOW_ALL_USERS` | no | Allow any room member (dev only) |
| `AGENTTEAMS_MATRIX_HOME_ROOM` | no | Room ID for cron / notification delivery |

All reads go through `gateway.platforms._shared.get_scoped_secret`
(multiplex-safe; never `os.getenv`); env wins over `config.yaml`
`gateway.platforms.agentteams_matrix.extra` at construct time.

## Behaviour (QwenPaw channel parity)

- Inbound through the core `InboundEvent`: self-skip, DM detection via
  the joined_members API (30 s cache, `room.users` / `member_count`
  fallback), mention gate in group rooms (on by default), unmentioned
  group messages buffered in `core.history.RoomHistory` and prepended to
  the next mention, read receipt + typing on receipt, mention-prefix
  strip, `NO_REPLY` drop.
- Outbound: text via `NioTransport.send`, media via `client.upload` +
  `room_send` (the core transport has no upload surface), typing via
  `NioTransport.typing`.
- Cron delivery: `cron_deliver_env_var` = `AGENTTEAMS_MATRIX_HOME_ROOM`
  plus a standalone sender (ephemeral client, one `room_send`, close).

## Install

```bash
plugins/agentteams-matrix/scripts/install.sh
```

The installer copies the payload into:

```text
${HERMES_HOME:-$HOME/.hermes}/plugins/agentteams-matrix/
```

and records per-file SHA-256 hashes in the marker:

```text
${HERMES_HOME:-$HOME/.hermes}/.agentteams/agentteams-matrix/installed.json
```

Idempotent: an unchanged re-run reports `up to date` and rewrites
nothing; a changed payload updates only the changed files and reconciles
removed ones. Uninstall (`scripts/uninstall.sh` →
`adapters/hermes/uninstall.sh`) removes exactly the marker-recorded
files and is a no-op without a marker.
