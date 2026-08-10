# QwenPaw Console Management

Browser-based dashboard for QwenPaw Workers — view status, logs, and configuration.

QwenPaw Workers are created **without** the console by default to save ~500MB RAM. Enable on demand when admin asks to "open console", "debug the worker", "access the worker shell", etc.

## Constraints

- Only local QwenPaw containers support this
- Controller-managed workers that do not expose a Manager-local container, and openclaw workers, do NOT support this console
- Not available on cloud (SAE) — use SAE console or SLS logs instead

## Commands

```bash
# Enable — recreates container with console; result JSON contains console_host_port
bash /opt/agentteams/agent/skills/worker-management/scripts/enable-worker-console.sh --name <NAME>

# Disable — recreates container without console, frees ~500MB RAM
bash /opt/agentteams/agent/skills/worker-management/scripts/enable-worker-console.sh --name <NAME> --action disable
```

After enabling, read `console_host_port` from the JSON result and report: `http://<manager-host>:<port>`. Remind admin to disable when done to reclaim memory.
