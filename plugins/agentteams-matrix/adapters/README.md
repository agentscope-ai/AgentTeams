# Runtime adapters

The bundle contract (`../contract.yaml`) is the runtime-neutral source of
truth. The OpenClaw config projector remains here; runtime-owned bridges and
native adapters belong to their Harness repositories.

| Runtime | Status |
|---------|--------|
| qwenpaw | [QwenPaw Harness](https://github.com/agentteams-group/agentteams-harness-qwenpaw/tree/main/adapters/matrix) |
| hermes | [Hermes Harness native Matrix overlay](https://github.com/agentteams-group/agentteams-harness-hermes/tree/main/plugins/platforms/matrix) |
| openclaw | Config projector here; optional sidecar in [OpenClaw Harness](https://github.com/agentteams-group/agentteams-harness-openclaw/tree/main/plugins/agentteams-matrix-openclaw) |

The old Core Hermes `agentteams_matrix` plugin is retired. Managed Hermes
images already install the native `platforms/matrix` overlay and need no data
migration. For an independently installed legacy plugin, run its previous
version's `adapters/hermes/uninstall.sh` with the same `HERMES_HOME` first;
then install the Harness native overlay. Preserve credentials and crypto
state, and do not run both adapters for the same account. The new Core
installer rejects `--adapter hermes` instead of reinstalling retired code.
