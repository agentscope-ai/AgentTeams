#!/usr/bin/env node
"use strict";

const fs = require("fs");

function shellQuote(value) {
  return `'${String(value).replace(/'/g, "'\\''")}'`;
}

function fail(message) {
  process.stdout.write(`echo ${shellQuote(`error: credential broker unavailable for find-skills: ${message}`)} >&2\n`);
  process.stdout.write("exit 1\n");
}

async function main() {
  const descriptorPath = process.argv[2];
  const descriptor = JSON.parse(fs.readFileSync(descriptorPath, "utf8"));
  const endpoint = String(descriptor.endpoint || "").replace(/\/+$/, "");
  const tokenFile = String(descriptor.tokenFile || "");
  const token = tokenFile ? fs.readFileSync(tokenFile, "utf8").trim() : "";
  if (!endpoint || !token) {
    throw new Error("descriptor missing endpoint or token file");
  }
  const response = await fetch(`${endpoint}/v1/credentials/skill-registry`, {
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: "application/json"
    }
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`broker returned ${response.status}: ${text}`);
  }
  const payload = text.trim() ? JSON.parse(text) : {};
  const mapping = {
    SKILLS_API_URL: payload.url,
    NACOS_AUTH_TYPE: payload.authType,
    HICLAW_NACOS_STS_ACCESS_KEY: payload.accessKeyId,
    HICLAW_NACOS_STS_SECRET_KEY: payload.accessKeySecret,
    HICLAW_NACOS_STS_SECURITY_TOKEN: payload.securityToken
  };
  for (const [key, value] of Object.entries(mapping)) {
    if (value) {
      process.stdout.write(`${key}=${shellQuote(value)}\n`);
    }
  }
}

main().catch(error => {
  fail(error.message);
});
