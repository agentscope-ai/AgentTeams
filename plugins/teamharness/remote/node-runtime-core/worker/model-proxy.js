#!/usr/bin/env node
"use strict";

const crypto = require("crypto");
const http = require("http");

function combineGatewayUrl(modelGatewayUrl, runtimeGatewayUrl) {
  const gateway = String(modelGatewayUrl || "").trim();
  const runtime = String(runtimeGatewayUrl || "").trim();
  if (!gateway) {
    return runtime;
  }
  if (!runtime) {
    return gateway;
  }
  try {
    const gatewayUrl = new URL(gateway);
    const runtimeUrl = new URL(runtime);
    gatewayUrl.pathname = runtimeUrl.pathname || gatewayUrl.pathname;
    gatewayUrl.search = runtimeUrl.search;
    return gatewayUrl.toString().replace(/\/+$/, "");
  } catch {
    return runtime || gateway;
  }
}

function openaiChatUrl(baseUrl) {
  const base = String(baseUrl || "").trim().replace(/\/+$/, "");
  if (!base) return "";
  if (base.endsWith("/v1/chat/completions") || base.endsWith("/chat/completions")) return base;
  if (base.endsWith("/v1")) return `${base}/chat/completions`;
  return `${base}/v1/chat/completions`;
}

function runtimeLlmConfig(edge, runtimeState) {
  const model = runtimeState.runtime?.desired?.model || {};
  return {
    model: String(model.model || "").trim(),
    baseUrl: combineGatewayUrl(edge.modelGatewayUrl, model.gatewayUrl),
    apiKey: String(model.gatewayKey || "").trim()
  };
}

function managedLlmConfig(edge, runtimeState) {
  const raw = runtimeLlmConfig(edge, runtimeState);
  const proxy = runtimeState.args?.modelProxy;
  if (proxy?.endpoint && proxy?.token) {
    return { ...raw, baseUrl: proxy.endpoint, apiKey: proxy.token };
  }
  return raw;
}

function contentText(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const parts = [];
  for (const block of content) {
    if (!block || typeof block !== "object") continue;
    if (block.type === "text" && typeof block.text === "string" && block.text) {
      parts.push(block.text);
    } else if (block.type === "tool_result") {
      const text = contentText(block.content);
      if (text) parts.push(text);
    }
  }
  return parts.join("\n");
}

function anthropicToolsToOpenai(payload) {
  if (!Array.isArray(payload.tools)) return [];
  return payload.tools
    .filter(tool => tool && typeof tool === "object" && String(tool.name || "").trim())
    .map(tool => {
      const fn = {
        name: String(tool.name).trim(),
        parameters: tool.input_schema && typeof tool.input_schema === "object"
          ? tool.input_schema
          : { type: "object", properties: {} }
      };
      if (tool.description) fn.description = String(tool.description);
      return { type: "function", function: fn };
    });
}

function anthropicToolChoiceToOpenai(payload) {
  const choice = payload.tool_choice;
  if (!choice || typeof choice !== "object") return undefined;
  const type = String(choice.type || "");
  if (type === "auto" || type === "any") return "auto";
  if (type === "none") return "none";
  if (type === "tool" && choice.name) {
    return { type: "function", function: { name: String(choice.name) } };
  }
  return undefined;
}

function anthropicMessagesToOpenai(payload, model) {
  const messages = [];
  const system = contentText(payload.system);
  if (system) messages.push({ role: "system", content: system });
  if (Array.isArray(payload.messages)) {
    for (const item of payload.messages) {
      if (!item || typeof item !== "object") continue;
      let role = String(item.role || "user");
      if (!["system", "user", "assistant"].includes(role)) role = "user";
      const content = item.content;
      if (role === "assistant" && Array.isArray(content)) {
        const textParts = [];
        const toolCalls = [];
        for (const block of content) {
          if (!block || typeof block !== "object") continue;
          if (block.type === "text" && block.text) {
            textParts.push(String(block.text));
          } else if (block.type === "tool_use" && block.name) {
            toolCalls.push({
              id: String(block.id || `toolu_${crypto.randomUUID().replace(/-/g, "")}`),
              type: "function",
              function: {
                name: String(block.name),
                arguments: JSON.stringify(block.input || {})
              }
            });
          }
        }
        if (textParts.length || toolCalls.length) {
          const message = { role: "assistant", content: textParts.join("\n") || null };
          if (toolCalls.length) message.tool_calls = toolCalls;
          messages.push(message);
        }
        continue;
      }
      if (role === "user" && Array.isArray(content)) {
        const textParts = [];
        const toolResults = [];
        for (const block of content) {
          if (!block || typeof block !== "object") continue;
          if (block.type === "text" && block.text) {
            textParts.push(String(block.text));
          } else if (block.type === "tool_result") {
            toolResults.push({
              role: "tool",
              tool_call_id: String(block.tool_use_id || ""),
              content: contentText(block.content)
            });
          }
        }
        if (textParts.length) messages.push({ role: "user", content: textParts.join("\n") });
        for (const result of toolResults) {
          if (result.tool_call_id) messages.push(result);
        }
        if (textParts.length || toolResults.length) continue;
      }
      const text = contentText(content);
      if (text) messages.push({ role, content: text });
    }
  }
  const request = {
    model,
    messages: messages.length ? messages : [{ role: "user", content: "" }],
    stream: false
  };
  const tools = anthropicToolsToOpenai(payload);
  if (tools.length) request.tools = tools;
  const toolChoice = anthropicToolChoiceToOpenai(payload);
  if (toolChoice !== undefined) request.tool_choice = toolChoice;
  for (const [source, target] of [
    ["max_tokens", "max_tokens"],
    ["temperature", "temperature"],
    ["top_p", "top_p"],
    ["stop_sequences", "stop"]
  ]) {
    if (payload[source] !== undefined) request[target] = payload[source];
  }
  return request;
}

function finiteToken(value) {
  if (value === undefined || value === null || value === "") return undefined;
  const number = Number(value);
  if (!Number.isFinite(number) || number < 0) return undefined;
  return Math.trunc(number);
}

function firstToken(...values) {
  for (const value of values) {
    const token = finiteToken(value);
    if (token !== undefined) return token;
  }
  return 0;
}

function openaiUsageToAnthropic(usage) {
  const raw = usage && typeof usage === "object" ? usage : {};
  const promptDetails = raw.prompt_tokens_details && typeof raw.prompt_tokens_details === "object"
    ? raw.prompt_tokens_details
    : {};
  return {
    input_tokens: firstToken(raw.input_tokens, raw.prompt_tokens),
    output_tokens: firstToken(raw.output_tokens, raw.completion_tokens),
    cache_creation_input_tokens: firstToken(raw.cache_creation_input_tokens, raw.cache_write_tokens),
    cache_read_input_tokens: firstToken(
      raw.cache_read_input_tokens,
      raw.cache_read_tokens,
      raw.cached_tokens,
      promptDetails.cached_tokens
    )
  };
}

function openaiToAnthropic(model, payload) {
  const choice = Array.isArray(payload.choices) ? payload.choices[0] : undefined;
  const message = choice && typeof choice === "object" && choice.message && typeof choice.message === "object"
    ? choice.message
    : {};
  const content = [];
  if (typeof message.content === "string" && message.content) {
    content.push({ type: "text", text: message.content });
  }
  if (Array.isArray(message.tool_calls)) {
    for (const call of message.tool_calls) {
      if (!call || typeof call !== "object" || !call.function || typeof call.function !== "object") continue;
      const name = String(call.function.name || "").trim();
      if (!name) continue;
      let input = {};
      const rawArgs = call.function.arguments;
      if (typeof rawArgs === "string" && rawArgs.trim()) {
        try {
          input = JSON.parse(rawArgs);
        } catch {
          input = {};
        }
      } else if (rawArgs && typeof rawArgs === "object") {
        input = rawArgs;
      }
      content.push({
        type: "tool_use",
        id: String(call.id || `toolu_${crypto.randomUUID().replace(/-/g, "")}`),
        name,
        input: input && typeof input === "object" ? input : {}
      });
    }
  }
  if (!content.length) content.push({ type: "text", text: "" });
  return {
    id: `msg_${crypto.randomUUID().replace(/-/g, "")}`,
    type: "message",
    role: "assistant",
    content,
    model,
    stop_reason: content.some(block => block.type === "tool_use") ? "tool_use" : "end_turn",
    stop_sequence: null,
    usage: openaiUsageToAnthropic(payload.usage)
  };
}

function anthropicStreamStartUsage(usage) {
  const raw = usage && typeof usage === "object" ? usage : {};
  return {
    input_tokens: firstToken(raw.input_tokens),
    output_tokens: 0,
    cache_creation_input_tokens: firstToken(raw.cache_creation_input_tokens),
    cache_read_input_tokens: firstToken(raw.cache_read_input_tokens)
  };
}

function sendJson(res, status, payload) {
  const body = Buffer.from(JSON.stringify(payload));
  res.writeHead(status, {
    "content-type": "application/json",
    "content-length": String(body.length)
  });
  res.end(body);
}

function writeAnthropicSse(res, message) {
  res.writeHead(200, {
    "content-type": "text/event-stream",
    "cache-control": "no-cache"
  });
  const write = (event, payload) => {
    res.write(`event: ${event}\n`);
    res.write(`data: ${JSON.stringify(payload)}\n\n`);
  };
  const start = {
    ...message,
    content: [],
    stop_reason: null,
    usage: anthropicStreamStartUsage(message.usage)
  };
  write("message_start", { type: "message_start", message: start });
  for (let index = 0; index < message.content.length; index += 1) {
    const block = message.content[index];
    if (block.type === "tool_use") {
      write("content_block_start", {
        type: "content_block_start",
        index,
        content_block: { type: "tool_use", id: block.id, name: block.name, input: {} }
      });
      write("content_block_delta", {
        type: "content_block_delta",
        index,
        delta: { type: "input_json_delta", partial_json: JSON.stringify(block.input || {}) }
      });
    } else {
      write("content_block_start", {
        type: "content_block_start",
        index,
        content_block: { type: "text", text: "" }
      });
      write("content_block_delta", {
        type: "content_block_delta",
        index,
        delta: { type: "text_delta", text: String(block.text || "") }
      });
    }
    write("content_block_stop", { type: "content_block_stop", index });
  }
  write("message_delta", {
    type: "message_delta",
    delta: { stop_reason: message.stop_reason, stop_sequence: null },
    usage: { output_tokens: firstToken(message.usage?.output_tokens) }
  });
  write("message_stop", { type: "message_stop" });
  res.end();
}

async function readRequestJson(req) {
  const chunks = [];
  for await (const chunk of req) chunks.push(Buffer.from(chunk));
  if (!chunks.length) return {};
  return JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
}

async function postOpenaiChat(upstream, apiKey, payload) {
  const response = await fetch(upstream, {
    method: "POST",
    headers: {
      accept: "application/json",
      "content-type": "application/json; charset=utf-8",
      authorization: `Bearer ${apiKey}`
    },
    body: JSON.stringify(payload)
  });
  const text = await response.text();
  let body = {};
  try {
    body = text ? JSON.parse(text) : {};
  } catch {
    body = { error: text };
  }
  if (!response.ok) {
    const message = typeof body.error === "string" ? body.error : JSON.stringify(body);
    const error = new Error(message || `upstream failed: ${response.status}`);
    error.status = response.status;
    throw error;
  }
  return body;
}

async function streamOpenaiChat(upstream, apiKey, payload, res) {
  const response = await fetch(upstream, {
    method: "POST",
    headers: {
      accept: "text/event-stream, application/json",
      "content-type": "application/json; charset=utf-8",
      authorization: `Bearer ${apiKey}`
    },
    body: JSON.stringify(payload)
  });
  if (!response.ok) {
    const text = await response.text();
    let body = {};
    try {
      body = text ? JSON.parse(text) : {};
    } catch {
      body = { error: text };
    }
    const message = typeof body.error === "string" ? body.error : JSON.stringify(body);
    const error = new Error(message || `upstream failed: ${response.status}`);
    error.status = response.status;
    throw error;
  }

  res.writeHead(response.status, {
    "content-type": response.headers.get("content-type") || "text/event-stream",
    "cache-control": response.headers.get("cache-control") || "no-cache"
  });
  if (!response.body) {
    res.end();
    return;
  }
  const reader = response.body.getReader();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    res.write(Buffer.from(value));
  }
  res.end();
}

async function startModelProxy(args, edge, runtimeState) {
  const token = crypto.randomBytes(24).toString("hex");
  const server = http.createServer(async (req, res) => {
    try {
      const llm = runtimeLlmConfig(edge, runtimeState);
      if (!llm.model || !llm.baseUrl || !llm.apiKey) {
        sendJson(res, 503, { type: "error", error: { type: "api_error", message: "managed model config is incomplete" } });
        return;
      }
      const apiKey = String(req.headers["x-api-key"] || "");
      const auth = String(req.headers.authorization || "");
      if (apiKey !== token && auth !== `Bearer ${token}` && apiKey !== "teamharness-local-proxy" && auth !== "Bearer teamharness-local-proxy") {
        sendJson(res, 401, { type: "error", error: { type: "authentication_error", message: "unauthorized" } });
        return;
      }
      const requestPath = new URL(req.url || "/", "http://127.0.0.1").pathname;
      if (req.method === "GET" && requestPath === "/v1/models") {
        sendJson(res, 200, {
          data: [{ type: "model", id: llm.model, display_name: llm.model }],
          has_more: false,
          first_id: llm.model,
          last_id: llm.model
        });
        return;
      }
      if (req.method === "POST" && ["/v1/chat/completions", "/chat/completions"].includes(requestPath)) {
        const payload = await readRequestJson(req);
        const upstreamPayload = { ...payload, model: llm.model || payload.model };
        if (upstreamPayload.stream) {
          await streamOpenaiChat(openaiChatUrl(llm.baseUrl), llm.apiKey, upstreamPayload, res);
          return;
        }
        const data = await postOpenaiChat(openaiChatUrl(llm.baseUrl), llm.apiKey, upstreamPayload);
        sendJson(res, 200, data);
        return;
      }
      if (req.method !== "POST" || !["/v1/messages", "/messages"].includes(requestPath)) {
        sendJson(res, 404, { type: "error", error: { type: "not_found_error", message: "not found" } });
        return;
      }
      const payload = await readRequestJson(req);
      const wantsStream = Boolean(payload.stream);
      const upstreamPayload = anthropicMessagesToOpenai(payload, llm.model);
      const data = await postOpenaiChat(openaiChatUrl(llm.baseUrl), llm.apiKey, upstreamPayload);
      const message = openaiToAnthropic(llm.model, data);
      if (wantsStream) {
        writeAnthropicSse(res, message);
      } else {
        sendJson(res, 200, message);
      }
    } catch (error) {
      sendJson(res, error.status || 502, {
        type: "error",
        error: { type: "api_error", message: error.message || String(error) }
      });
    }
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  const port = address && typeof address === "object" ? address.port : 0;
  args.modelProxy = { server, endpoint: `http://127.0.0.1:${port}`, token };
  return args.modelProxy;
}

module.exports = {
  combineGatewayUrl,
  openaiChatUrl,
  runtimeLlmConfig,
  startModelProxy
};
