# Harness Worker for HiClaw

A HiClaw worker runtime that delegates the agent loop to external CLI tools:
- Claude Code (`claude`)
- Gemini CLI (`gemini`)
- OpenCode (`opencode`)
- Codex CLI (`codex`)

## Architecture

See `docs/plan-harness-worker-implementation-for-hiclaw.md` for full details.

Key patterns:
- **Request/response model**: Each Matrix message spawns one CLI invocation
- **Matrix relay**: Built on `matrix-nio` (NOT `hermes_matrix.adapter` subclass)
- **FileSync**: MinIO sync using `mc` CLI (copied from `hermes_worker.sync`)
- **Bridge**: Two-phase (create + overlay) copied from `copaw_worker.bridge`
- **Policies**: Copied verbatim from `hermes_matrix.policies`

## Usage

```bash
harness-worker \
  --name my-worker \
  --fs localhost:9000 \
  --fs-key minioaccess \
  --fs-secret miniosecret \
  --fs-bucket hiclaw-storage \
  --harness-type claude
```

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `HICLAW_WORKER_NAME` | Yes | - | Worker name |
| `HICLAW_FS_ENDPOINT` | Yes | - | MinIO endpoint |
| `HICLAW_FS_ACCESS_KEY` | Yes | - | MinIO access key |
| `HICLAW_FS_SECRET_KEY` | Yes | - | MinIO secret key |
| `HICLAW_FS_BUCKET` | No | `hiclaw-storage` | MinIO bucket |
| `HICLAW_INSTALL_DIR` | No | `/root/hiclaw-fs/agents` | Workspace root |
| `HICLAW_HARNESS_TYPE` | No | `claude` | CLI variant: claude\|gemini\|opencode\|codex |
| `HICLAW_HARNESS_TIMEOUT_MS` | No | `600000` | Per-invocation timeout (ms) |