# Runtime adapters

This bundle carries one runtime adapter per supported AgentTeams runtime.
The bundle contract (`../contract.yaml`) is the runtime-neutral source of
truth; each adapter only projects it onto its runtime.

| Runtime | Status |
|---------|--------|
| qwenpaw | P0 — present |
| hermes | PR1 — present (platform plugin, in-process core) |
| openclaw | PR2 — absent until the sidecar protocol lands |

The Hermes adapter registers the `agentteams_matrix` platform with the
Hermes gateway and drives the bundle's runtime-neutral core in-process
(see `hermes/README.md`). The OpenClaw adapter lands in PR2 together
with the sidecar protocol, without changing the contract or the bundle
layout.
