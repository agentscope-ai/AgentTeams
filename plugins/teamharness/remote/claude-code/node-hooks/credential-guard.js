#!/usr/bin/env node
"use strict";

let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", chunk => {
  input += chunk;
});
process.stdin.on("end", () => {
  let payload = {};
  try {
    payload = input.trim() ? JSON.parse(input) : {};
  } catch {
    payload = {};
  }
  const text = JSON.stringify(payload.tool_input || {});
  const sensitiveFiles = [
    "credential-token",
    "credential-broker.json",
    "model-env",
    "edge-token.json",
    "sts.json",
    "launcher.json"
  ];
  const pattern = new RegExp(`(\\.env|secret|token|password|credential|api[_-]?key|${sensitiveFiles.join("|")})`, "i");
  if (!pattern.test(text)) {
    return;
  }
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      additionalContext: "TeamHarness credential guard noticed a credential-like path or token. Do not expose secrets in Matrix, shared files, prompts, or tool output."
    }
  }));
});
