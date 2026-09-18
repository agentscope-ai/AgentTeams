# AgentTeams Integration Tests

Automated integration test suite that validates all 10 POC acceptance cases.

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
| test-05 | Case 5 | Heartbeat triggers Manager inquiry |
| test-06 | Case 6 | Create Bob, collaborative task |
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
switching to QwenPaw, and `28` checks Manager state migration. Keep building
`copaw-worker` for `23`; retiring CoPaw regression does not retire migration
validation.

The integration matrix now has 7 jobs instead of 10, and controller-shard
script invocations fall from the original 48 to 28. All scenario files remain
available through `--test-filter`. OpenClaw, QwenPaw, and Hermes LLM interaction
coverage stays in place. Controller-only scenarios use their own readiness
checks instead of waiting for an unrelated Manager session to stabilize.
No-LLM runs skip session stabilization entirely.

Run the lightweight scheduling regression checks without containers or keys:

```bash
python3 tests/check-integration-scheduling.py
```

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
