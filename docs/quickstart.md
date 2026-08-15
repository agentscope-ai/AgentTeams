# AgentTeams Quickstart

English | [中文](zh-cn/quickstart.md)

This guide follows the recommended local deployment path. You will install AgentTeams, create your first Worker, and complete a task that demonstrates human intervention. Dedicated guides cover detailed configuration and Kubernetes deployment; after this guide, continue with [AgentTeams Use Cases](usage/use-cases.md).

## What you will have

- An AgentTeams instance running on your local machine.
- A Manager you can talk to through Element Web.
- A Worker created and managed by the Manager.
- A visible task history involving a Human, the Manager, and the Worker.

## Prerequisites

- macOS or Linux. Windows users should follow the [Windows Deployment Guide](usage/deployment/windows.md).
- Docker Desktop, Docker Engine, or a compatible Podman environment is running.
- At least 2 CPU cores and 4 GB of available memory. Use 4 CPU cores and 8 GB when running multiple Workers.
- A working LLM API key. The installer supports Alibaba Cloud Model Studio/Qwen and OpenAI-compatible services.
- Local ports `18080`, `18001`, and `18088` are available. The Dashboard also uses port `13000` when enabled by default.

> For a first evaluation, use the installer's Quick Start mode. Choose Manual Setup when you need a custom model endpoint, external access, ports, persistence, runtimes, or images.

## Step 1: Install AgentTeams

Run the installer in a terminal:

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

Follow the prompts:

1. Select English.
2. Choose Quick Start or Manual Setup.
3. Enter the LLM API key. For another OpenAI-compatible service in Manual Setup, also enter its Base URL and model ID.
4. Wait for the model connectivity check and container startup to complete.

For OpenAI-compatible services, the Base URL commonly includes `/v1`; follow the requirements of your provider.

After a successful installation, the terminal prints:

- The Element Web login URL.
- The administrator username and password.
- The Higress Console URL.
- The Dashboard URL, when enabled.
- The locations of the configuration file, data volume, and Manager workspace.

Save the login information before continuing.

## Step 2: Verify and sign in

Check the main containers:

```bash
docker ps --filter name=agentteams-controller
docker ps --filter name=agentteams-manager
```

You can also check the Manager through `agt`:

```bash
docker exec agentteams-controller agt get managers
```

By default, open this URL in a browser:

```text
http://127.0.0.1:18088/#/login
```

Sign in with the administrator username and password printed by the installer. You should see a conversation or room for the Manager.

If the page does not load or the Manager is not ready, inspect the logs:

```bash
docker logs --tail 200 agentteams-controller
docker logs --tail 200 agentteams-manager
```

See the [FAQ](usage/troubleshooting/faq.md) for more troubleshooting steps.

## Step 3: Create your first Worker

Open a direct message with the Manager in Element Web and send:

> Create a Worker named alice for Python development and code testing.

The Manager will ask for or confirm the Worker's role, model, runtime, and Skills based on the current configuration. For a first evaluation, accept the recommended options. If you choose a runtime, prefer one whose default image was prepared during installation.

Provisioning normally takes several dozen seconds. The Manager asks the controller to:

1. Create the Worker resource and Matrix identity.
2. Prepare gateway permissions and shared storage configuration.
3. Start a separate Worker container.
4. Create a Matrix room containing the Human, Manager, and Worker.

Check the status from a terminal:

```bash
docker exec agentteams-controller agt get workers
docker ps --filter name=agentteams-worker
```

Wait until `alice` reaches `Running`, and confirm that its room appears in Element Web.

## Step 4: Complete your first task

Open Alice's Matrix room and send:

> Create a Python command-line program that accepts a name and prints a greeting. Include a README and basic tests. When finished, explain which files you created and how to run the tests.

You can watch task delegation, progress, and results in the room. When Alice finishes, confirm that the response includes:

- The implementation file.
- A README or usage instructions.
- Tests and their results.
- The artifact location or retrieval instructions.

## Step 5: Try human intervention

While the Worker is still running, add another requirement:

> Additional requirement: when no name is provided, default to `World`, and add a test for that branch.

Confirm that the Worker understands the additional requirement and covers both the original task and the new requirement in the final result. This is the Human-in-the-loop workflow in AgentTeams: people can observe collaboration and correct the direction before a task finishes.

## Completion checklist

- [ ] `agentteams-controller` and `agentteams-manager` are running.
- [ ] You can sign in to Element Web and talk to the Manager.
- [ ] Alice is in the `Running` state.
- [ ] Alice's Matrix room is visible to the Human.
- [ ] The Worker returned the implementation, instructions, and test results.
- [ ] The Worker incorporated a requirement added during execution.

After these checks pass, you have completed the smallest end-to-end AgentTeams workflow.

## Next steps

- Read the [AgentTeams Overview](overview.md) for the roles, components, and deployment modes.
- Try software delivery, research, localization, incident analysis, long-running collaboration, and adding and using a custom Skill in [AgentTeams Use Cases](usage/use-cases.md).
- Use the [Local Deployment Guide](usage/deployment/local.md) for model, port, domain, storage, runtime, and automated installation options.
- Continue with the [Manager Guide](usage/manager-guide.md) and [Worker Guide](usage/worker-guide.md).
- Use [Declarative Resource Management](usage/resource-management.md) to learn the `agt` CLI and YAML workflows.
- Read [Architecture](design/architecture.md) for Matrix, Higress, object storage, and controller details.
- Use the [Kubernetes Deployment Guide](usage/deployment/kubernetes.md) to create a shared instance in a cluster.

## Uninstall

The following command removes AgentTeams containers, networks, data volumes, configuration files, and the local workspace. Run it only after confirming that you no longer need the data:

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh) uninstall
```
