# MCP Server YAML templates

Operational YAML configs for `setup-mcp-server.sh` and install-time Higress bootstrap (`setup-higress.sh`).

Copied into Manager images at `/opt/hiclaw/configs/mcp-templates/`.

Usage:

```bash
bash .../setup-mcp-server.sh github "<token>"
# or explicit template:
bash .../setup-mcp-server.sh github "<token>" --template /opt/hiclaw/configs/mcp-templates/mcp-github.yaml
```

These files are **not** agent-facing skill references — keep large API/tool definition blobs out of `manager/agent/skills/`.
