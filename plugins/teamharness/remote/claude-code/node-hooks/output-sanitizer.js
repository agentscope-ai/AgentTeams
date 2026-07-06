#!/usr/bin/env node
"use strict";

let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", chunk => {
  input += chunk;
});
process.stdin.on("end", () => {
  const patterns = [
    /sk-[A-Za-z0-9_-]{16,}/,
    /AKIA[0-9A-Z]{16}/,
    /Bearer\s+[A-Za-z0-9._~+/=-]{12,}/,
    /(access_key_id|accessKeyId|access_key_secret|accessKeySecret|security_token|securityToken|gatewayKey|accessToken|ANTHROPIC_API_KEY)\s*[:=]/i
  ];
  if (!patterns.some(pattern => pattern.test(input))) {
    return;
  }
  process.stdout.write(JSON.stringify({
    hookSpecificOutput: {
      hookEventName: "PostToolUse",
      additionalContext: "Potential secret-like output detected. Redact before sharing."
    }
  }));
});
