/**
 * Lifecycle config regressions (#1470).
 *
 * The install/upgrade/rollback scripts write the channel section into the
 * OpenClaw config. The section lives at `channels.<id>` — a platform key at
 * the config root is rejected by the OpenClaw schema with
 * `Unrecognized key: "agentteams-matrix"` — so these tests run the real
 * `scripts/lib-lifecycle.sh` helpers against real config files and assert
 * the before/after shape:
 *
 *   - the section lands under `channels`, never at the root;
 *   - every other channel and every unrelated top-level setting survives;
 *   - keys inside our own section that the scripts do not manage survive;
 *   - purge removes only our channel + plugin entry;
 *   - the file mode is preserved;
 *   - when the real `openclaw` CLI can run on this host (Node 24+), the
 *     resulting config validates against the active schema. The check
 *     skips cleanly on hosts where the CLI refuses to start (it aborts on
 *     Node < 24), so the shape assertions above are the always-on gate.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const PLUGIN_ROOT = resolve(HERE, "..", "..");
const LIB = join(PLUGIN_ROOT, "scripts", "lib-lifecycle.sh");
const OPENCLAW_BIN = join(PLUGIN_ROOT, "node_modules", ".bin", "openclaw");

type Json = Record<string, unknown>;

function readJson(path: string): Json {
  return JSON.parse(readFileSync(path, "utf-8")) as Json;
}

function runLifecycle(fn: "lifecycle_apply_channel_config" | "lifecycle_purge_channel_config", env: Record<string, string>): void {
  execFileSync("bash", ["-c", `set -euo pipefail; source "${LIB}"; ${fn}`], {
    env: { ...process.env, ...env },
    stdio: "pipe",
  });
}

function sandbox(): { config: string; env: Record<string, string> } {
  const dir = mkdtempSync(join(tmpdir(), "agentteams-lifecycle-"));
  const config = join(dir, "openclaw.json");
  return {
    config,
    env: {
      OPENCLAW_CONFIG_PATH: config,
      AGENTTEAMS_OPENCLAW_STATE_DIR: join(dir, "state"),
      AGENTTEAMS_MATRIX_SOCKET: "/run/agentteams/matrix.sock",
      AGENTTEAMS_MATRIX_AGENT: "agentteams-test",
    },
  };
}

function seedConfig(path: string): void {
  writeFileSync(
    path,
    JSON.stringify(
      {
        gateway: { mode: "local", auth: { mode: "none" } },
        channels: {
          slack: { enabled: true, botToken: "xoxb-keep-me" },
          "agentteams-matrix": {
            enabled: false,
            socketPath: "/old.sock",
            agent: "old-agent",
            dmPolicy: "allowlist",
          },
        },
        plugins: {
          entries: {
            other: { enabled: true },
            "agentteams-matrix": { enabled: false },
          },
        },
      },
      null,
      2,
    ) + "\n",
    { mode: 0o640 },
  );
}

/**
 * The CLI aborts on Node < 24 (node:sqlite NUL-truncation guard) while
 * `--version` still exits 0, so the probe must exercise the validating
 * command itself: a minimal known-valid config, `config validate --json`,
 * and a skip when the CLI refuses to run on this host.
 */
function cliUsable(): boolean {
  const dir = mkdtempSync(join(tmpdir(), "agentteams-cli-probe-"));
  const probe = join(dir, "openclaw.json");
  writeFileSync(
    probe,
    JSON.stringify({ gateway: { mode: "local", auth: { mode: "none" } } }),
  );
  const res = spawnSync(OPENCLAW_BIN, ["config", "validate", "--json"], {
    env: { ...process.env, OPENCLAW_CONFIG_PATH: probe },
    encoding: "utf-8",
    timeout: 60_000,
  });
  if (res.status !== 0) return false;
  try {
    return (JSON.parse(res.stdout) as { valid?: boolean }).valid === true;
  } catch {
    return false;
  }
}

function validateWithCli(config: string): { valid: boolean; issues?: unknown } {
  const out = execFileSync(OPENCLAW_BIN, ["config", "validate", "--json"], {
    env: { ...process.env, OPENCLAW_CONFIG_PATH: config },
    stdio: "pipe",
    timeout: 60_000,
  }).toString();
  const parsed = JSON.parse(out) as { valid?: boolean; issues?: unknown };
  return { valid: parsed.valid === true, issues: parsed.issues };
}

describe("lifecycle channel config", () => {
  it("writes channels.<id>, preserves every other setting, and keeps our unmanaged keys", () => {
    const { config, env } = sandbox();
    seedConfig(config);

    runLifecycle("lifecycle_apply_channel_config", env);

    const after = readJson(config);
    // The section is nested under channels/ — never a root-level key.
    assert.equal("agentteams-matrix" in after, false, "must not write a root-level platform key");
    const channels = after.channels as Json;
    const section = channels["agentteams-matrix"] as Json;
    assert.equal(section.enabled, true);
    assert.equal(section.socketPath, "/run/agentteams/matrix.sock");
    assert.equal(section.agent, "agentteams-test");
    // Keys the scripts do not manage survive inside our own section.
    assert.equal(section.dmPolicy, "allowlist");
    // Every other channel and unrelated top-level setting is untouched.
    assert.deepEqual(channels.slack, { enabled: true, botToken: "xoxb-keep-me" });
    assert.deepEqual(after.gateway, { mode: "local", auth: { mode: "none" } });
    assert.deepEqual((after.plugins as Json).entries, {
      other: { enabled: true },
      "agentteams-matrix": { enabled: false },
    });
    // The update is idempotent.
    const first = readFileSync(config, "utf-8");
    runLifecycle("lifecycle_apply_channel_config", env);
    assert.equal(readFileSync(config, "utf-8"), first);
  });

  it("creates a fresh config with the section nested under channels", () => {
    const { config, env } = sandbox(); // no file yet
    runLifecycle("lifecycle_apply_channel_config", env);
    const after = readJson(config);
    assert.deepEqual(Object.keys(after), ["channels"]);
    const section = (after.channels as Json)["agentteams-matrix"] as Json;
    assert.equal(section.enabled, true);
    assert.equal(section.agent, "agentteams-test");
  });

  it("preserves the config file mode", () => {
    const { config, env } = sandbox();
    seedConfig(config);
    runLifecycle("lifecycle_apply_channel_config", env);
    assert.equal(statSync(config).mode & 0o777, 0o640);
  });

  it("purges only our channel and plugin entry", () => {
    const { config, env } = sandbox();
    seedConfig(config);
    runLifecycle("lifecycle_apply_channel_config", env);

    runLifecycle("lifecycle_purge_channel_config", env);

    const after = readJson(config);
    const channels = after.channels as Json;
    assert.equal("agentteams-matrix" in channels, false);
    assert.deepEqual(channels.slack, { enabled: true, botToken: "xoxb-keep-me" });
    const entries = (after.plugins as Json).entries as Json;
    assert.equal("agentteams-matrix" in entries, false);
    assert.deepEqual(entries.other, { enabled: true });
    assert.deepEqual(after.gateway, { mode: "local", auth: { mode: "none" } });
  });

  it("validates against the real OpenClaw schema when the CLI can run here", (t) => {
    if (!cliUsable()) {
      t.skip("openclaw CLI does not run on this host (it requires Node 24+); shape assertions above are the gate");
      return;
    }
    const { config, env } = sandbox();
    seedConfig(config);
    runLifecycle("lifecycle_apply_channel_config", env);

    const result = validateWithCli(config);
    assert.equal(result.valid, true, `openclaw config validate: ${JSON.stringify(result.issues)}`);
  });
});
