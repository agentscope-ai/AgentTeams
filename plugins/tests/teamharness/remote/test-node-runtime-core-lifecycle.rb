#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
core_root = repo_root / "plugins/teamharness/remote/node-runtime-core/worker"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

script = <<~JS
  const http = require("http");
  const fs = require("fs");
  const os = require("os");
  const path = require("path");
  const assert = require("assert");
  const argsCore = require(#{(core_root / "args.js").to_s.inspect});
  const lifecycle = require(#{(core_root / "lifecycle.js").to_s.inspect});

  function listen(server) {
    return new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => resolve(server.address().port));
    });
  }

  function writeBootstrapTokenFile(dir, token) {
    const tokenFile = path.join(dir, "credentials", "bootstrap-token");
    fs.mkdirSync(path.dirname(tokenFile), { recursive: true });
    fs.writeFileSync(tokenFile, `${token}\\n`, { mode: 0o600 });
    return tokenFile;
  }

  async function readBody(req) {
    const chunks = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    return Buffer.concat(chunks).toString("utf8");
  }

  (async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-core-lifecycle-"));
    const calls = [];
    const requests = [];
    const runtimeYaml = [
      "matrix:",
      "  accessToken: matrix-token",
      "member:",
      "  name: worker-01",
      "desired:",
      "  model:",
      "    model: qwen-test",
      "storage:",
      "  provider: oss",
      ""
    ].join("\\n");
    const server = http.createServer(async (req, res) => {
      const body = await readBody(req);
      requests.push({ method: req.method, url: req.url, auth: req.headers.authorization || "", body });
      if (req.method === "POST" && req.url === "/api/v1/edge/token") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({
          token: "edge-token",
          jwtToken: "jwt-refreshed",
          workerName: "worker-01",
          workerResourceName: "worker-resource",
          runtimeName: "worker-01",
          matrixHomeserver: "http://matrix.example",
          modelGatewayUrl: "http://gateway.example"
        }));
        return;
      }
      if (req.method === "POST" && req.url === "/api/v1/credentials/sts") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({
          access_key_id: "ak",
          access_key_secret: "sk",
          security_token: "sts",
          oss_endpoint: `http://127.0.0.1:${port}`,
          oss_bucket: "test-bucket",
          expires_in_sec: 3600
        }));
        return;
      }
      if (req.method === "POST" && req.url === "/api/v1/workers/worker-resource/heartbeat") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end("{}");
        return;
      }
      if (req.method === "GET" && req.url === "/test-bucket/shared/runtime/members/worker-01/runtime.yaml") {
        res.writeHead(200, { "content-type": "text/yaml" });
        res.end(runtimeYaml);
        return;
      }
      res.writeHead(404, { "content-type": "text/plain" });
      res.end("not found");
    });
    const port = await listen(server);

    try {
      const token = Buffer.from(JSON.stringify({
        jwtToken: "jwt-initial",
        matrixUrl: `http://127.0.0.1:${port}`,
        controllerUrl: `http://127.0.0.1:${port}`,
        modelGatewayUrl: `http://127.0.0.1:${port}/gateway`
      })).toString("base64");
      const adapter = {
        parseArgs(argv) {
          return argsCore.parseRuntimeArgs(argv, {
            runtime: "core-test",
            defaultStateDir: tmp,
            commandName: "runtimeCommand",
            commandDefault: process.execPath,
            commandArg: "--runtime-command"
          });
        },
        command: args => args.runtimeCommand,
        commandMissingReason: "RuntimeNotFound",
        commandMissingMessage: args => `runtime command not found: ${args.runtimeCommand}`,
        afterRuntimeLoaded: async () => calls.push("afterRuntimeLoaded"),
        applyAgentPackage: async () => calls.push("applyAgentPackage"),
        startModelProxy: async args => {
          calls.push("startModelProxy");
          args.modelProxy = { server: { close: cb => { calls.push("closeModelProxy"); cb(); } } };
        },
        startBroker: async () => {
          calls.push("startBroker");
          return { close: cb => { calls.push("closeBroker"); cb(); } };
        },
        applyGlobalIntegrations: () => calls.push("applyGlobalIntegrations"),
        cleanupBrokerFiles: () => calls.push("cleanupBrokerFiles"),
        cleanupGlobalIntegrations: () => calls.push("cleanupGlobalIntegrations"),
        startRemotePeriodicTasks: () => {
          throw new Error("periodic tasks should not start in once mode");
        },
        matrixLoop: async () => {
          throw new Error("matrix loop should not start in once mode");
        },
        readyMessage: "core worker running"
      };

      const workDir = path.join(tmp, "workdir");
      await lifecycle.runWorkerMain([
        "--bootstrap-token-file", writeBootstrapTokenFile(tmp, token),
        "--work-dir", workDir,
        "--runtime-command", process.execPath,
        "--once"
      ], adapter);
      assert(fs.existsSync(workDir), "worker lifecycle should create missing work dir");

      assert.deepStrictEqual(calls, [
        "afterRuntimeLoaded",
        "applyAgentPackage",
        "startModelProxy",
        "startBroker",
        "applyGlobalIntegrations",
        "closeModelProxy",
        "closeBroker",
        "cleanupBrokerFiles",
        "cleanupGlobalIntegrations"
      ]);
      assert.deepStrictEqual(JSON.parse(requests.find(item => item.url === "/api/v1/edge/token").body), { jwtToken: "jwt-initial" });
      const persistedBootstrap = JSON.parse(Buffer.from(fs.readFileSync(path.join(tmp, "credentials", "bootstrap-token"), "utf8").trim(), "base64").toString("utf8"));
      assert.strictEqual(persistedBootstrap.jwtToken, "jwt-refreshed");
      assert.strictEqual(requests.find(item => item.url === "/api/v1/credentials/sts").auth, "Bearer edge-token");
      assert(requests.some(item => item.url === "/api/v1/workers/worker-resource/heartbeat"), "heartbeat should be reported before once-mode cleanup");

      const statusDoc = JSON.parse(fs.readFileSync(path.join(tmp, "status.json"), "utf8"));
      assert.strictEqual(statusDoc.reason, "HeartbeatReported");

      const missingTmp = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-core-lifecycle-missing-"));
      await lifecycle.runWorkerMain([
        "--bootstrap-token-file", writeBootstrapTokenFile(missingTmp, token),
        "--work-dir", missingTmp,
        "--runtime-command", path.join(missingTmp, "missing-runtime"),
        "--state-dir", missingTmp,
        "--once"
      ], adapter);
      const waitingStatus = JSON.parse(fs.readFileSync(path.join(missingTmp, "status.json"), "utf8"));
      assert.strictEqual(waitingStatus.reason, "RuntimeNotFound");

      console.log(JSON.stringify({ ok: true, calls: calls.length, requests: requests.length }));
    } finally {
      await new Promise(resolve => server.close(resolve));
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  })().catch(error => {
    console.error(error.stack || error.message);
    process.exit(1);
  });
JS

stdout, stderr, status = Open3.capture3("node", "-e", script)
fail!("lifecycle core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("lifecycle core test did not report ok") unless result["ok"] == true
