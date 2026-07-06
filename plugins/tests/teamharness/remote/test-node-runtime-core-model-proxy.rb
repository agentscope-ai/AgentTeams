#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
core_model_proxy = repo_root / "plugins/teamharness/remote/node-runtime-core/worker/model-proxy.js"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

script = <<~JS
  const http = require("http");
  const assert = require("assert");
  const core = require(#{core_model_proxy.to_s.inspect});

  function listen(server) {
    return new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => resolve(server.address().port));
    });
  }

  function parseSseEvents(text) {
    return text.trim().split(/\\n\\n+/).filter(Boolean).map(chunk => {
      const lines = chunk.split(/\\n/);
      const eventLine = lines.find(line => line.startsWith("event: "));
      const dataLine = lines.find(line => line.startsWith("data: "));
      return {
        event: eventLine ? eventLine.slice("event: ".length) : "",
        data: dataLine ? JSON.parse(dataLine.slice("data: ".length)) : undefined
      };
    });
  }

  (async () => {
    const gatewayRequests = [];
    const gateway = http.createServer(async (req, res) => {
      const chunks = [];
      for await (const chunk of req) chunks.push(Buffer.from(chunk));
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString("utf8")) : {};
      gatewayRequests.push({ path: req.url, auth: req.headers.authorization, body });
      if (req.url !== "/default/v1/chat/completions") {
        res.writeHead(404, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: "not found" }));
        return;
      }
      if (body.stream) {
        res.writeHead(200, { "content-type": "text/event-stream" });
        res.end(`data: ${JSON.stringify({ choices: [{ delta: { content: "stream-ok" } }] })}\\n\\ndata: [DONE]\\n\\n`);
        return;
      }
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({
        choices: [{ message: { role: "assistant", content: "anthropic-ok" } }],
        usage: {
          prompt_tokens: 123,
          completion_tokens: 45,
          total_tokens: 168,
          prompt_tokens_details: { cached_tokens: 67 }
        }
      }));
    });
    const gatewayPort = await listen(gateway);

    const args = {};
    const runtimeState = {
      runtime: {
        desired: {
          model: {
            model: "qwen3.7-max",
            gatewayUrl: `http://127.0.0.1:${gatewayPort}/default`,
            gatewayKey: "gateway-key"
          }
        }
      }
    };
    await core.startModelProxy(args, { modelGatewayUrl: `http://127.0.0.1:${gatewayPort}` }, runtimeState);

    try {
      const endpoint = args.modelProxy.endpoint;
      const streamResponse = await fetch(`${endpoint}/v1/chat/completions`, {
        method: "POST",
        headers: { "content-type": "application/json", authorization: "Bearer teamharness-local-proxy" },
        body: JSON.stringify({ model: "ignored", messages: [{ role: "user", content: "hi" }], stream: true })
      });
      assert.strictEqual(streamResponse.status, 200);
      const streamText = await streamResponse.text();
      assert(streamText.includes("stream-ok"), "streaming SSE payload should pass through");

      const anthropicResponse = await fetch(`${endpoint}/v1/messages`, {
        method: "POST",
        headers: { "content-type": "application/json", "x-api-key": args.modelProxy.token },
        body: JSON.stringify({ model: "ignored", messages: [{ role: "user", content: "hi" }] })
      });
      assert.strictEqual(anthropicResponse.status, 200);
      const anthropic = await anthropicResponse.json();
      assert.strictEqual(anthropic.content[0].text, "anthropic-ok");
      assert.deepStrictEqual(anthropic.usage, {
        input_tokens: 123,
        output_tokens: 45,
        cache_creation_input_tokens: 0,
        cache_read_input_tokens: 67
      });

      const anthropicStreamResponse = await fetch(`${endpoint}/v1/messages`, {
        method: "POST",
        headers: { "content-type": "application/json", "x-api-key": args.modelProxy.token },
        body: JSON.stringify({ model: "ignored", messages: [{ role: "user", content: "hi" }], stream: true })
      });
      assert.strictEqual(anthropicStreamResponse.status, 200);
      const anthropicStream = await anthropicStreamResponse.text();
      const anthropicEvents = parseSseEvents(anthropicStream);
      const messageStart = anthropicEvents.find(item => item.event === "message_start")?.data;
      const messageDelta = anthropicEvents.find(item => item.event === "message_delta")?.data;
      assert.deepStrictEqual(messageStart.message.usage, {
        input_tokens: 123,
        output_tokens: 0,
        cache_creation_input_tokens: 0,
        cache_read_input_tokens: 67
      });
      assert.deepStrictEqual(messageDelta.usage, { output_tokens: 45 });

      const unauthorized = await fetch(`${endpoint}/v1/messages`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ messages: [{ role: "user", content: "hi" }] })
      });
      assert.strictEqual(unauthorized.status, 401);

      assert(gatewayRequests.some(item => item.body.model === "qwen3.7-max" && item.body.stream === true), "streaming request should rewrite model");
      assert(gatewayRequests.every(item => item.auth === "Bearer gateway-key"), "gateway key should only be used upstream");
      console.log(JSON.stringify({ ok: true, gatewayRequests: gatewayRequests.length }));
    } finally {
      await new Promise(resolve => args.modelProxy.server.close(resolve));
      await new Promise(resolve => gateway.close(resolve));
    }
  })().catch(error => {
    console.error(error.stack || error.message);
    process.exit(1);
  });
JS

stdout, stderr, status = Open3.capture3("node", "-e", script)
fail!("model proxy core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("model proxy core test did not report ok") unless result["ok"] == true
