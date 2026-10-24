# Runtime adapters

This bundle carries one runtime adapter per supported AgentTeams runtime.
The bundle contract (`../contract.yaml`) is the runtime-neutral source of
truth; each adapter only projects it onto its runtime.

| Runtime | Status |
|---------|--------|
| qwenpaw | P0 — present |
| openclaw | later phase — intentionally absent in P0 |
| hermes | later phase — intentionally absent in P0 |

The OpenClaw and Hermes adapters are deliberately not included in P0. They
will land in later phases without changing the contract or the bundle layout.
The experimental deepseek-harness adapter remains in the `plugins/teamharness`
compatibility path and is not part of this portable bundle in P0.
