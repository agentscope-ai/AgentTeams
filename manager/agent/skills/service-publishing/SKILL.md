---
name: service-publishing
description: Expose worker HTTP services via Higress gateway. Use when admin asks to publish a worker's web app or API to make it externally accessible.
---

# Service Publishing

## Overview

Expose HTTP services running inside worker containers to the outside world via the Higress gateway. Each exposed port gets an auto-generated domain name.

## How It Works

Add `expose` to a Worker's spec to publish container ports. The controller automatically creates the Higress domain, service source, and route.

**Auto-generated domain pattern:**
```
worker-{name}-{port}-local.agentteams.io
```

Example: worker `alice` exposing port `8080` → `worker-alice-8080-local.agentteams.io`

## Usage

### Via CLI

```bash
# Expose port 8080 for worker alice
agt apply worker --name alice --model qwen3.5-plus --expose 8080

# Expose multiple ports
agt apply worker --name alice --model qwen3.5-plus --expose 8080,3000

# Check exposed ports
agt get worker alice
# Look for status.exposedPorts in the output

# Remove exposed ports (update without --expose)
agt apply worker --name alice --model qwen3.5-plus
```

### Via YAML

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: qwen3.5-plus
  expose:
    - port: 8080
    - port: 3000
```

Apply with:
```bash
agt apply -f worker.yaml
```

### Workers referenced by a Team

Configure `expose` on the Worker CR, then reference that Worker from the Team:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: lead
spec:
  model: qwen3.5-plus
---
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: backend
spec:
  model: qwen3.5-plus
  expose:
    - port: 8080
---
apiVersion: agentteams.io/v1beta1
kind: Team
metadata:
  name: dev-team
spec:
  workerMembers:
    - name: lead
      role: team_leader
    - name: backend
      role: worker
```

## Important Notes

- The worker container must be running and the service must be listening on the specified port before it can be accessed
- Domains are auto-generated; custom domains are not yet supported
- No authentication is configured on exposed routes (public access)
- Docker DNS resolves the worker container name (`agentteams-worker-{name}`) automatically within `agentteams-net`
- To stop exposing a port, remove it from the `expose` list and re-apply
