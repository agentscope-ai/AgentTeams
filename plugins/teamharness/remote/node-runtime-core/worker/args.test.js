"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const test = require("node:test");
const { parseRuntimeArgs } = require("./args");

function withEnv(name, value, fn) {
  const oldValue = process.env[name];
  if (value === undefined) {
    delete process.env[name];
  } else {
    process.env[name] = value;
  }
  try {
    return fn();
  } finally {
    if (oldValue === undefined) {
      delete process.env[name];
    } else {
      process.env[name] = oldValue;
    }
  }
}

function withPath(value, fn) {
  return withEnv("PATH", value, fn);
}

test("model config mode defaults to native-config", () => {
  const args = parseRuntimeArgs([
    "--bootstrap-token-file",
    "/tmp/bootstrap-token"
  ]);

  assert.equal(args.modelConfigMode, "native-config");
});

test("model config mode accepts native-config", () => {
  const args = parseRuntimeArgs([
    "--bootstrap-token-file",
    "/tmp/bootstrap-token",
    "--model-config-mode",
    "native-config"
  ]);

  assert.equal(args.modelConfigMode, "native-config");
});

test("model config mode rejects old user-config value", () => {
  assert.throws(
    () => parseRuntimeArgs([
      "--bootstrap-token-file",
      "/tmp/bootstrap-token",
      "--model-config-mode",
      "user-config"
    ]),
    /native-config/
  );
});

test("default runtime command falls back to configured search paths", () => {
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "teamharness-command-"));
  const command = path.join(tempDir, "claude");
  fs.writeFileSync(command, "#!/bin/sh\n");
  fs.chmodSync(command, 0o755);
  try {
    withPath("", () => {
      const args = parseRuntimeArgs([], {
        commandName: "claudeCommand",
        commandDefault: "claude",
        commandArg: "--claude-command",
        commandSearchPaths: [command]
      });

      assert.equal(args.claudeCommand, command);
    });
  } finally {
    fs.rmSync(tempDir, { recursive: true, force: true });
  }
});

test("explicit runtime command skips fallback search paths", () => {
  const args = parseRuntimeArgs([
    "--claude-command",
    "/custom/claude"
  ], {
    commandName: "claudeCommand",
    commandDefault: "claude",
    commandArg: "--claude-command",
    commandSearchPaths: ["/fallback/claude"]
  });

  assert.equal(args.claudeCommand, "/custom/claude");
});

test("environment runtime command skips fallback search paths", () => {
  withEnv("TEAMHARNESS_CLAUDE_COMMAND", "/env/claude", () => {
    const args = parseRuntimeArgs([], {
      commandName: "claudeCommand",
      commandEnvVars: ["TEAMHARNESS_CLAUDE_COMMAND"],
      commandDefault: "claude",
      commandArg: "--claude-command",
      commandSearchPaths: ["/fallback/claude"]
    });

    assert.equal(args.claudeCommand, "/env/claude");
  });
});
