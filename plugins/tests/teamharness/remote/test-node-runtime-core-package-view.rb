#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
package_view_core = repo_root / "plugins/teamharness/remote/node-runtime-core/worker/package-view.js"

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
  const packageView = require(#{package_view_core.to_s.inspect});

  async function readBody(req) {
    const chunks = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    return Buffer.concat(chunks);
  }

  function listen(server) {
    return new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => resolve(server.address().port));
    });
  }

  (async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-core-package-view-"));
    const packageDir = path.join(tmp, "package");
    fs.mkdirSync(path.join(packageDir, "config"), { recursive: true });
    fs.mkdirSync(path.join(packageDir, "skills", "frontend-design"), { recursive: true });
    fs.writeFileSync(path.join(packageDir, "config", "AGENTS.md"), "agent instructions\\n");
    fs.writeFileSync(path.join(packageDir, "config", "SOUL.md"), "soul instructions\\n");
    fs.writeFileSync(path.join(packageDir, "skills", "frontend-design", "SKILL.md"), "skill instructions\\n");

    const writes = {
      "agents/worker-01/skills/old/SKILL.md": "stale"
    };
    const deletes = [];
    const server = http.createServer(async (req, res) => {
      const body = await readBody(req);
      if (req.method === "PUT" && req.url.startsWith("/test-bucket/")) {
        const key = decodeURIComponent(req.url.replace(/^\\/test-bucket\\//, ""));
        writes[key] = body.toString("utf8");
        res.writeHead(200, { "content-type": "application/xml" });
        res.end("");
        return;
      }
      if (req.method === "DELETE" && req.url.startsWith("/test-bucket/")) {
        const key = decodeURIComponent(req.url.replace(/^\\/test-bucket\\//, ""));
        deletes.push(key);
        delete writes[key];
        res.writeHead(204);
        res.end("");
        return;
      }
      if (req.method === "GET" && req.url.startsWith("/test-bucket/?prefix=")) {
        const parsed = new URL(req.url, "http://127.0.0.1");
        const prefix = parsed.searchParams.get("prefix") || "";
        const keys = Object.keys(writes).filter(key => key.startsWith(prefix));
        res.writeHead(200, { "content-type": "application/xml" });
        res.end(`<ListBucketResult>${keys.map(key => `<Contents><Key>${key}</Key></Contents>`).join("")}</ListBucketResult>`);
        return;
      }
      res.writeHead(404);
      res.end("not found");
    });
    const port = await listen(server);

    try {
      const sts = {
        access_key_id: "ak",
        access_key_secret: "sk",
        security_token: "sts",
        oss_endpoint: `http://127.0.0.1:${port}`,
        oss_bucket: "test-bucket"
      };
      const args = { runtime: "core-test", stateDir: tmp };
      const runtimeState = {
        runtime: {
          member: { name: "worker-01", runtimeName: "worker-01", runtime: "remote-managed-local" },
          storage: { memberPrefix: "agents/worker-01" }
        }
      };
      const state = {
        status: "applied",
        ref: "oss://agents/worker-01/packages/pkg.zip",
        digest: "digest-1",
        objectKey: "agents/worker-01/packages/pkg.zip"
      };
      const prefixes = packageView.packageViewBasePrefixes(runtimeState.runtime);
      assert.deepStrictEqual(prefixes, ["agents/worker-01", "agents/worker-01/.qwenpaw/workspaces/default"]);

      const synced = await packageView.syncAgentPackageViewIfNeeded(args, runtimeState, state, packageDir, sts);
      assert.strictEqual(synced.viewSync.status, "synced");
      assert.strictEqual(synced.viewSync.skillFiles, 2);
      assert.strictEqual(writes["agents/worker-01/AGENTS.md"], "agent instructions\\n");
      assert.strictEqual(writes["agents/worker-01/SOUL.md"], "soul instructions\\n");
      assert.strictEqual(writes["agents/worker-01/skills/frontend-design/SKILL.md"], "skill instructions\\n");
      assert.strictEqual(writes["agents/worker-01/.qwenpaw/workspaces/default/skills/frontend-design/SKILL.md"], "skill instructions\\n");
      assert(deletes.includes("agents/worker-01/skills/old/SKILL.md"), "stale skill should be deleted before sync");
      const marker = JSON.parse(writes["agents/worker-01/.agent-package.json"]);
      assert.strictEqual(marker.digest, "digest-1");

      const statusDoc = JSON.parse(fs.readFileSync(path.join(tmp, "status.json"), "utf8"));
      assert.strictEqual(statusDoc.reason, "AgentPackageViewSynced");
      const stateDoc = JSON.parse(fs.readFileSync(path.join(tmp, "agent-package-state.json"), "utf8"));
      assert.strictEqual(stateDoc.viewSync.status, "synced");

      const beforeWriteCount = Object.keys(writes).length;
      await packageView.syncAgentPackageViewIfNeeded(args, runtimeState, stateDoc, packageDir, sts);
      assert.strictEqual(Object.keys(writes).length, beforeWriteCount, "already synced package should not rewrite view");
      console.log(JSON.stringify({ ok: true, prefixes, writes: Object.keys(writes).length }));
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
fail!("package-view core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("package-view core test did not report ok") unless result["ok"] == true
