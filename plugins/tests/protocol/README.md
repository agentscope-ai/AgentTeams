# Task / project protocol characterization corpus (Phase 0 — S0.2)

Golden fixtures and snapshots for **shared/tasks/** and **shared/projects/** shapes used by AgentTeams project/task protocol.

## Purpose

Before Phase 5 extracts `agentteams_protocol`, this corpus:

1. Documents **on-disk layout** and sample `meta.json` / `plan.md` / `spec.md` / `result.md` shapes.
2. Runs the **CoPaw domain layer** (`copaw/src/copaw_worker/task.py`) through scripted flows and compares outputs to checked-in snapshots.
3. Records **known divergences** between CoPaw hooks domain and **TeamHarness MCP** (`plugins/teamharness/mcp/server.py`) without forcing them to match.

## Two engines (compare, do not conflate)

| Engine | Code path | How this corpus exercises it |
|--------|-----------|--------------------------------|
| **CoPaw domain** | `copaw/src/copaw_worker/task.py` | `run_characterization.py` + `test_characterization.py` — direct `FileSystemTaskStore` + domain functions (same core CoPaw hooks call) |
| **CoPaw hooks** | `copaw/src/copaw_worker/hooks/tools/{taskflow,projectflow}.py` | Covered indirectly via domain; hook-specific tests: `copaw/tests/test_taskflow_tool.py` |
| **TeamHarness MCP** | `plugins/teamharness/mcp/server.py` — `_taskflow`, `_projectflow` | Separate integration tests: `plugins/tests/teamharness/mcp/tools/test-{taskflow,projectflow}.rb`; diffs listed in `known-engine-diffs.json` |

MCP has **no** import of `copaw_worker`; parity is behavioral, not structural. When outputs differ, update `known-engine-diffs.json` — do not change production code in Phase 0.

## Directory layout

```
plugins/tests/protocol/
├── README.md                 # this file
├── known-engine-diffs.json   # documented MCP vs CoPaw domain differences
├── run_characterization.py   # CLI runner (update snapshots / print diff)
├── test_characterization.py  # pytest gate
└── fixtures/
    ├── shapes/               # static samples (documentation + copy sources)
    │   ├── task-meta.sample.json
    │   ├── project-meta.sample.json
    │   ├── plan-dag.sample.md
    │   ├── spec.sample.md
    │   └── result.sample.md
    └── cases/
        └── dag-delegate-flow/
            ├── actions.json           # step script for domain runner
            └── snapshots/
                └── copaw-domain/      # golden JSON snapshots per step
```

### Workspace paths (runtime convention)

```
{workspace}/shared/projects/{projectId}/
  meta.json
  plan.md
  result.md          # Leader-authored project summary (MCP may write earlier)

{workspace}/shared/tasks/{taskId}/
  meta.json
  spec.md            # Leader-written; worker read-only
  result.md          # structured STATUS/SUMMARY/DELIVERABLES protocol
  base/              # optional read-only inputs (exclude on worker push)
  progress/          # optional worker logs
```

## Running

From repo root:

```bash
# Compare domain runner output to golden snapshots (CI gate)
python -m pytest plugins/tests/protocol/test_characterization.py -q

# Regenerate golden snapshots after intentional domain changes (review diff!)
python plugins/tests/protocol/run_characterization.py --update

# Print human-readable diff for one case
python plugins/tests/protocol/run_characterization.py --case dag-delegate-flow
```

TeamHarness MCP (full mc mock + role gates):

```bash
bash plugins/tests/run-integration-tests.sh
# includes test-taskflow.rb and test-projectflow.rb
```

## Adding a case

1. Add `fixtures/cases/<case-name>/actions.json` with a `steps` array.
2. Run `run_characterization.py --update --case <case-name>`.
3. Review generated files under `snapshots/copaw-domain/`.
4. If MCP behavior differs, add an entry to `known-engine-diffs.json` and extend Ruby MCP tests if needed.

## Phase 5+ usage

When extracting `agentteams_protocol`:

- Move domain functions behind the shared package; keep snapshot paths stable or migrate with a one-time `--update`.
- MCP adapter must pass the same corpus steps (add `snapshots/teamharness-mcp/` when dual-run begins).
- Use `AGENTTEAMS_REFACTOR_PROTOCOL_CORE=1` (see `design/remediation-smoke.md`) for staged cutover.
