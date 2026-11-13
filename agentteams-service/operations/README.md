# operations/ — R1 operation manifest

`workers.json` is the machine-readable operation manifest for the R1
worker API. It is the single source for:

- the `agt` CLI flag bindings (R2 wires the generator; R1 freezes the file),
- the Skill schema (`worker.create` / `worker.list` / `worker.get`),
- the CI drift gate (re-generate, then `git diff --exit-code`).

Scope rules:

| Route | R1 owner | Generated as |
|---|---|---|
| `POST /api/v1/workers` | service native | operation |
| `GET /api/v1/workers` | service native | operation |
| `GET /api/v1/workers/{name}` | service native | operation |
| `PUT /api/v1/workers/{name}` | service → controller proxy (D110) | proxy route only |
| `DELETE /api/v1/workers/{name}` | service → controller proxy (D110) | proxy route only |

R1 does **not** change the controller-side CLI. `agt` lives in
`agentteams-controller/cmd/agt`; wiring the generator to it is an R2 task,
because "controller zero change" is the hardest G1 precondition.

Field-count note: `WorkerResponse` has **43** top-level fields in the
source (`types.go:92-146`); 58 §1.4's "45" mistakenly counts
`ExposedPortInfo.port/domain` as top-level. The three projection views are
27 / 12 / 35 fields. This is tracked as contract debt and corrected in the
58 amendment.
