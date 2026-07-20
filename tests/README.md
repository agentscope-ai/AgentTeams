# HiClaw Integration Tests

Automated integration test suite that validates all 10 POC acceptance cases.

## Architecture

Tests simulate human interaction by calling the Matrix API directly, then verify system responses and side effects:

```
Test Script                     HiClaw System
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

## Running Tests

### Via Makefile (Recommended)

```bash
# Full test flow (auto-creates and cleans up test container)
HICLAW_LLM_API_KEY=sk-xxx make test

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
| `HICLAW_LLM_API_KEY` | Yes | LLM API key for Agent behavior |
| `AGENTTEAMS_GITHUB_TOKEN` | No | GitHub PAT for tests 08-13 (`HICLAW_GITHUB_TOKEN` remains supported) |

## Helper Libraries

- `lib/test-helpers.sh`: Assertions, lifecycle, logging, Docker helpers
- `lib/matrix-client.sh`: Matrix API wrapper (register, login, send/read messages)
- `lib/higress-client.sh`: Higress Console API wrapper (consumers, routes, MCP)
- `lib/minio-client.sh`: MinIO verification (file existence, content, listing)
