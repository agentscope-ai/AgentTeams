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
- A model API key is optional during installation. You can add it in Dashboard before your first conversation.
- Local ports `18080`, `18001`, and `18088` are available. The Dashboard also uses port `13000` when enabled by default.

> The default command starts Dashboard without configuration prompts. If the container engine is missing or stopped, the installer guides you to install or start it and waits before continuing. Use `--manual` to configure installation options in the terminal.

## Step 1: Install AgentTeams

Run the installer in a terminal:

```bash
bash <(curl -sSL https://agentteams.cloud/install.sh)
```

With no explicit version, the Bash installer checks the recommended Manager, embedded controller and Dashboard images against your container engine's architecture before saving configuration. Worker images are pulled when you create Workers. Missing image tags or architectures trigger a check of the bundled fallback; connection or authentication errors stop installation. Upgrades never fall back automatically. Dashboard and DeepSeek Harness keep their independent image versions.

Wait for Dashboard to open. The installer copies the login password to your clipboard and prefills the username as `admin`; paste the password and sign in. If the browser or clipboard is unavailable, use the URL and credentials printed in the terminal.

If a model has not been configured, open the Manager conversation and click **Configure model**. Save and verify your API key and model in model management, then return to the conversation. You can also export `AGENTTEAMS_LLM_API_KEY` before installation; for a custom endpoint, set `AGENTTEAMS_LLM_PROVIDER=openai-compat`, `AGENTTEAMS_OPENAI_BASE_URL` and `AGENTTEAMS_DEFAULT_MODEL` as well.

For OpenAI-compatible services, the Base URL commonly includes `/v1`; follow the requirements of your provider.

After a successful installation, the terminal prints:

- The Dashboard login URL (and an optional Element Web URL).
- The administrator username and password.
- The Higress Console URL.
- The Dashboard URL, when enabled.
- The locations of the configuration file, data volume, and Manager workspace.

Save the login information before continuing.

To upgrade later, run the same command again and press Enter at the single confirmation. The installer retains your accounts, model configuration, chat history, data and workspaces, backs up the original configuration, and prepares images before replacing services. Existing Worker containers stay in place during normal embedded-stack upgrades. If existing data cannot be matched to its configuration, installation stops without cleaning it up. See the [upgrade instructions](../install/README.md#upgrade-an-existing-local-installation) for Dashboard version handling and the separate backup/rollback workflow.

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
http://127.0.0.1:13000/
```

Sign in with the administrator username and password printed by the installer. You should see a conversation or room for the Manager.

If the page does not load or the Manager is not ready, inspect the logs:

```bash
docker logs --tail 200 agentteams-controller
docker logs --tail 200 agentteams-manager
```

See the [FAQ](usage/troubleshooting/faq.md) for more troubleshooting steps.

## Step 3: Create your first Worker

Open the Manager conversation in Dashboard and send:

> Create a Worker named alice for Python development and code testing.

The Manager will ask for or confirm the Worker's role, model, runtime, and Skills based on the current configuration. For a first evaluation, accept the recommended options. The selected runtime's image is downloaded when first needed, which can take longer on the first launch.

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

Wait until `alice` reaches `Running`, and confirm that its conversation appears in Dashboard.

## Step 4: Complete your first task

Open Alice's conversation in Dashboard and send:

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
- [ ] You can sign in to Dashboard and talk to the Manager.
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
bash <(curl -fsSL https://agentteams.cloud/install.sh) uninstall
```
