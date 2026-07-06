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
  const bootstrap = require(#{(core_root / "bootstrap.js").to_s.inspect});
  const controller = require(#{(core_root / "controller.js").to_s.inspect});
  const runtimeConfig = require(#{(core_root / "runtime-config.js").to_s.inspect});
  const storage = require(#{(core_root / "storage-oss.js").to_s.inspect});
  const status = require(#{(core_root / "status.js").to_s.inspect});

  function listen(server) {
    return new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => resolve(server.address().port));
    });
  }

  function readBody(req) {
    return new Promise((resolve, reject) => {
      const chunks = [];
      req.on("data", chunk => chunks.push(Buffer.from(chunk)));
      req.on("end", () => resolve(Buffer.concat(chunks)));
      req.on("error", reject);
    });
  }

  (async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-core-runtime-"));
    const requests = [];
    const writes = {};
    const runtimeYaml = [
      "apiVersion: hiclaw.io/v1beta1",
      "kind: MemberRuntimeConfig",
      "matrix:",
      "  accessToken: matrix-token",
      "  teamRoomId: '!team:example'",
      "member:",
      "  name: worker-01",
      "  runtimeName: worker-01",
      "  personalRoomId: '!personal:example'",
      "desired:",
      "  model:",
      "    model: qwen3.7-max",
      "    providerId: default",
      "    gatewayUrl: http://gateway.example/default",
      "    gatewayKey: secret-gateway-key",
      "  agentPackage:",
      "    ref: oss://agents/worker-01/packages/pkg.zip",
      "storage:",
      "  provider: oss",
      "  bucket: test-bucket",
      "  endpoint: http://127.0.0.1:1",
      "  memberPrefix: agents/worker-01",
      ""
    ].join("\\n");

    const server = http.createServer(async (req, res) => {
      const bodyBuffer = await readBody(req);
      const bodyText = bodyBuffer.toString("utf8");
      requests.push({ method: req.method, url: req.url, auth: req.headers.authorization || "", body: bodyText });

      if (req.method === "POST" && req.url === "/api/v1/edge/token") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({
          token: "edge-token",
          jwtToken: "jwt-refreshed",
          workerName: "worker-01",
          workerResourceName: "magic-worker-worker-01",
          runtimeName: "worker-01",
          teamName: "team-a"
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
      if (req.method === "POST" && req.url === "/api/v1/workers/magic-worker-worker-01/heartbeat") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end("{}");
        return;
      }
      if (req.method === "GET" && req.url === "/test-bucket/shared/runtime/members/worker-01/runtime.yaml") {
        res.writeHead(200, { "content-type": "text/yaml" });
        res.end(runtimeYaml);
        return;
      }
      if (req.method === "PUT" && req.url.startsWith("/test-bucket/")) {
        writes[decodeURIComponent(req.url.replace(/^\\/test-bucket\\//, ""))] = bodyText;
        res.writeHead(200, { "content-type": "application/xml" });
        res.end("");
        return;
      }
      if (req.method === "DELETE" && req.url.startsWith("/test-bucket/")) {
        delete writes[decodeURIComponent(req.url.replace(/^\\/test-bucket\\//, ""))];
        res.writeHead(204);
        res.end("");
        return;
      }
      if (req.method === "GET" && req.url.startsWith("/test-bucket/?prefix=")) {
        const url = new URL(req.url, "http://127.0.0.1");
        const prefix = url.searchParams.get("prefix") || "";
        const keys = Object.keys(writes).filter(key => key.startsWith(prefix));
        res.writeHead(200, { "content-type": "application/xml" });
        res.end(`<ListBucketResult>${keys.map(key => `<Contents><Key>${key}</Key></Contents>`).join("")}</ListBucketResult>`);
        return;
      }
      res.writeHead(404, { "content-type": "text/plain" });
      res.end("not found");
    });
    const port = await listen(server);

    try {
      const token = Buffer.from(JSON.stringify({
        jwtToken: "jwt-initial",
        matrixUrl: `http://127.0.0.1:${port}/matrix/`,
        controllerUrl: `http://127.0.0.1:${port}/controller/`,
        modelGatewayUrl: `http://127.0.0.1:${port}/gateway/`
      })).toString("base64");
      const decoded = bootstrap.decodeBootstrapToken(token);
      assert.strictEqual(decoded.controllerUrl, `http://127.0.0.1:${port}/controller`);
      assert.throws(() => bootstrap.decodeBootstrapToken(""), /bootstrap token is required/);
      assert.throws(() => bootstrap.decodeBootstrapToken(Buffer.from("{}").toString("base64")), /jwtToken is required/);

      const args = {
        runtime: "core-test",
        stateDir: tmp,
        bootstrapTokenFile: path.join(tmp, "credentials", "bootstrap-token"),
        instanceId: "lw_test",
        runtimeRefreshIntervalSeconds: 60
      };
      bootstrap.writeBootstrapTokenFile(args, token);
      const edge = await controller.exchangeEdgeToken(args, decoded);
      assert.strictEqual(edge.workerName, "worker-01");
      assert.deepStrictEqual(JSON.parse(requests.find(item => item.url === "/api/v1/edge/token").body), { jwtToken: "jwt-initial" });
      assert.strictEqual(bootstrap.decodeBootstrapToken(fs.readFileSync(args.bootstrapTokenFile, "utf8")).jwtToken, "jwt-refreshed");

      const sts = await controller.requestSts(args, edge);
      assert.strictEqual(requests.find(item => item.url === "/api/v1/credentials/sts").auth, "Bearer edge-token");

      const loaded = await runtimeConfig.loadRuntimeConfig(args, edge, sts);
      assert.strictEqual(loaded.objectKey, "shared/runtime/members/worker-01/runtime.yaml");
      assert.strictEqual(loaded.runtime.desired.model.model, "qwen3.7-max");
      assert.deepStrictEqual(runtimeConfig.matrixRooms(loaded.runtime), ["!team:example", "!personal:example"]);
      const snapshot = JSON.parse(fs.readFileSync(path.join(tmp, "runtime-state.json"), "utf8"));
      assert.strictEqual(snapshot.desired.model.hasGatewayKey, true);
      assert(!JSON.stringify(snapshot).includes("secret-gateway-key"), "runtime-state snapshot must redact gateway key");

      await storage.ossPut(sts, "agents/worker-01/file.txt", "hello");
      assert.strictEqual(writes["agents/worker-01/file.txt"], "hello");
      const listed = await storage.ossList(sts, "agents/worker-01/");
      assert.deepStrictEqual(listed, ["agents/worker-01/file.txt"]);
      await storage.ossDelete(sts, "agents/worker-01/file.txt");
      assert.deepStrictEqual(await storage.ossList(sts, "agents/worker-01/"), []);

      await controller.reportHeartbeat(args, edge);
      assert(requests.some(item => item.method === "POST" && item.url === "/api/v1/workers/magic-worker-worker-01/heartbeat"));

      const statusDoc = status.readJson(path.join(tmp, "status.json"), {});
      assert.strictEqual(statusDoc.runtime, "core-test");
      console.log(JSON.stringify({ ok: true, requests: requests.length }));
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
fail!("runtime-config core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("runtime-config core test did not report ok") unless result["ok"] == true
