# Using Atlas Cloud as the LLM service

Atlas Cloud exposes an OpenAI-compatible text API, so AgentTeams reaches it through the existing generic path. No dedicated provider type, preset, or adapter service is needed.

| Setting | Value |
|---|---|
| Provider type | `openai-compat` |
| Base URL | `https://api.atlascloud.ai/v1` (the API root, not `/chat/completions`) |
| API key | Your Atlas Cloud API key |
| Model | The exact model ID listed by `GET https://api.atlascloud.ai/v1/models` (for example `deepseek-ai/deepseek-v4.1-flash`) |

Model IDs on the gateway change over time. Always confirm the ID against the live model list before configuring it:

```bash
curl -s https://api.atlascloud.ai/v1/models | grep -o '"id":"[^"]*"' | head
```

## Configuration

### Kubernetes (Helm)

```yaml
credentials:
  llmApiKey: "<your-atlascloud-api-key>"
  llmProvider: openai-compat
  llmBaseUrl: https://api.atlascloud.ai/v1
  defaultModel: <exact-model-id>
```

### Local installer

```bash
AGENTTEAMS_LLM_PROVIDER=openai-compat \
AGENTTEAMS_OPENAI_BASE_URL=https://api.atlascloud.ai/v1 \
AGENTTEAMS_LLM_API_KEY='<your-atlascloud-api-key>' \
AGENTTEAMS_DEFAULT_MODEL='<exact-model-id>' \
./agentteams-install.sh
```

## Validation

Validation has two separate levels. Passing the first does not imply the second.

### Level 1: basic connectivity (what the Helm LLM preflight checks)

The Helm preflight Job sends one minimal non-streaming `/chat/completions` request with the configured key, base URL, and model. It confirms that the key is accepted, the URL is reachable, and the model ID exists. It does not exercise streaming or tool calls. You can reproduce it by hand:

```bash
export ATLASCLOUD_API_KEY='<your-atlascloud-api-key>'
curl -s https://api.atlascloud.ai/v1/chat/completions \
  -H "Authorization: Bearer $ATLASCLOUD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<exact-model-id>","messages":[{"role":"user","content":"Reply with: ok"}],"max_tokens":64}'
```

Expected failure modes: HTTP 401 for an invalid key, HTTP 400 `not found` for an unknown model ID.

### Level 2: model capability checks (needed before relying on it for Agents)

Agents depend on streaming responses and tool calling, which a connectivity check does not cover. Check both directly against the gateway for the model you intend to use.

Streaming: add `"stream": true` to the request above. Expect `data: {...}` chunks ending with `data: [DONE]`.

Tool calling: send a request with `tools` and confirm the response has `finish_reason: "tool_calls"` and a `tool_calls[0].function` with the expected name and JSON arguments. Then send the tool result back as a `role: "tool"` message and confirm the model produces a final answer.

```bash
curl -s https://api.atlascloud.ai/v1/chat/completions \
  -H "Authorization: Bearer $ATLASCLOUD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<exact-model-id>",
       "messages":[{"role":"user","content":"What is the weather in Paris? Use the tool."}],
       "tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather for a city",
         "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
       "max_tokens":256}'
```

Capabilities are per model. Repeat these checks when you change `defaultModel` or assign a different model to a Worker.

### Level 3: end-to-end Agent validation

The direct gateway checks above do not prove the full AgentTeams path (Higress route, Manager, Worker runtime). After installing, send the Manager a task that makes it use a tool and create a Worker, and confirm it completes.

## Verification record

On 2026-11-02, the Level 1 and Level 2 requests above were run directly against `https://api.atlascloud.ai/v1` with `deepseek-ai/deepseek-v4.1-flash` (listed by `/v1/models` that day):

| Check | Result |
|---|---|
| Non-streaming chat completion | HTTP 200, `finish_reason: stop` |
| Unknown model ID / invalid key | HTTP 400 `not found` / HTTP 401 |
| Streaming | HTTP 200, 24 chunks, terminated by `[DONE]` |
| Tool call (non-streaming) | `finish_reason: tool_calls`, `get_weather` with `{"city": "Paris"}` |
| Tool call (streaming) | Name and arguments reassembled from deltas, `finish_reason: tool_calls` |
| Tool result round trip | HTTP 200, final answer using the tool result |

Not verified: a Helm install or local install of AgentTeams with these values, the Higress route, or any Manager/Worker task (Level 3).

Parameter support is model-specific (see the [Atlas Cloud protocol guide](https://www.atlascloud.ai/docs/llm-protocols)). When re-checked on 2026-11-11, `deepseek-ai/deepseek-v4.1-flash` returned HTTP 400 for a request with `logprobs: true`, and accepted `reasoning_effort: "low"` (HTTP 200). This applies only to that model on that date and is not a gateway-wide rule. Check any optional parameter against the model you intend to use.
