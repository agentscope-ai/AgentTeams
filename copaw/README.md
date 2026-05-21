# copaw-worker

Lightweight [HiClaw](https://github.com/higress-group/hiclaw) Worker runtime based on [CoPaw](https://github.com/agentscope-ai/CoPaw).

## Install

```bash
pip install copaw-worker
```

## Usage

```bash
copaw-worker --name <worker-name> --fs <minio-endpoint> --fs-key <access-key> --fs-secret <secret-key>
```

## Testing

Focused health and sync tests:

```bash
UV_CACHE_DIR=/private/tmp/hiclaw-uv-cache PYTHONPATH=src uv run --no-project --with pytest pytest tests/test_health.py tests/test_worker_health.py tests/test_worker_api.py tests/test_sync.py
```

See [HiClaw worker guide](https://github.com/higress-group/hiclaw/blob/main/docs/worker-guide.md) for full setup instructions.
