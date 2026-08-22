---
name: model-switch
description: Switch the Manager Agent's own LLM model. Use when the human admin requests changing the Manager's model.
---

# Model Switch

Switch the Manager's own LLM model. The script tests connectivity first, then patches `openclaw.json`.

## Usage

```bash
bash /opt/hiclaw/agent/skills/model-switch/scripts/update-manager-model.sh <MODEL_ID> [--context-window <SIZE>] [--no-reasoning]
```

Examples:
```bash
bash /opt/hiclaw/agent/skills/model-switch/scripts/update-manager-model.sh claude-sonnet-4-6
bash /opt/hiclaw/agent/skills/model-switch/scripts/update-manager-model.sh my-custom-model --context-window 300000
bash /opt/hiclaw/agent/skills/model-switch/scripts/update-manager-model.sh deepseek-chat --no-reasoning
```

## What the script does

1. Strips any `hiclaw-gateway/` prefix from the model name
2. Tests the model via `POST /v1/chat/completions` on the AI Gateway — exits with error if unreachable
3. If the model is already in the `models` array: switches `agents.defaults.model.primary`
4. If the model is new: adds it to the `models` array and switches primary
5. Always outputs `RESTART_REQUIRED`

## After running the script

The script always outputs `RESTART_REQUIRED`. Run `openclaw gateway restart` to apply the change, then tell the human admin the switch is complete.

## Reasoning control

By default, reasoning (extended thinking) is enabled. To disable it, pass `--no-reasoning`.

## On failure

If the gateway test fails (non-200), the script outputs `ERROR: MODEL_NOT_REACHABLE` and exits. No changes are made to `openclaw.json`.

When you see this error, tell the human admin clearly:

1. The model is not reachable because the current default AI Provider likely does not support it.
2. They need to open the Higress Console and:
   - Create a **new AI Provider** for the model's vendor (e.g. `kimi`, `deepseek`, `minimax`).
   - Create a **new AI Route** with a model name prefix predicate (e.g. provider `kimi` → match `kimi-*`), so that requests for models with that prefix are routed to the new provider, while unmatched models continue to go through the default route.
3. **Do NOT modify the default AI Provider** — it is managed by the initialization config and will be overwritten on restart.

After the admin confirms the provider and route are configured, you can retry the model-switch script.

## Important

This skill switches the **primary model** (persisted in `openclaw.json`). After running the script, you must run `openclaw gateway restart` for the change to take effect. The human admin can also use the `/model` slash command to switch the current session's model instantly without restart, but that is non-persistent and only supports pre-configured models.

## Switching to an unknown model

When the human admin requests switching to a model you don't recognize, you MUST:

1. **Ask the admin two questions** before running the script:
   - "This model is not in the known list. What is its context window size (in tokens)?"
   - "Does this model support reasoning (extended thinking)?"
2. Run the script with the appropriate flags:
   ```bash
   bash /opt/hiclaw/agent/skills/model-switch/scripts/update-manager-model.sh <MODEL_ID> --context-window <SIZE> [--no-reasoning]
   ```
3. If the admin does not know the context window, use the default (150,000) by omitting `--context-window`.
4. If the model does not support reasoning, add `--no-reasoning`.

## Pre-configured models (for reference)

| Model | contextWindow | maxTokens |
|-------|--------------|-----------|
| gpt-5.4 | 1,050,000 | 128,000 |
| gpt-5.3-codex / gpt-5-mini / gpt-5-nano | 400,000 | 128,000 |
| claude-opus-4-6 | 1,000,000 | 128,000 |
| claude-sonnet-4-6 | 1,000,000 | 64,000 |
| claude-haiku-4-5 | 200,000 | 64,000 |
| qwen3.5-plus | 200,000 | 64,000 |
| deepseek-chat / deepseek-reasoner / kimi-k2.5 | 256,000 | 128,000 |
| glm-5 / MiniMax-M2.7 / MiniMax-M2.7-highspeed / MiniMax-M2.5 | 200,000 | 128,000 |
