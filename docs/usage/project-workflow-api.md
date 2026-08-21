# Project / Workflow Inspection API

> Added by the project workflow inspection PR (agentteams/AgentTeams#1169).

The controller exposes four read-only endpoints that surface TeamHarness
project state (`shared/projects/{id}/meta.json`) as a LangGraph-aligned
workflow view, plus per-team spawn subagent sessions and their conversation
streams. They are the data source for human-facing views (dashboard, QwenPaw
console plugin) and are consumed by `agt get projects`.

## Scope & prerequisites

These endpoints work in **any AgentTeams deployment** that runs TeamHarness
(projectflow/taskflow) on its workers — the storage layout is read through
the controller's configured object-store client, so `AGENTTEAMS_STORAGE_PREFIX`
and `AGENTTEAMS_FS_BUCKET` (including non-default values) are handled
automatically; no per-deployment code or configuration is required.

Prerequisites:

* TeamHarness MCP (`plugins/teamharness`) is installed on the workers that
  orchestrate projects. Only projects created by `projectflow`
  (`create_project` / `create_quick_project`) produce the
  `shared/projects/{id}/meta.json` that these endpoints read. Teams that
  manage tasks manually without projectflow have no project data — that is
  expected, not a bug.
* Project writes are pushed to shared storage by `_sync_project`
  (introduced alongside this API), so the controller sees near-live state
  rather than a startup snapshot.

Deployment modes (embedded Docker, incluster K8s) are all supported; in the
no-K8s development mode the controller skips authentication like the other
endpoints, so RBAC applies only when an authenticator is configured.

## Endpoints

### `GET /api/v1/projects`

List projects across all teams (and the global `shared/projects/` prefix).

Query parameters:

| Param | Meaning |
|:--|:--|
| `team` | Return only projects whose team matches. Team leaders are already scoped to their own team(s); standalone projects (empty team) only match when no filter is set. |

Response `200 OK`:

```json
{
  "projects": [
    {
      "project_id": "demo-project-001",
      "title": "Demo project",
      "status": "active",
      "plan_type": "dag",
      "team_id": "biz-team",
      "mode": "project"
    }
  ],
  "total": 1
}
```

* `status` is the raw project status written by TeamHarness:
  `active` | `paused` | `completed`.
* Projects are sorted by `project_id`. Duplicate ids across prefixes are
  de-duplicated (meta.json may be mirrored under both the effective team name
  and the CR name prefix).
* Projects with a missing or malformed `meta.json` are skipped (the directory
  may exist while the file is mid-write upstream).

### `GET /api/v1/projects/{id}/workflow`

Return the LangGraph-aligned workflow for one project.

Optional query parameter:

| Parameter | Type | Meaning |
|:--|:--|:--|
| `includeTasks` | `bool` | When `true`, also read each task's TaskMeta (`shared/tasks/{id}/meta.json`) and attach a `tasks_detail` array with spec/result/deliverable fields. Default `false` keeps the response lightweight. |

Response `200 OK`:

```json
{
  "project_id": "demo-project-001",
  "title": "Demo project",
  "status": "active",
  "plan_type": "dag",
  "team_id": "biz-team",
  "mode": "project",
  "source": "dingtalk",
  "nodes": [
    {"id": "t1", "name": "Task 1", "status": "completed", "assignee": "@w1:matrix.local"},
    {"id": "t2", "name": "Task 2", "status": "delegated", "assignee": "@w2:matrix.local"}
  ],
  "edges": [
    {"source": "t1", "target": "t2", "conditional": false}
  ],
  "next": ["t2"],
  "interrupts": [
    {"id": "t3", "value": "blocked"},
    {"id": "loop", "value": "waiting for human decision"}
  ],
  "values": {
    "project_id": "demo-project-001",
    "title": "Demo project",
    "status": "active",
    "plan_type": "dag",
    "team_id": "biz-team",
    "mode": "project",
    "task_count": {"completed": 1, "delegated": 1}
  },
  "loop": null,
  "requester": "dingtalk:user:session",
  "source_room_id": "!room:matrix.local",
  "tasks_detail": [
    {
      "task_id": "t1",
      "project_id": "demo-project-001",
      "status": "completed",
      "spec_path": "shared/tasks/t1/spec.md",
      "assigned_to": "@w1:matrix.local",
      "summary": "Alpha report done",
      "result_status": "SUCCESS",
      "deliverables": [{"type": "file", "path": "shared/tasks/t1/output.pdf"}],
      "result_path": "shared/tasks/t1/result.md"
    }
  ]
}
```

`tasks_detail` is only present when `?includeTasks=true`. It surfaces the
TaskMeta fields that the project-level `nodes[]` summary does not carry:
`spec_path` (task spec file), `summary` / `result_status` / `result_path`
(submission result), `deliverables` (artifact list) and `cancel_reason`.
TaskMeta is read from the project's owning scope only: team projects read
`teams/{team}/shared/tasks/{id}/meta.json`, standalone projects read
`shared/tasks/{id}/meta.json`. There is no cross-scope fallback, and a
TaskMeta whose `project_id` names a different project is rejected — an
unrelated task that happens to share the id can never mix in. Tasks without
a TaskMeta file (e.g. not yet delegated) are skipped; per-task read errors
are skipped so one bad task never fails the whole response.

Node statuses are normalized to a frontend-friendly enum:

| API value | Raw TeamHarness status |
|:--|:--|
| `pending` | `planned` |
| `delegated` | `assigned` |
| `in-progress` | `in_progress`, `submitted` |
| `completed` | `completed` |
| `revision` | `revision` |
| `blocked` | `blocked`, `cancelled` |

Semantics (mirror upstream `_ready_nodes` / `_ready_loop_nodes`):

* `next` — ready nodes: tasks whose raw status is `planned`/`assigned` and
  whose dependencies are all `completed`. Empty when the project is not active
  or a loop is `waiting_user` / `blocked` / `completed`.
* `interrupts` — human-decision waiting points: a blocked task, or a loop in
  `waiting_user` / `blocked` state.
* `values.task_count` — node counts per normalized status.

Error responses:

| Code | Meaning |
|:--|:--|
| `400` | Missing project id. |
| `403` | Authenticated but the role cannot read projects at all (e.g. Worker). |
| `404` | Project not found (no meta.json under any scanned prefix) — **or** the caller is a scoped reader (team leader / L2 human) who does not own the project (existence is hidden to prevent id enumeration). |
| `500` | K8s or object-store failure. |

### `GET /api/v1/projects/{id}/tasks/{taskId}/artifact`

Download one of a task's artifacts, completing the "deliverable → download →
review → accept" loop for dashboards and the console plugin.

Optional query parameter:

| Param | Meaning |
|:--|:--|
| `path` | The artifact path to download. Must be one of the task's **declared** artifacts — `result_path`, `spec_path` or an entry of `deliverables` (all read from TaskMeta). When omitted, the `result_path` (the published result) is served. |

Without `?path=` the artifact is the task's `result_path` (published result).
With `?path=` the requested path must be one of the task's declared artifacts
— `result_path`, `spec_path` (task spec) or a `deliverables` entry. The path
is then validated against a strict allowlist: it must be under
`shared/tasks/{taskId}/` or `shared/projects/{projectId}/`, and must not
contain `..` or start with `/`. Because the allowlist AND the declared-artifact
check both apply, a compromised worker cannot craft a path that reads
arbitrary MinIO objects, nor can a client download an undeclared file that
happens to live in the task directory.

The file is returned with `Content-Disposition: attachment` (filename =
basename, RFC 5987 `filename*=utf-8''...` for non-ASCII names so Chinese
filenames download correctly) and a `Content-Type` inferred from the
extension.

Error responses:

| Code | Meaning |
|:--|:--|
| `400` | Missing project id or task id. |
| `403` | Authenticated but the role cannot read projects at all (e.g. Worker). |
| `404` | Project not found / caller does not own it (existence hidden) / task not in the project graph / task has no published artifact / requested path is not a declared artifact / artifact file missing / artifact path rejected / TaskMeta exists only outside the project's scope or belongs to another project. |
| `500` | K8s or object-store failure. |

### `GET /api/v1/projects/{id}/spawns`

Aggregate the **spawn subagent sessions** created by the project team's
workers. Spawn subagents are the primary execution vehicle of worker agents
(workers schedule, spawns execute); this endpoint makes that activity
visible alongside the team-level task graph.

Data source: each team worker's `chats.json`
(`agents/{worker}/.qwenpaw/workspaces/default/chats.json` in object storage,
mirrored by the worker's FileSync). A chat entry is a spawn session when
`meta.spawn == true` (QwenPaw 2.1+) or its `session_id` carries the `sub-`
prefix (2.0.1 fallback).

Response:

```json
{
  "project_id": "demo-project-001",
  "workers": [
    {
      "worker": "sysdev-lead",
      "spawns": [
        {
          "session_id": "sub-3f2a9b1c",
          "name": "fix harmony build script",
          "status": "running",
          "created_at": "2026-08-13T10:00:00+00:00",
          "updated_at": "2026-08-13T10:05:00+00:00",
          "root_session_id": "matrix:!room:server",
          "spawn": true,
          "subagent_allowed_tools": ["read_file", "write_file"],
          "subagent_skills": ["pdf"]
        }
      ]
    }
  ]
}
```

Notes:

- `root_session_id` is the normalized session key of the parent session
  (`matrix:` prefix canonicalized) — the session that called
  `spawn_subagent`. The endpoint is **project-scoped, not team-scoped**: a
  spawn is listed only when its root session is one of the project's rooms
  (the project `source_room_id` or a graph task's `TaskMeta.room_id`). A
  spawn rooted elsewhere — another project's room, or a legacy 2.0.1 spawn
  without a persisted root — is **omitted**, never attached to every project
  of the team.
- A worker with a missing or unreadable `chats.json` is still listed, with
  an empty `spawns` array — one broken worker never 500s the whole project.
- `subagent_allowed_tools` / `subagent_skills` are only present when the
  spawn was created with a tool/skill whitelist (2.1+).
- `name` is the chat title assigned when the spawn session was created
  (not the full task prompt).
- Standalone projects (no owning team) return an empty `workers` array —
  there is no team membership to derive the worker list from.

Error responses:

| Code | Meaning |
|:--|:--|
| `400` | Missing project id. |
| `403` | Authenticated but the role cannot read projects at all (e.g. Worker). |
| `404` | Project not found / caller does not own it (existence hidden). |
| `500` | K8s or object-store failure. |

### `GET /api/v1/projects/{id}/spawns/{sessionId}/messages`

Returns one spawn session's **conversation stream** — user context messages,
model turns and tool results — so a client can show what the spawn is doing,
its full task prompt, and its progress. The stream is read from the owning
worker's `history.db` (QwenPaw's scroll `HistoryStore`, mirrored into object
storage by the worker FileSync alongside `chats.json`).

Query parameters:

| Param | Type | Default | Meaning |
|:--|:--|:--|:--|
| `limit` | int | `20` | Number of messages (most recent window). Capped at `50`. |

Response:

```json
{
  "session_id": "sub-3f2a9b1c",
  "task": "Design the auth module",
  "messages": [
    {
      "seq": 1,
      "kind": "context_msg",
      "role": "user",
      "content": "Design the auth module",
      "created_at": "2026-08-13T15:00:00"
    },
    {
      "seq": 2,
      "kind": "model_turn",
      "role": "assistant",
      "name": "read_file",
      "content": "Reading the spec first",
      "headline": "read spec",
      "created_at": "2026-08-13T15:00:05"
    },
    {
      "seq": 3,
      "kind": "tool_result",
      "role": "assistant",
      "name": "read_file",
      "content": "spec.md contents",
      "tool_state": "success",
      "created_at": "2026-08-13T15:00:06"
    }
  ],
  "has_more": false
}
```

- `kind` is one of `context_msg` (user message), `model_turn` (assistant
  reply — `headline` is the turn's retrieval headline, "what it is doing at
  a glance"), or `tool_result` (tool execution result — `name` is the tool
  and `tool_state` its final state).
- `task` is the first user message of the session — the spawn task text.
- `messages` is ascending (oldest first), covering the most recent `limit`
  rows; `has_more` reports whether older rows exist.
- The database is pulled to a temp dir and opened strictly read-only
  (pure-Go `modernc.org/sqlite`), with a fallback that reads the
  checkpointed portion when the WAL sidecars are missing or inconsistent.
  The schema is stable across QwenPaw 2.0.1 and 2.1.
- An empty session (db present, no rows) returns `200` with `messages: []`.

Error responses:

| Code | Meaning |
|:--|:--|
| `400` | Missing project id or session id. |
| `403` | Authenticated but the role cannot read projects at all (e.g. Worker). |
| `404` | Project not found / caller does not own it (existence hidden) / session not owned by any team worker / `history.db` missing or unreadable. |
| `500` | K8s or object-store failure. |

### `GET /api/v1/projects/{id}/history`

Returns the project's **intervention timeline** — the pre-intervention
`meta.json` snapshots written by the lifecycle endpoints (see
`POST /pause`, `POST /resume`, … below; each intervention snapshots the
previous state into `history/{unixNano}.json`, retaining the most recent 50).

Query parameters:

| Param | Type | Default | Meaning |
|:--|:--|:--|:--|
| `team` | string | — | Optional team qualifier, same semantics as the other read endpoints. |

Response:

```json
{
  "project_id": "demo-project-001",
  "snapshots": [
    { "timestamp": "1723785123456789012" }
  ]
}
```

- `snapshots` is **newest first**. `timestamp` is the snapshot filename
  (unix nanoseconds) and is kept as a **string** — 19-digit nanosecond
  values exceed the JavaScript safe-integer range, so numeric transport
  would silently lose precision.
- An empty history returns `200` with `"snapshots": []`.

### `GET /api/v1/projects/{id}/history/{timestamp}`

Returns one snapshot's raw `meta.json` content **verbatim** (same schema as
`GET /workflow`'s source). `timestamp` must be a 19-digit unixNano value;
anything else is rejected `400` (this doubles as the traversal guard).

Error responses:

| Code | Meaning |
|:--|:--|
| `400` | Missing project id / malformed timestamp. |
| `403` | Authenticated but the role cannot read projects at all (e.g. Worker). |
| `404` | Project or snapshot not found / caller does not own it (existence hidden). |
| `409` | Ambiguous project id across teams; retry with `?team=`. |
| `500` | K8s or object-store failure. |

## Project identity & disambiguation

Project ids are only unique within a worker workspace upstream: two teams can
hold the same project id. The API therefore treats `(team, project_id)` as
the identity:

- `GET /api/v1/projects` lists every distinct `(team, project_id)` — the same
  id under two teams appears twice, disambiguated by `team_id` (a scoped
  caller only sees the entries of their accessible teams).
- The read endpoints (`workflow`, `tasks/{taskId}/artifact`, `spawns`,
  `spawns/{sessionId}/messages`, `history`, `history/{timestamp}`) accept an
  optional `?team=` query parameter that narrows resolution to that team's
  storage prefix.
- Without `?team=`, if the same project id exists under multiple teams the
  endpoint returns `409 Conflict` (`project id is ambiguous across teams;
  retry with ?team=`) instead of silently resolving to the first match.
  Callers scoped to one of those teams (team leader / L2 human) resolve their
  own team's project without ambiguity.

## Authentication & authorization

Two bearer-token paths are accepted (composite authenticator):

1. **Kubernetes service-account token** (TokenReview): admin / manager /
   worker. Team leaders (worker with `team_leader` role) see only their own
   team's projects.
2. **Matrix access token** (L2 humans): the token is validated with
   `GET /_matrix/client/v3/account/whoami`; the owning Matrix localpart is
   matched to a `Human` CR with `permissionLevel: 2` (Team). The human's
   `accessibleTeams` set is used as the multi-team scope — every team they
   control is aggregated into a single list/read view. Non-L2 humans
   (permissionLevel 1 or 3) are rejected.

Authorization matrix:

| Caller | List | Get workflow | Get spawns | Get spawn messages | Get history |
|:--|:--|:--|:--|:--|:--|
| admin / manager | all teams | any project | any project | any project | any project |
| team-leader (SA) | own team only | own team only | own team only | own team only | own team only |
| L2 human (Matrix) | all `accessibleTeams` | any accessible team | any accessible team | any accessible team | any accessible team |
| worker | denied | denied | denied | denied | denied |

## `agt` CLI

`agt get projects [name]` wraps both endpoints:

```bash
agt get projects                      # list all
agt get projects --team biz-team      # filter by team
agt get projects demo-project-001     # workflow detail
agt get projects demo-project-001 -o json
agt get projects demo-project-001 --mermaid   # render DAG as mermaid
```

The CLI forwards whatever bearer token is configured (`AGENTTEAMS_AUTH_TOKEN`
or `AGENTTEAMS_AUTH_TOKEN_FILE`) verbatim, so an L2 human can also use it by
pointing either variable at their own Matrix access token — no separate CLI
auth mode is needed.

## Human intervention & lifecycle endpoints

The read endpoints above are complemented by write endpoints that let
humans intervene in agent-orchestrated workflows. All writes are code-level
authorized: the middleware rejects cross-team writes (authorizer
`requireSameTeam`), and the handler additionally runs `checkProjectAccess`
after resolving the owning team because the middleware cannot map a project
path to a team. Every write is stamped with audit fields
(`updated_by` / `updated_at`, and `pause_reason` when a reason is given) and
applies an ETag conditional write — the object's ETag (content hash) is
bound at read time and the write is a MinIO If-Match conditional write, so
a worker pushing a newer `meta.json` between the read and the write makes
the write fail with `409` instead of clobbering it.

### `POST /api/v1/projects`

Create a project (structured, aligned with TeamHarness `create_project`).
An admin/manager may create a standalone project without a team; a team
leader or L2 human must pass a `team_id` they can access.

Request body:

```json
{
  "title": "New project",
  "source": "matrix",
  "requester": "@luo:server",
  "team_id": "biz-team",
  "project_id": "optional-custom-id",
  "source_room_id": "!room:server"
}
```

`project_id` defaults to a generated value when omitted and must be a plain
token matching TeamHarness `_safe_id` (`[A-Za-z0-9][A-Za-z0-9._-]*`); the
generated default is a compact timestamp + nanoseconds (never an RFC3339
timestamp — its `:` would be rejected by TeamHarness). Response `201 Created`:

```json
{
  "project_id": "proj-20260814-160628-406426239",
  "title": "New project",
  "status": "active",
  "team_id": "biz-team",
  "plan_type": "dag"
}
```

Errors: `400` missing/invalid title or project id / team required for
scoped callers; `409` project already exists; `403`/`404` cross-team
(denied / existence hidden).

### `POST /api/v1/projects/{id}/pause`

Set a project's status to `paused`. Pausing stops new task dispatch
(`ready_nodes` returns empty) but does not interrupt in-flight tasks; their
completion reports still arrive (documented behavior — in-flight work is
not cancelled). Optional body `{"reason": "..."}` is recorded in
`pause_reason`. Response `200` returns the updated workflow
(`buildWorkflow`). Errors: `409` already paused / completed; `404` not
found or not owned.

### `POST /api/v1/projects/{id}/resume`

Set a paused project back to `active`. Response `200` returns the updated
workflow. Errors: `409` not paused; `404` not found or not owned.

### `POST /api/v1/projects/{id}/replan`

Replace a project's DAG plan. The request body carries the new tasks
(optional `tasks` array):

```json
{
  "tasks": [
    {"taskId": "t1", "title": "Step 1", "assignedTo": "@dev:server", "dependsOn": []},
    {"taskId": "t2", "title": "Step 2", "dependsOn": ["t1"]}
  ]
}
```

Fields are normalized like TeamHarness `_normalize_task`
(`taskId`/`task_id`, `assignedTo`/`assigned_to`, `dependsOn`/`depends_on`,
status defaults to `planned`, `pending` maps to `planned`); a task id that
already exists keeps its previous title/assignee/status when the raw entry
omits them. Validation mirrors `_validate_task_graph`: duplicate ids,
unknown dependencies, and dependency cycles are rejected with `400`.
Preconditions (`409`): `plan_type` must be `dag` (loop replans go through
`record_loop_iteration`), status must be `active`, and no task may be
`in_progress`/`submitted`. Response `200` returns the updated workflow.

### `POST /api/v1/projects/{id}/tasks/{taskId}/cancel`

Cancel a single task. Body requires `reason` (and optional
`replacementTaskId`). The task must be mutable — a terminal task
(completed/revision/blocked/cancelled) is rejected with `409`. The task's
`TaskMeta` is stamped `status=cancelled` + `cancel_reason` and the project
node status is updated. Response `200` returns the updated workflow.
Errors: `400` missing reason; `404` task not in project / task meta
missing; `409` terminal task.

### `POST /api/v1/projects/{id}/complete`

Mark a project completed (terminal state). All tasks must be in a terminal
status (completed/revision/blocked/cancelled — no in_progress/submitted/
planned), otherwise `409`. Response `200` returns the updated workflow.

### Notifications

After a successful write, the Controller sends an admin message
(`SendMessageAsAdmin`) to the project's `source_room_id` (falling back to
`reply_route.target_session`), so agents in that room learn about the
intervention without polling. Best-effort: no notification is sent when
the room is unknown or Matrix is not configured.

## Authentication & authorization

Two bearer-token paths are accepted (composite authenticator):

1. **Kubernetes service-account token** (TokenReview): admin / manager /
   worker. Team leaders (worker with `team_leader` role) see only their own
   team's projects.
2. **Matrix access token** (L2 humans): the token is validated with
   `GET /_matrix/client/v3/account/whoami`; the owning Matrix localpart is
   matched to a `Human` CR with `permissionLevel: 2` (Team). The human's
   `accessibleTeams` set is used as the multi-team scope — every team they
   control is aggregated into a single list/read view. Non-L2 humans
   (permissionLevel 1 or 3) are rejected.

Authorization matrix:

| Caller | List | Get workflow | Write (create/pause/resume/replan/cancel/complete) |
|:--|:--|:--|:--|
| admin / manager | all teams | any project | any project |
| team-leader (SA) | own team only | own team only | own team only |
| L2 human (Matrix) | all `accessibleTeams` | any accessible team | any accessible team |
| worker | denied | denied | denied |

## `agt` CLI

`agt get projects [name]` wraps both endpoints:

```bash
agt get projects                      # list all
agt get projects --team biz-team      # filter by team
agt get projects demo-project-001     # workflow detail
agt get projects demo-project-001 -o json
agt get projects demo-project-001 --mermaid   # render DAG as mermaid
```

The CLI forwards whatever bearer token is configured (`AGENTTEAMS_AUTH_TOKEN`
or `AGENTTEAMS_AUTH_TOKEN_FILE`) verbatim, so an L2 human can also use it by
pointing either variable at their own Matrix access token — no separate CLI
auth mode is needed.

### `agt project` (the lifecycle write API write commands)

`agt project` wraps the write endpoints so a human can intervene without
raw curl:

```bash
agt project create --title "New project" --team biz-team --source matrix
agt project pause demo-project-001 --reason "customer review"
agt project resume demo-project-001
agt project replan demo-project-001 --tasks tasks.json   # JSON array file
agt project cancel demo-project-001 demo-project-001-01 --reason "no longer needed"
agt project complete demo-project-001
```

The same bearer-token forwarding applies (Matrix token for L2 humans).

## Worker checkpoint endpoints

The Controller proxies two read-only endpoints of each worker's QwenPaw app
(checkpoint system, QwenPaw ≥ 2.1) so humans and frontends can inspect a
worker's execution timeline — auto snapshots after every response round plus
manual `/checkpoint snapshot`, stored in the worker's `checkpoints/shadow.git`.

| Endpoint | Meaning |
|:--|:--|
| `GET /api/v1/workers/{name}/checkpoints/graph` | Checkpoint graph (nodes with kind/timestamp/query preview, sessions, summary). Optional `?limit=` (1..1000). |
| `GET /api/v1/workers/{name}/checkpoints/status` | `auto_enabled`, `has_checkpoints`, `workspace_dir`. |

- **Scope**: same worker read authorization as `GET /api/v1/workers/{name}`
  — team leaders / L2 humans only see workers in their teams; unknown or
  out-of-scope workers are hidden as `404`.
- **Embedded mode only**: the endpoints proxy the worker's qwenpaw app inside
  the shared docker network. The upstream address is resolved from the
  effective container prefix (`AGENTTEAMS_PROXY_CONTAINER_PREFIX`, or derived
  from `AGENTTEAMS_RESOURCE_PREFIX` when auto-prefixing is enabled; empty when
  disabled — the same value the docker backend uses for container naming) and
  the effective console port resolved through the same system-wins env chain
  used at container creation (the system env always defines
  `AGENTTEAMS_CONSOLE_PORT`, so a conflicting `Worker.spec.env` value is
  discarded and the container always listens on `8088`). In kube mode
  they return `503`.
- **Degradation**: a worker running QwenPaw < 2.1 has no checkpoint router,
  so the upstream `404` is translated to `502` with
  `checkpoint API unavailable (requires QwenPaw 2.1)`.
- Forwarding is fixed-path (graph / status) with a strict query whitelist —
  not a generic reverse proxy.

Error responses:

| Code | Meaning |
|:--|:--|
| `400` | Invalid worker name / unsupported subpath or query parameter / invalid `limit`. |
| `404` | Worker not found / caller does not own it (existence hidden). |
| `502` | Worker app unreachable, pre-2.1 checkpoint API, or upstream error. |
| `503` | Kube mode (no stable worker pod DNS to proxy). |
