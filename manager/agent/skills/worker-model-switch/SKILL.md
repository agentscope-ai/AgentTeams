---
name: worker-model-switch
description: Switch a Worker Agent's LLM model via hiclaw CLI. Use when the human admin requests changing a Worker's model.
---

# Worker Model Switch

Switch a Worker's LLM model using the hiclaw CLI. The controller handles all details: model parameter resolution, openclaw.json generation, storage push, and container recreation.

## Usage

```bash
agt update worker --name <WORKER_NAME> --model <MODEL_ID>
```

Examples:
```bash
agt update worker --name alice --model claude-sonnet-4-6
agt update worker --name alice --model deepseek-chat
```

## What happens

The controller automatically:
1. Updates the Worker CR's `spec.model`
2. Resolves model parameters (contextWindow, maxTokens, reasoning, input modalities) from its built-in model registry
3. Regenerates `openclaw.json` and pushes it to storage
4. Recreates the Worker container

You do NOT need to specify context window, reasoning, or any model parameters — the controller knows them.

## On success

The CLI prints `worker/<name> configured`. The controller reconciles the change automatically. No manual restart is needed.

## On failure

Report the CLI error output to the human admin. Common causes:
- Worker name does not exist
- Controller API is unreachable

## Unknown models

If the admin requests a model not in the controller's built-in registry, the controller uses safe defaults (contextWindow=150000, maxTokens=128000, reasoning=true, input=["text"]). If the admin needs specific parameters for an unknown model, they should update the controller's model registry rather than overriding at the skill level.
