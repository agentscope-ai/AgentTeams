# TaskForge contracts — mirror placeholder

Reserved slot for the wire contracts of the TaskForge service (TrackForge,
issue #1508), consumed by the TeamHarness client
(`plugins/teamharness/mcp/lifecycle/taskforge_client.py`).

## Source

- Upstream: TaskForge release repository
- Path: `core/schemas/`

## Files to be mirrored

The following contract files will be mirrored from `core/schemas/`:

| File | Covers |
| --- | --- |
| `project.v1.json` | Project resource (client constant `PROJECTS`) |
| `task.v1.json` | Task resource, actions and sub-resources (`TASKS`, `task_action()`, `task_sub()`) |
| `loop.json` | Loop resource (client constant `LOOPS`) |
| `event.json` | Event shape (client constant `EVENTS`) |

The names track the client's intended path layout and are provisional:
the mirror step copies the authoritative published files as-is, whatever
their final names are. Final file names follow the TaskForge 0.1.0 release.

## Current status

Unpublished placeholder. TaskForge 0.1.0 is not released yet, so this
directory intentionally contains no schema files. Do not hand-write
schemas here — inventing a contract before the release would freeze a
wire surface the TaskForge team has not published.

## Mirror timing

After TaskForge 0.1.0 is released, the release pipeline syncs
`core/schemas/*` into this directory. The bundle copy under
`plugins/agentteams-teamharness/contracts/taskforge/` is re-synced in the
same commit; byte equality is pinned by the compatibility-copy drift
test (`plugins/tests/agentteams-teamharness/golden/
test_compatibility_copy_drift.py`).
