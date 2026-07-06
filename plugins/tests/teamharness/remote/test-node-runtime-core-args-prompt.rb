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
  const assert = require("assert");
  const argsCore = require(#{(core_root / "args.js").to_s.inspect});
  const promptCore = require(#{(core_root / "prompt.js").to_s.inspect});

  process.env.TEAMHARNESS_STATE_DIR = "/tmp/teamharness-state";
  process.env.TEST_PLUGIN_DIR = "/tmp/plugin-from-env";
  process.env.TEST_COMMAND = "runtime-from-env";
  const parsed = argsCore.parseRuntimeArgs([
    "--bootstrap-token-file", "/tmp/bootstrap-token",
    "--plugin-install-scope", "global",
    "--model-config-mode", "managed-global",
    "--assets-dir", "/tmp/assets",
    "--work-dir", "/tmp/work",
    "--instance-id", "lw_1",
    "--runtime-command", "runtime-from-arg",
    "--interval", "7",
    "--runtime-refresh-interval", "11",
    "--heartbeat-interval", "13",
    "--max-concurrent-tasks", "9",
    "--once"
  ], {
    runtime: "runtime-a",
    defaultStateDir: "/tmp/default-state",
    pluginEnvVar: "TEST_PLUGIN_DIR",
    pluginArgAliases: ["--plugin-dir", "--assets-dir"],
    commandName: "runtimeCommand",
    commandEnvVars: ["TEST_COMMAND"],
    commandDefault: "runtime-default",
    commandArg: "--runtime-command"
  });

  assert.strictEqual(parsed.runtime, "runtime-a");
  assert.strictEqual(parsed.bootstrapTokenFile, "/tmp/bootstrap-token");
  assert.strictEqual(parsed.pluginInstallScope, "global");
  assert.strictEqual(parsed.modelConfigMode, "managed-global");
  assert.strictEqual(parsed.stateDir, "/tmp/teamharness-state");
  assert.strictEqual(parsed.pluginDir, "/tmp/assets");
  assert.strictEqual(parsed.workDir, "/tmp/work");
  assert.strictEqual(parsed.instanceId, "lw_1");
  assert.strictEqual(parsed.runtimeCommand, "runtime-from-arg");
  assert.strictEqual(parsed.intervalSeconds, 7);
  assert.strictEqual(parsed.runtimeRefreshIntervalSeconds, 11);
  assert.strictEqual(parsed.heartbeatIntervalSeconds, 13);
  assert.strictEqual(parsed.maxConcurrentTasks, 8);
  assert.strictEqual(parsed.once, true);

  assert.throws(() => argsCore.parseRuntimeArgs(["--unknown"], { runtime: "runtime-a" }), /unknown argument: --unknown/);

  delete process.env.TEAMHARNESS_STATE_DIR;
  delete process.env.TEST_COMMAND;
  process.env.TEAMHARNESS_MAX_CONCURRENT_TASKS = "3";
  const fallback = argsCore.parseRuntimeArgs([], {
    runtime: "runtime-b",
    defaultStateDir: "/tmp/default-state",
    commandName: "runtimeCommand",
    commandEnvVars: ["TEST_COMMAND"],
    commandDefault: "runtime-default"
  });
  assert.strictEqual(fallback.stateDir, "/tmp/default-state");
  assert.strictEqual(fallback.runtimeCommand, "runtime-default");
  assert.strictEqual(fallback.modelConfigMode, "native-config");
  assert.strictEqual(fallback.maxConcurrentTasks, 3);
  delete process.env.TEAMHARNESS_MAX_CONCURRENT_TASKS;
  const noMax = argsCore.parseRuntimeArgs([], {
    runtime: "runtime-c",
    defaultStateDir: "/tmp/default-state",
    commandName: "runtimeCommand",
    commandDefault: "runtime-default"
  });
  assert.strictEqual(noMax.maxConcurrentTasks, undefined);
  assert.strictEqual(argsCore.normalizeMaxConcurrentTasks("0", 2), 2);
  assert.strictEqual(argsCore.normalizeMaxConcurrentTasks("12", 2), 8);

  const history = Array.from({ length: 25 }, (_, index) => ({
    sender: `@u${index}:example`,
    body: `message-${index}`,
    eventId: `$event-${index}`
  }));
  const stablePrompt = promptCore.buildStableRuntimePrompt({
    introLines: ["Intro line", ""],
    metadata: [
      { label: "Member", value: "worker-01" },
      { label: "Role", value: "worker" }
    ],
    guidanceLines: ["Reply directly."],
    runtimeState: {
      runtime: {
        desired: {
          model: { model: "qwen-test" }
        }
      }
    },
    workspace: "/tmp/work",
    contextSections: ["Agent instructions:\\nFollow the task."]
  });
  const turnPrompt = promptCore.buildMatrixTurnPrompt({
    metadata: [{ label: "Room event id", value: "$hit" }],
    roomHistory: history,
    currentMessage: "worker-01: hello"
  });
  const prompt = promptCore.buildRuntimePrompt({
    introLines: ["Intro line", ""],
    metadata: [
      { label: "Member", value: "worker-01" },
      { label: "Role", value: "worker" }
    ],
    guidanceLines: ["Reply directly."],
    runtimeState: {
      runtime: {
        desired: {
          model: { model: "qwen-test" }
        }
      }
    },
    workspace: "/tmp/work",
    contextSections: ["Agent instructions:\\nFollow the task."],
    roomHistory: history,
    currentMessage: "worker-01: hello"
  });

  assert(prompt.startsWith("Intro line\\n\\nMember: worker-01\\nRole: worker"));
  assert(prompt.includes("Reply directly.\\nManaged model: qwen-test\\nWorkspace: /tmp/work"));
  assert(prompt.includes("\\nRuntime context:\\nAgent instructions:\\nFollow the task."));
  assert(!stablePrompt.includes("worker-01: hello"), "stable prompt must not include current message");
  assert(!turnPrompt.includes("Agent instructions"), "turn prompt must not include stable context");
  assert(!prompt.includes("message-4"), "room history should keep only the latest 20 items");
  assert(prompt.includes("[Chat messages since your last reply - for context]"));
  assert(prompt.includes("@u5:example: message-5 [id:$event-5]"));
  assert(prompt.includes("@u24:example: message-24 [id:$event-24]"));
  assert(prompt.endsWith("\\n[Current message - respond to this]\\nworker-01: hello\\n"));

  console.log(JSON.stringify({ ok: true, argsRuntime: parsed.runtime, promptLines: prompt.split(/\\n/).length }));
JS

stdout, stderr, status = Open3.capture3("node", "-e", script)
fail!("args/prompt core test failed:\n#{stderr}\n#{stdout}") unless status.success?
result = JSON.parse(stdout)
fail!("args/prompt core test did not report ok") unless result["ok"] == true
