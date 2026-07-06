#!/usr/bin/env ruby
# frozen_string_literal: true

require "json"
require "open3"
require "pathname"

repo_root = Pathname.new(__dir__).join("../../../..").expand_path
worker_cli = repo_root / "plugins/teamharness/remote/openclaw/node-worker/src/cli.js"

def fail!(message)
  warn "ERROR: #{message}"
  exit 1
end

script = <<~JS
  const assert = require("assert");
  const worker = require(#{worker_cli.to_s.inspect});

  const sessions = {};
  const roomId = "!team:matrix.example";
  const args = { instanceId: "lw_openclaw_test", workDir: "/tmp/openclaw-workdir-for-test" };
  const runtimeState = {
    runtime: {
      member: {
        name: "worker-01",
        runtimeName: "worker-01"
      }
    }
  };
  const edge = { workerName: "worker-01" };

  assert.strictEqual(worker.openclawSessionStoreKey(roomId), roomId);
  const first = worker.openclawSessionForRoom(sessions, roomId, args, runtimeState, edge);
  assert.match(first, /^oc_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+$/);
  assert(!first.includes(roomId), "runtime session id must not include raw Matrix room id");
  assert(!first.includes(args.workDir), "runtime session id must not include workdir");
  assert.strictEqual(sessions[roomId], first);

  const same = worker.openclawSessionForRoom(sessions, roomId, args, runtimeState, edge);
  assert.strictEqual(same, first, "sessionForRoom should reuse existing room session");

  worker.dropOpenClawSessionForRoom(sessions, roomId, args, runtimeState, edge);
  const rotated = sessions[roomId];
  assert.match(rotated, /^oc_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+_[A-Za-z0-9_-]+$/);
  assert.notStrictEqual(rotated, first, "dropSessionForRoom should rotate OpenClaw runtime session id");

  worker.storeOpenClawSessionForRoom(sessions, roomId, args, "not-openclaw-session");
  assert.strictEqual(sessions[roomId], rotated, "non-OpenClaw session ids should not overwrite runtime session state");
  worker.storeOpenClawSessionForRoom(sessions, roomId, args, "oc_custom_worker_room_epoch");
  assert.strictEqual(sessions[roomId], "oc_custom_worker_room_epoch");

  const message = worker.currentMessageFromTurn({
    messages: [
      { sender: "@u1:example", body: "第一条" },
      { sender: "@u2:example", body: "第二条" }
    ]
  });
  assert(message.includes("[1] @u1:example: 第一条"));
  assert(message.includes("[2] @u2:example: 第二条"));

  console.log(JSON.stringify({ ok: true, first, rotated }));
JS

stdout, stderr, status = Open3.capture3("node", stdin_data: script)
fail!("openclaw session test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("openclaw session test did not report ok") unless result["ok"] == true
