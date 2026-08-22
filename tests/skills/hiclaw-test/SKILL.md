---
name: hiclaw-test
description: "Complete HiClaw test cycle including installation, uninstallation, running tests, and exporting debug logs for analysis. Use for (1) verifying HiClaw functionality (2) CI/CD test validation (3) issue diagnosis and debugging (4) pre-merge testing. Trigger words: test HiClaw, run HiClaw tests, hiclaw test, make test, verify HiClaw installation."
---

# HiClaw Test Cycle

Complete HiClaw testing workflow including installation verification, functional tests, and issue diagnosis.

## Quick Start

```bash
# 1. Clone/update code
git clone https://github.com/alibaba/hiclaw.git && cd hiclaw

# 2. Create config file (first time)
cp hiclaw-manager.env.example ~/hiclaw-manager.env
# Edit ~/hiclaw-manager.env and set HICLAW_LLM_API_KEY, etc.

# 3. Run full test
set -a && . ~/hiclaw-manager.env && set +a && make test
```

## Full Test Cycle

### Step 1: Prepare Environment

```bash
# Clone latest code
git clone https://github.com/alibaba/hiclaw.git
cd hiclaw

# Check if config file exists
ls ~/hiclaw-manager.env
```

### Step 2: Run Full Test

```bash
# Load config and run tests (automatically executes install → test → uninstall)
set -a && . ~/hiclaw-manager.env && set +a && make test
```

Test cases:
- **test-01**: Manager startup health check
- **test-02**: Create Worker Alice
- **test-03**: Assign task to Worker
- **test-04**: Human intervention with additional instructions
- **test-05**: Heartbeat query mechanism
- **test-06**: Multi-Worker collaboration
- **test-08~14**: GitHub/MCP related tests (requires HICLAW_GITHUB_TOKEN)

### Step 3: Individual Install/Uninstall

```bash
# Install only
set -a && . ~/hiclaw-manager.env && set +a && HICLAW_YOLO=1 make install

# Uninstall only
make uninstall

# Run tests using existing installation (skip reinstall)
set -a && . ~/hiclaw-manager.env && set +a
./tests/run-all-tests.sh --skip-build --use-existing
```

## Export Debug Logs

When tests fail or hang, use `hiclaw-debug.sh` to export logs:

```bash
# In hiclaw repository directory
./tests/skills/hiclaw-test/scripts/hiclaw-debug.sh all

# Analyze hang issues only
./tests/skills/hiclaw-test/scripts/hiclaw-debug.sh analyze
```

### Manual Log Export

```bash
# Manager container logs
docker logs --tail 100 hiclaw-manager 2>&1

# Manager Agent logs
docker exec hiclaw-manager tail -100 /var/log/hiclaw/manager-agent.log

# Manager Agent error logs
docker exec hiclaw-manager tail -50 /var/log/hiclaw/manager-agent-error.log

# Worker container logs
docker ps --filter "name=hiclaw-worker" --format "table {{.Names}}\t{{.Status}}"
docker logs --tail 50 hiclaw-worker-alice 2>&1

# Test output files
ls tests/output/
cat tests/output/metrics-*.json
```

## Common Issue Diagnosis

### 1. Test Hangs

Use `hiclaw-debug.sh` to analyze PHASE_DONE messages for mention issues:

```bash
# Run in HiClaw repository directory
./tests/skills/hiclaw-test/scripts/hiclaw-debug.sh analyze 1h

# Or use export-debug-log.py directly
python3 scripts/export-debug-log.py --range 1h
```

`hiclaw-debug.sh` checks if Worker's PHASE_DONE messages include `@manager`:
- ✅ Includes `@manager` → Message will be processed by Manager
- ⚠️ Missing `@manager` → Message ignored, may cause hang

**Common cause**: In multi-phase collaboration projects, Worker doesn't @mention Manager after completing a phase

**Solution**: Fixed in v1.0.8+, Manager adds Multi-Phase Collaboration Protocol to task specs

### 2. Worker Not Responding

```bash
# Check if Worker container is running
docker ps --filter "name=hiclaw-worker"

# Check Worker Agent process
docker exec hiclaw-worker-alice ps aux | grep openclaw
```

### 3. LLM Call Failures

```bash
# Check error logs
docker exec hiclaw-manager grep -i "error\|fail" /var/log/hiclaw/manager-agent-error.log
```

### 4. Test Timeout

Some tests (like test-14-git-collab) take longer, you can increase timeout:

```bash
# Run test script directly with custom timeout
timeout 1200 ./tests/run-all-tests.sh --skip-build --use-existing
```

## Test Results Interpretation

### Successful Test

```
========================================
  Test Summary
========================================
  Total:  12
  [32mPassed: 12[0m
  [31mFailed: 0[0m
========================================
```

### Skipped Tests

```
[36m[TEST INFO][0m SKIP: No GitHub token configured
```

Requires `HICLAW_GITHUB_TOKEN` environment variable.

### Metrics Files

Each test generates `metrics-XX-testname.json` containing:
- LLM call count
- Token usage
- Execution time
- Cache hit statistics

## Cleanup Environment

```bash
# Full uninstall
make uninstall

# Delete all Worker containers
docker rm -f $(docker ps -aq --filter "name=hiclaw-worker")

# Delete test code
rm -rf ./hiclaw
```

## References

- [tests/README.md](https://github.com/alibaba/hiclaw/blob/main/tests/README.md) - Test framework documentation
- [install/README.md](https://github.com/alibaba/hiclaw/blob/main/install/README.md) - Installation guide
- [references/troubleshooting.md](references/troubleshooting.md) - Detailed troubleshooting
