# AgentTeams Overview

English | [中文](zh-cn/overview.md)

AgentTeams is a multi-agent collaboration system with humans in the loop. You interact with a Manager through a familiar instant-messaging interface. The Manager can create and organize Workers with different capabilities, break down tasks, track progress, and coordinate multiple agents in shared context.

AgentTeams uses Matrix for communication between humans and agents, Higress as the unified gateway for model and MCP traffic, and shared object storage for agent configuration, task context, and artifacts. A controller manages the creation, update, and deletion of agents.

## When to use AgentTeams

AgentTeams is a good fit when you want to:

- Coordinate specialized Workers for frontend, backend, testing, research, or other roles.
- Let a Manager break down and track longer-running project work.
- Add requirements, inspect progress, or take over while agents are working.
- Control access to models, MCP tools, and external credentials through a unified gateway.
- Start on a local machine and later deploy a shared instance on Kubernetes.

If you only need a one-off conversation with a single agent and do not need role separation, shared task space, or human oversight, a standalone agent tool is usually simpler.

## Core roles

| Role | Responsibility |
|---|---|
| **Human** | Sets goals through a Matrix client, observes the full collaboration, and can intervene at any time. |
| **Manager** | Understands goals, creates or selects Workers, delegates work, tracks progress, and consolidates results. |
| **Worker** | Executes focused tasks. Each Worker can have its own role, model, runtime, Skills, and MCP configuration. |
| **Team** | Packages multiple Workers and a Team Leader into a reusable collaboration unit. |
| **Team Leader** | Coordinates Team members, maintains team context, and moves collaborative work forward. |

## System components

| Component | Purpose |
|---|---|
| **agentteams-controller** | Manages Worker, Manager, Team, and Human resources, agent lifecycles, and related infrastructure configuration. |
| **Matrix / Tuwunel** | Carries visible communication among Humans, Managers, Workers, and Team Leaders. |
| **Element Web** | The default Matrix web client. Other compatible Matrix clients can also be used. |
| **Higress** | Acts as the AI/API gateway for LLM and MCP traffic, identity, and access control. |
| **MinIO or compatible object storage** | Stores agent workspaces, configuration, shared task context, and artifacts. |
| **Manager and Worker containers/Pods** | Run the agent runtimes. They are separated from infrastructure and can be created or replaced on demand. |

See [Architecture](design/architecture.md) for detailed component relationships and data flows.

## How collaboration works

1. A Human describes a goal to the Manager in Matrix.
2. The Manager decides whether to use an existing Worker, create a Worker, or organize a Team.
3. The controller prepares the agent's Matrix identity, gateway permissions, shared storage configuration, and runtime environment.
4. The Manager delegates work to a Worker or Team and tracks progress in Matrix rooms.
5. Workers use models, Skills, and authorized MCP tools, and write shared context and artifacts to object storage.
6. The Human can add requirements, correct the direction, or approve decisions during execution.
7. The Manager consolidates the results and reports back to the Human.

All important communication happens in Matrix rooms, so humans can see how work is decomposed, delegated, and completed.

## Two deployment modes

| | Local deployment | Kubernetes deployment |
|---|---|---|
| Best for | Personal evaluation, development, and single-machine use | Shared teams, long-running environments, and production deployments |
| Infrastructure | One embedded controller container runs Higress, Tuwunel, MinIO, Element Web, and the controller | Components run as Kubernetes workloads or external services |
| Agent runtime | Manager and Workers run in separate containers | Manager and Workers run in separate Pods |
| Lifecycle management | The controller manages agents through Docker or Podman | The controller manages agents through Kubernetes CRDs |
| Installation entry point | `install/agentteams-install.sh` | The `helm/agentteams` Helm chart |

For a first evaluation, start with the local path in [Quickstart](quickstart.md). For a shared or production environment, see the [Kubernetes Deployment Guide](usage/deployment/kubernetes.md).

## Agent runtimes

Manager currently supports:

- **CoPaw**: the current QwenPaw-based Python Manager implementation; the canonical value is `qwenpaw`, while the local installer still uses `copaw` as a compatibility alias.
- **OpenClaw**: a Node.js runtime.

Worker resources support the following runtimes. Available images depend on the installation method and version:

- **OpenClaw**
- **CoPaw**
- **QwenPaw**
- **Hermes**

The Controller and Helm values already contain an OpenHuman backend and image configuration, but the shipped Worker CRD enum does not yet accept an explicit `spec.runtime: openhuman`. Until a separate business-code change aligns that contract, do not treat OpenHuman as a directly declarable Worker runtime.

The runtime selects the agent framework and image, while AgentTeams manages the Worker's identity, Matrix rooms, and persistent data. Switching runtimes normally recreates the Worker environment and should not be done while the Worker is executing a task.

## Resources and management interfaces

AgentTeams models the system through four declarative resources: `Worker`, `Manager`, `Team`, and `Human`. You can manage them by:

- Sending natural-language requests to the Manager in Matrix.
- Using the `agt` CLI inside the controller or Manager container.
- Applying YAML manifests with `agt apply -f`.
- Managing AgentTeams CRDs directly on Kubernetes.

See [Declarative Resource Management](usage/resource-management.md) for fields and operations.

## Security model

- Workers do not need direct access to real model or MCP service keys.
- Higress uses separate identities and consumer credentials to control model and tool access.
- Matrix rooms retain a visible collaboration timeline for Humans, Managers, and Workers.
- Agent configuration and persistent data live in centralized object storage, so Worker environments can be replaced.
- Kubernetes deployments can integrate with existing gateways, object storage, and credential providers.

Production deployments should also use HTTPS, network policies, least-privilege access, Secret management, backups, and auditing.

## Where to go next

| Goal | Document |
|---|---|
| Complete the first task locally | [Quickstart](quickstart.md) |
| Explore reusable multi-agent workflows | [AgentTeams Use Cases](usage/use-cases.md) |
| Review every local installation option | [Local Deployment Guide](usage/deployment/local.md) |
| Deploy a shared instance on Kubernetes | [Kubernetes Deployment Guide](usage/deployment/kubernetes.md) |
| Understand components and communication | [Architecture](design/architecture.md) |
| Configure and operate the Manager | [Manager Guide](usage/manager-guide.md) |
| Create, deploy, and maintain Workers | [Worker Guide](usage/worker-guide.md) |
| Install Skills on an existing Worker | [Worker Guide: Installing Skills](usage/worker-guide.md#installing-skills-on-a-worker) |
| Manage resources with YAML and `agt` | [Declarative Resource Management](usage/resource-management.md) |
| Install on Windows | [Windows Deployment](usage/deployment/windows.md) |
| Troubleshoot common problems | [FAQ](usage/troubleshooting/faq.md) |
| Contribute to the project | [Development Guide](usage/development.md) |
