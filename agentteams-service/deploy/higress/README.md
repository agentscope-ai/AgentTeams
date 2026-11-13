# deploy/higress — route split (R1)

`routes.yaml` freezes the five-route split from 58 §1.5/§6 (D110):

| Route | Owner | Envelope |
|---|---|---|
| `POST /api/v1/workers` | service native | new `{code,message}` |
| `GET /api/v1/workers` | service native | new `{code,message}` |
| `GET /api/v1/workers/{name}` | service native | new `{code,message}` |
| `PUT /api/v1/workers/{name}` | service → controller proxy | controller's old `{message}` |
| `DELETE /api/v1/workers/{name}` | service → controller proxy | controller's old `{message}` |

`/internal/v1/*` is never routed by Higress. The G1 gate compares the closed
mode (service disabled, controller serves everything) against the enabled
mode on exactly these five routes.

R1 ships only the declarative file: the installers/helm wiring is an R2 task
(58 §6 O3). Do not add a wildcard route — the audit round removed it because
`PUT/DELETE/GET-by-name` would have had no owner.
