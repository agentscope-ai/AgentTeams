#!/usr/bin/env node
/**
 * Runtime SDK verification for the agentteams-matrix OpenClaw plugin.
 *
 * Type-checking (tsc) only proves the SDK's .d.ts matches our source; this
 * script proves the INSTALLED SDK actually loads and our built plugin entry
 * imports against it at RUNTIME:
 *
 *   1. `openclaw/plugin-sdk/channel-core` imports and exports the
 *      `defineChannelPluginEntry` function we use in src/index.ts.
 *   2. `openclaw/plugin-sdk/channel-config-schema` imports and exports
 *      `buildJsonChannelConfigSchema` (used by src/plugin.ts).
 *   3. `openclaw/plugin-sdk/channel-inbound` imports (type-only import in
 *      src/plugin.ts; the module must still exist at runtime).
 *   4. The BUILT entry (dist/src/index.js, produced by `npm run build`)
 *      default-exports a channel plugin entry object with the expected
 *      identity and callbacks, and dist/src/plugin.js exposes the plugin
 *      factory plus the setRuntime/clearRuntime seams.
 *
 * Usage:  npm run build && node scripts/verify-sdk.mjs
 * Exits 0 when every check passes, 1 otherwise (one line per failure).
 */

import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const failures = [];

async function check(name, fn) {
  try {
    await fn();
    console.log(`  ok: ${name}`);
  } catch (err) {
    failures.push(`${name}: ${err instanceof Error ? err.message : String(err)}`);
    console.error(`FAIL: ${name}`);
    console.error(String(err && err.stack ? err.stack : err));
  }
}

if (!existsSync(`${root}dist/src/index.js`)) {
  console.error("dist/src/index.js is missing — run `npm run build` first.");
  process.exit(1);
}

await check("channel-core exports defineChannelPluginEntry (function)", async () => {
  const sdk = await import("openclaw/plugin-sdk/channel-core");
  assert.equal(typeof sdk.defineChannelPluginEntry, "function");
});

await check("channel-config-schema exports buildJsonChannelConfigSchema (function)", async () => {
  const sdk = await import("openclaw/plugin-sdk/channel-config-schema");
  assert.equal(typeof sdk.buildJsonChannelConfigSchema, "function");
});

await check("channel-inbound module loads (type-only import target)", async () => {
  const sdk = await import("openclaw/plugin-sdk/channel-inbound");
  assert.equal(typeof sdk, "object");
});

await check("built entry default-exports the agentteams-matrix plugin entry", async () => {
  const entryModule = await import(`${root}dist/src/index.js`);
  const entry = entryModule.default;
  // The shape `defineChannelPluginEntry` actually returns (OpenClaw
  // 2026.10.9, `DefinedChannelPluginEntry`): the options are named
  // `plugin`/`setRuntime`, the RESULT exposes `channelPlugin`/
  // `setChannelRuntime`. Asserting the option names on the result was
  // wrong (it is what this script used to do).
  assert.equal(typeof entry, "object", "default export must be an entry object");
  assert.equal(entry.id, "agentteams-matrix");
  assert.equal(typeof entry.name, "string");
  assert.equal(typeof entry.description, "string");
  assert.equal(typeof entry.register, "function", "register missing");
  assert.equal(typeof entry.channelPlugin, "object", "channelPlugin missing");
  assert.equal(entry.channelPlugin.id, "agentteams-matrix");
  assert.equal(typeof entry.setChannelRuntime, "function", "setChannelRuntime missing");
  // Channel plugin surfaces the gateway relies on.
  assert.equal(typeof entry.channelPlugin.gateway.startAccount, "function");
  assert.equal(typeof entry.channelPlugin.gateway.stopAccount, "function");
  assert.equal(typeof entry.channelPlugin.outbound.sendText, "function");
  assert.equal(typeof entry.channelPlugin.config.resolveAccount, "function");
});

await check("setRuntime accepts a runtime object without throwing (no-op store)", async () => {
  const entryModule = await import(`${root}dist/src/index.js`);
  const pluginModule = await import(`${root}dist/src/plugin.js`);
  assert.equal(typeof pluginModule.setRuntime, "function");
  assert.equal(typeof pluginModule.clearRuntime, "function");
  // The entry's setRuntime only stores the runtime; calling it with a plain
  // object must not throw (the real PluginRuntime is injected by the host).
  entryModule.default.setChannelRuntime({});
  pluginModule.clearRuntime();
});

await check("plugin factory produces a configurable account for channel config", async () => {
  const { createOpenClawMatrixPlugin } = await import(`${root}dist/src/plugin.js`);
  const plugin = createOpenClawMatrixPlugin({
    onInboundMessage: () => {},
  });
  const account = plugin.config.resolveAccount({
    channels: {
      "agentteams-matrix": {
        enabled: true,
        socketPath: "/tmp/verify.sock",
        agent: "verify-agent",
      },
    },
  });
  assert.equal(account.accountId, "default");
  assert.equal(account.enabled, true);
  assert.equal(account.configured, true);
  assert.equal(account.socketPath, "/tmp/verify.sock");
  assert.equal(account.agent, "verify-agent");
  assert.equal(plugin.config.isConfigured(account), true);
});

if (failures.length > 0) {
  console.error(`\nverify-sdk: ${failures.length} check(s) failed`);
  process.exit(1);
}
console.log("\nverify-sdk: all SDK runtime checks passed");
