# HiClaw Troubleshooting Guide

## Test Hang Issues

### Symptoms: Test timeout, no failure but doesn't complete

### Diagnosis Steps

1. Check if Manager is waiting for a signal:
```bash
docker exec hiclaw-manager grep "Waiting for" /var/log/hiclaw/manager-agent.log | tail -10
```

2. Check if Worker messages are being processed:
```bash
docker exec hiclaw-manager grep -E "PHASE[0-9]_DONE|REVISION_NEEDED" /var/log/hiclaw/manager-agent.log
```

3. Key: Check `wasMentioned` status:
```bash
docker exec hiclaw-manager grep "resolveMentions (inbound)" /var/log/hiclaw/manager-agent.log | tail -20
```

### Common Causes

#### 1. Worker Not @mentioning Manager (Fixed)

**Symptoms**:
```
resolveMentions (inbound): wasMentioned=false text="PHASE2_DONE..."
```

**Cause**: Worker sends message in project room without @mentioning Manager, causing Manager to ignore the message.

**Solution** (Fixed in v1.0.8+):
- Manager automatically adds "Multi-Phase Collaboration Protocol" when assigning multi-phase tasks
- Worker AGENTS.md includes standalone gotcha explaining phase completion must @mention

#### 2. LLM Response Timeout

**Symptoms**:
```
[tools] exec failed: timeout
```

**Solution**: Increase LLM timeout or check API availability

#### 3. Container Resource Insufficient

**Symptoms**:
```
OOMKilled
```

**Solution**: Increase Docker memory limit

## Installation Issues

### Container Startup Failure

```bash
# Check container logs
docker logs hiclaw-manager

# Check port conflicts
netstat -tlnp | grep -E "18080|18088|18001"
```

### Image Build Failure

```bash
# Clear Docker cache and rebuild
docker builder prune
make build
```

## Worker Issues

### Worker Container Won't Start

```bash
# Check Worker image
docker images | grep hiclaw

# Manual startup test
docker run --rm -it hiclaw/worker-agent:latest /bin/bash
```

### Worker Cannot Connect to Matrix

```bash
# Check Worker's Matrix credentials (container path is fixed)
docker exec hiclaw-worker-alice cat /root/.openclaw/channels/matrix/credentials.json
```

## Test Skip Issues

### GitHub Tests Skipped

Need to set `HICLAW_GITHUB_TOKEN`:
```bash
export HICLAW_GITHUB_TOKEN="ghp_xxx"
make test
```

## Performance Optimization

### Tests Running Too Slow

1. Use `--skip-build` to skip image building
2. Use `--use-existing` to use existing installation
3. Use `--test-filter` to run specific tests only

```bash
./tests/run-all-tests.sh --skip-build --use-existing --test-filter "01 02 03"
```

### High LLM Token Consumption

Check metrics files for token consumption per test:
```bash
# In repository directory
cat tests/output/metrics-*.json | jq '.totals.tokens'
```

## Quick Diagnosis Commands

```bash
# Export all logs (run from skill directory)
./tests/skills/hiclaw-test/scripts/hiclaw-debug.sh all

# Analyze hang issues only
./tests/skills/hiclaw-test/scripts/hiclaw-debug.sh analyze

# View Manager container status
docker ps --filter "name=hiclaw"

# Quick view recent errors
docker exec hiclaw-manager tail -50 /var/log/hiclaw/manager-agent-error.log
```
