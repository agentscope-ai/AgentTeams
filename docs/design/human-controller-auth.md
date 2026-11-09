# Human authentication and gateway management

Human accounts log in to Matrix with their own credentials. Clients present that
Matrix access token to the Controller. The Controller verifies it with Matrix
`whoami`, matches the authoritative Human Matrix identity, and resolves the
Human's current permission level. L1 requires a reconciled `status.matrixUserID`
and maps to administrator access; L2 keeps its
existing team scope. L3 retains read-only assigned-worker access. Other levels
and unknown/deleting Humans are rejected.

Matrix token verification retains the existing five-minute cache. Human
permissions and team membership are re-read on every request, including cache
hits, so deleting or downgrading a Human does not retain cached privileges.
Matrix logout/token revocation can still take up to five minutes to propagate.

`GET /api/v1/me` accepts authenticated Human Matrix identities and returns:

```json
{"name":"alice","permissionLevel":1,"accessibleTeams":[]}
```

ServiceAccount tokens remain supported on existing management endpoints but do
not impersonate a Human on `/me`.

Gateway management endpoints use the existing gateway authorization policy:

| Method | Path | Operation |
| --- | --- | --- |
| GET / POST | `/api/v1/gateway/ai/providers` | List / create providers |
| GET / PUT / DELETE | `/api/v1/gateway/ai/providers/{name}` | Read / update / delete a provider |
| GET / POST | `/api/v1/gateway/ai/routes` | List / create AI routes |
| GET / PUT / DELETE | `/api/v1/gateway/ai/routes/{name}` | Read / update / delete an AI route |

L2 callers cannot access these endpoints. The Controller uses its configured
Higress client and internal Console session; user tokens and cookies are not
forwarded to Higress. Provider tokens are removed from responses. Upstream
authentication failures are returned as gateway failures, rather than asking
the user for another administrator login. Unsupported gateway backends return
501. This change does not add cloud AI Gateway management support.

The existing `AGENTTEAMS_ADMIN_USER`, `AGENTTEAMS_ADMIN_PASSWORD` and
`AGENTTEAMS_AI_GATEWAY_ADMIN_URL` Controller configuration supplies Higress
access. Set `AGENTTEAMS_HIGRESS_FALLBACK_CONFIG_WRITE_ENABLED=true` on the
Controller only when its Higress version supports fallback writes. Route list
responses report `fallbackConfigWritable` to clients.

Upgrade the Controller before the Dashboard. New Human logins require `/me` and
do not fall back to a shared administrator credential on older Controllers.
External Console-only Dashboard installations retain their separate legacy
login and gateway access path.
