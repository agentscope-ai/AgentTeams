# AgentTeams Integration Tests

Integration scenarios for the current runtime, Task protocol, and migration paths.
Historical POC/GitHub scenarios remain available for manual runs; the
CI filters below define the maintained regression matrix.

## Architecture

Tests simulate human interaction by calling the Matrix API directly, then verify system responses and side effects:

```
Test Script                     AgentTeams System
    |                               |
    ├── Matrix API: send message ──>| Manager Agent processes
    |                               │ (creates Worker, assigns task, etc.)
    ├── poll Matrix API for reply <─|
    ├── verify reply content        |
    ├── verify Higress Console ────>| (Consumer created? Route updated?)
    ├── verify MinIO files ────────>| (SOUL.md written? task/spec.md?)
    └── PASS / FAIL                 |
```

## Test Cases

| Test | POC Case | Description |
|------|----------|-------------|
| test-01 | Case 1 | Manager boot, all services healthy, IM login |
| test-02 | Case 2 | Create Worker Alice via Matrix conversation |
| test-03 | Case 3 | Assign task, Worker completes |
| test-04 | Case 4 | Human intervenes with supplementary instructions |
| test-05 | Case 5 | On-demand heartbeat checklist and final report |
| test-06 | Case 6 | Create Bob and verify Worker identity isolation |
| test-07 | Case 7 | Credential smooth rotation (TODO) |
| test-08 | Case 8 | GitHub operations via MCP Server |
| test-09 | Case 9 | Multi-Worker GitHub collaboration |
| test-10 | Case 10 | MCP permission dynamic revoke/restore |
| test-11 | Feature | Multi-round GitHub PR collaboration |

## CI coverage and duplicate execution

The CI matrix keeps runtime-dependent worker checks (`15`, `17`–`20`, `22`,
`24`) on OpenClaw, QwenPaw, and Hermes. Similar setup does not make
these scenarios redundant: they verify different import/update paths,
runtime-consumed files, team permissions, skills, and deletion behavior.

| Scenarios | CI placement | Reason |
|-----------|--------------|--------|
| `23` runtime switch | QwenPaw controller shard | The scenario explicitly switches OpenClaw → CoPaw → QwenPaw; the matrix worker default does not change its coverage. |
| `25` name validation | QwenPaw controller shard | Shared `agt` validation and Higress leak checks do not depend on worker runtime. |
| `27`, `28` Manager startup/migration | QwenPaw controller shard | The QwenPaw + Hermes entry uses the same Manager image and adds no Manager coverage. |
| `100` cleanup | Each controller shard, last | Each installation has its own resources to verify and clean up. |

CoPaw standalone interaction, controller, and legacy Team DAG shards are
retired as CoPaw moves to QwenPaw. QwenPaw team orchestration runs in `26`;
the legacy `21` scenario directly imports CoPaw tools and remains available
for manual diagnostics against a CoPaw installation. It is no longer in the
CI matrix or the default release-baseline filter.

Migration coverage remains: `23` exercises an actual CoPaw worker before
switching to QwenPaw, `28` checks Manager state migration, and `29` uses the
pinned published CoPaw image for retained-data upgrade coverage. An old
migration input is intentional and must not be removed as an obsolete test.

The integration matrix now has 7 jobs instead of 10, and controller-shard
script invocations fall from the original 48 to 29. All scenario files remain
available through `--test-filter`. OpenClaw, QwenPaw, and Hermes LLM interaction
coverage stays in place. Controller-only scenarios use their own readiness
checks instead of waiting for an unrelated Manager session to stabilize.
No-LLM runs skip session stabilization entirely.

Run the lightweight scheduling regression checks without containers or keys:

```bash
python3 tests/check-integration-scheduling.py
python3 tests/test-ci-interaction.py
```

### Current-mode case audit

| Cases | Maintained behavior |
|-------|---------------------|
| `03` | Keep the standalone Worker request, registered room/mention check, and current-run shared artifact marker. It does not need a Project or a main Task. |
| `04` | Wait for the Worker's explicit paused-request acknowledgement, then send the human supplement. Execute the delivered Python artifact to verify both original and supplementary requirements. No fixed 20/60-second sleeps or nonempty-history PASS. |
| `05` | Request the current heartbeat checklist explicitly and require this run's final report marker on every Manager runtime. Remove unrelated WebAssembly research and fixed 30/60-second sleeps. This checks checklist execution, not periodic scheduler timing. |
| `06` | Keep conversational creation of a second Worker, runtime, gateway, workspace and distinct Matrix identities. Retire the REST API/`Project:` room search, 300-second search plus 60-second fallback, completion-keyword scan, and directory-list-only acceptance. Team delegation is checked by `26`. |
| `26` | Keep QwenPaw plugin/package/role/hot-update checks. Replace `projectflow create_project/plan_dag`, manual Task IDs, and `taskflow`/storage paths with public Task creation, planning, begin/submit, independent acceptance and completion. The driver registers a real human Matrix event through `agt task create`; real Leader/Worker models perform the work. Require the accepted immutable file content and its native Matrix attachment related to the final report in the original conversation. This is not a test of model-driven Manager registration. |
| `08`, `09`, `11`, `13`, `14`, `21` | Historical GitHub/Project/CoPaw flows are outside the seven-job CI matrix and release-baseline filter. They retain legacy waits/protocols and are not current Task certification. Use an explicit `--test-filter` for compatibility diagnostics; a local run without a filter still discovers all numbered scripts. Do not add these cases back to default CI unchanged. |
| `15`, `17`–`20`, `22`–`25`, `27`–`29`, `100` | Keep controller, permissions, runtime-switch, startup, migration and cleanup coverage. Short condition-poll intervals are not blind business-task sleeps. |

Session JSONL stabilization is only an optional metrics flush. Unknown or
unsupported runtimes, missing files and empty observations skip that wait;
they do not prove agent readiness or zero token usage. Nonempty supported
observations retain a bounded flush wait. Functional PASS comes from each
scenario's own acknowledgement, artifact or public Task/delivery assertions.

For context, the OpenClaw shard in run `45297881675` took 36m05s, including
five exhausted Worker metrics waits (10m13s total) and roughly six minutes
searching for an obsolete Project room in `06`. These are observations of the
old run, not a measured speedup of this revision. Compare hosted test-step
times separately from image preparation, runner queue time and runner minutes.

## Running Tests

### Via Makefile (Recommended)

```bash
# Full test flow (auto-creates and cleans up test container)
AGENTTEAMS_LLM_API_KEY=sk-xxx make test

# Skip image rebuild
make test SKIP_BUILD=1

# Run specific tests
make test TEST_FILTER="01 02"

# Test an existing Manager installation
make test SKIP_INSTALL=1
```

### Direct Script Execution

```bash
# Build + run all tests
./tests/run-all-tests.sh

# Use existing images
./tests/run-all-tests.sh --skip-build

# Run specific tests only
./tests/run-all-tests.sh --test-filter "01 02 03"

# Run against an already-installed Manager
./tests/run-all-tests.sh --use-existing

# Use a custom container name
./tests/run-all-tests.sh --container my-test-container
```

## Required Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `AGENTTEAMS_LLM_API_KEY` | Yes | LLM API key for Agent behavior |
| `AGENTTEAMS_GITHUB_TOKEN` | No | GitHub PAT for tests 08-11 |

## Helper Libraries

- `lib/test-helpers.sh`: Assertions, lifecycle, logging, Docker helpers
- `lib/matrix-client.sh`: Matrix API wrapper (register, login, send/read messages)
- `lib/higress-client.sh`: Higress Console API wrapper (consumers, routes, MCP)
- `lib/minio-client.sh`: MinIO verification (file existence, content, listing)

### Website installer contract fixtures

The Bash and PowerShell installers are maintained only in the website repository. Before running the installer contract tests locally, check out the same revision used by `.github/workflows/helm-lint.yml` into `.installer-source/`:

```bash
git clone https://github.com/agentteams-group/agentteams-group.github.io.git .installer-source
git -C .installer-source checkout 8fef1a33f77547a2144e001d3287696ae6fcc3b0
```

This checkout is test input only, ignored by Git, and is never used as an installation wrapper. Runtime integration installation uses the website endpoint directly.
