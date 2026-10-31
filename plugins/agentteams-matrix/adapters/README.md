# Runtime adapters

This bundle carries one runtime adapter per supported AgentTeams runtime.
The bundle contract (`../contract.yaml`) is the runtime-neutral source of
truth; each adapter only projects it onto its runtime.

| Runtime | Status |
|---------|--------|
| qwenpaw | P0 — present |
| openclaw | P1 — present (`adapters/openclaw/`, projects `channels.matrix`) |
| hermes | P1 — present (`adapters/hermes/`, projects the `platforms.matrix` block) |

The OpenClaw and Hermes adapters were added in P1 (commit
`5877c10f`) without changing the contract or the bundle layout; their key
projections were re-aligned with the upstream config schemas in P1-fix.
Each adapter directory carries a README with the per-key upstream
evidence.
