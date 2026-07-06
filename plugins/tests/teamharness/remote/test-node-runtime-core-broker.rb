#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
broker_core = repo_root / "plugins/teamharness/remote/node-runtime-core/worker/broker.js"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

script = <<~JS
  const fs = require("fs");
  const os = require("os");
  const path = require("path");
  const assert = require("assert");
  const broker = require(#{broker_core.to_s.inspect});

  (async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-core-broker-"));
    const args = { runtime: "core-test", stateDir: tmp };
    const edge = {
      teamName: "",
      workerName: "worker-01",
      runtimeName: "worker-01",
      matrixHomeserver: "http://matrix.example"
    };
    const sts = {
      access_key_id: "ak",
      access_key_secret: "sk",
      security_token: "sts",
      oss_endpoint: "https://oss-cn-hangzhou-internal.aliyuncs.com",
      oss_bucket: "bucket-a",
      expires_in_sec: 3600,
      expiration: "2030-01-01T00:00:00Z"
    };
    const runtimeState = {
      digest: "runtime-digest",
      sts,
      runtime: {
        metadata: { generation: 7 },
        member: {
          name: "member-a",
          runtimeName: "worker-01",
          role: "worker",
          matrixUserId: "@worker-01:example",
          personalRoomId: "!personal:example"
        },
        matrix: {
          accessToken: "matrix-token",
          homeserver: "http://matrix-runtime.example",
          teamRoomId: "!team:example"
        },
        storage: { teamPrefix: "teams/team-runtime", memberPrefix: "agents/worker-01" },
        desired: {
          skillRegistry: { provider: "nacos", authType: "sts-hiclaw" }
        }
      }
    };
    const callbacks = { descriptor: null, refreshed: 0, cleared: 0 };
    const server = await broker.startBroker(args, edge, sts, runtimeState, {
      runtime: "core-test",
      modelConfig: () => ({ model: "qwen3.7-max", baseUrl: "http://model.local", apiKey: "model-key" }),
      writeDescriptor: (_args, descriptor) => {
        const descriptorFile = path.join(tmp, "credential-broker.json");
        fs.writeFileSync(descriptorFile, JSON.stringify(descriptor));
        callbacks.descriptor = { ...descriptor, descriptorFile };
        return descriptorFile;
      },
      refreshMcpConfig: () => { callbacks.refreshed += 1; },
      clearMcpNeedsAuthCache: () => { callbacks.cleared += 1; }
    });

    try {
      assert(callbacks.descriptor, "descriptor callback should be called");
      assert.strictEqual(callbacks.refreshed, 1);
      assert.strictEqual(callbacks.cleared, 1);
      assert.strictEqual(process.env.TEAMHARNESS_CREDENTIAL_BROKER_DESCRIPTOR, callbacks.descriptor.descriptorFile);
      const token = fs.readFileSync(callbacks.descriptor.tokenFile, "utf8").trim();
      const base = callbacks.descriptor.endpoint;

      const unauthorized = await fetch(`${base}/v1/runtime/context`);
      assert.strictEqual(unauthorized.status, 401);

      async function get(pathname) {
        const response = await fetch(`${base}${pathname}`, { headers: { authorization: `Bearer ${token}` } });
        assert.strictEqual(response.status, 200, `${pathname} should succeed`);
        return response.json();
      }

      const context = await get("/v1/runtime/context");
      assert.strictEqual(context.runtime, "core-test");
      assert.strictEqual(context.teamName, "team-runtime");
      assert.strictEqual(context.memberName, "member-a");
      assert.strictEqual(context.teamRoomId, "!team:example");
      assert.strictEqual(context.runtimeDigest, "runtime-digest");

      const matrix = await get("/v1/credentials/matrix");
      assert.strictEqual(matrix.homeserver, "http://matrix-runtime.example");
      assert.strictEqual(matrix.accessToken, "matrix-token");
      assert.deepStrictEqual(matrix.rooms, ["!team:example", "!personal:example"]);

      const model = await get("/v1/credentials/model");
      assert.strictEqual(model.model, "qwen3.7-max");
      assert.strictEqual(model.apiKey, "model-key");

      const skillRegistry = await get("/v1/credentials/skill-registry");
      assert.strictEqual(skillRegistry.provider, "nacos");
      assert.strictEqual(skillRegistry.accessKeyId, "ak");
      assert.strictEqual(skillRegistry.securityToken, "sts");

      const storage = await get("/v1/credentials/storage");
      assert.strictEqual(storage.provider, "oss");
      assert.strictEqual(storage.bucket, "bucket-a");
      assert.strictEqual(storage.endpoint, "https://oss-cn-hangzhou.aliyuncs.com");

      const missing = await fetch(`${base}/missing`, { headers: { authorization: `Bearer ${token}` } });
      assert.strictEqual(missing.status, 404);
      console.log(JSON.stringify({ ok: true, endpoint: base }));
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
fail!("broker core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("broker core test did not report ok") unless result["ok"] == true
