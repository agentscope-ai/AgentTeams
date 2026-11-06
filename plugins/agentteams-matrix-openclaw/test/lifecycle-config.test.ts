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
 *
 * The "uninstall failure path" block drives the real `uninstall.sh` with a
 * fake `openclaw` CLI to pin the three-valued inventory contract: an
 * unreadable inventory (command failure or malformed JSON) must abort the
 * uninstall and keep the `active`/`previous` release markers, while a
 * listed plugin is removed and an established absence is a clean no-op.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
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

/**
 * Uninstall failure-path regressions (#1470).
 *
 * `lifecycle_is_listed` is three-valued: listed / absent / unreadable.
 * The uninstaller used to collapse "inventory command exited non-zero"
 * and "plugin not in the inventory" into one outcome — with a corrupted
 * OpenClaw config, `plugins list --json` exits 1, the uninstaller then
 * reported "nothing to uninstall", deleted the `active`/`previous` release
 * markers, and exited 0 with "Uninstall complete" while the plugin was
 * still installed. These tests run the real `uninstall.sh` against a fake
 * `openclaw` CLI that reproduces each inventory state:
 *
 *   - unreadable inventory (command fails, or malformed JSON) -> non-zero
 *     exit, release markers KEPT, no "Uninstall complete";
 *   - listed plugin -> authoritative `plugins uninstall` invoked, markers
 *     cleared, exit 0;
 *   - established absence (empty inventory) -> no-op, markers cleared,
 *     exit 0 (idempotency is preserved).
 */
describe("uninstall failure path", () => {
  type InventoryMode = "corrupt" | "malformed" | "listed" | "absent";

  /**
   * Fake `openclaw` CLI. Its behavior is driven by a mode file, and every
   * invocation is appended to a call log so the tests can assert which
   * subcommands the uninstaller actually ran (no silent bypass).
   */
  function fakeCli(dir: string, mode: InventoryMode): string {
    const state = join(dir, "fake-cli-state");
    mkdirSync(state, { recursive: true });
    const callLog = join(state, "calls.log");
    const cli = join(dir, "fake-openclaw");
    const script = `#!/usr/bin/env bash
echo "$*" >> "${callLog}"
case "$1 $2" in
  "plugins list")
    case "$(cat "${state}/mode")" in
      corrupt)   echo "fatal: failed to load openclaw config" >&2; exit 1 ;;
      malformed) echo '{"plugins": [' ;;
      listed)    echo '{"plugins": [{"id": "agentteams-matrix", "enabled": true}]}' ;;
      absent)    echo '{"plugins": []}' ;;
      *)         echo "fake-openclaw: unknown mode" >&2; exit 64 ;;
    esac
    ;;
  "plugins uninstall")
    echo "uninstalled $3" >&2
    ;;
  *)
    echo "fake-openclaw: unhandled: $*" >&2
    exit 64
    ;;
esac
`;
    writeFileSync(cli, script, { mode: 0o755 });
    writeFileSync(join(state, "mode"), mode + "\n");
    return cli;
  }

  /** A sandbox that looks like an existing installed state: both markers set. */
  function installedSandbox(mode: InventoryMode): {
    active: string;
    previous: string;
    callLog: string;
    env: Record<string, string>;
  } {
    const dir = mkdtempSync(join(tmpdir(), "agentteams-uninstall-"));
    const state = join(dir, "state");
    const releaseDir = join(state, "releases", "0.1.0");
    mkdirSync(releaseDir, { recursive: true });
    const active = join(state, "active");
    const previous = join(state, "previous");
    writeFileSync(active, releaseDir + "\n");
    writeFileSync(previous, releaseDir + "\n");
    const cli = fakeCli(dir, mode);
    return {
      active,
      previous,
      callLog: join(dir, "fake-cli-state", "calls.log"),
      env: {
        OPENCLAW_CONFIG_PATH: join(dir, "openclaw.json"),
        AGENTTEAMS_OPENCLAW_STATE_DIR: state,
        OPENCLAW_BIN: cli,
        AGENTTEAMS_MATRIX_SOCKET: "/run/agentteams/matrix.sock",
        AGENTTEAMS_MATRIX_AGENT: "agentteams-test",
      },
    };
  }

  function runUninstall(env: Record<string, string>) {
    return spawnSync("bash", [join(PLUGIN_ROOT, "uninstall.sh")], {
      env: { ...process.env, ...env },
      encoding: "utf-8",
    });
  }

  it("an unreadable inventory (list command fails) aborts and keeps the release markers", () => {
    const { active, previous, callLog, env } = installedSandbox("corrupt");
    const res = runUninstall(env);

    assert.notEqual(res.status, 0, "must fail, not report success:\n" + res.stderr);
    assert.ok(res.stderr.includes("cannot read the plugin inventory"), res.stderr);
    assert.ok(!res.stderr.includes("nothing to uninstall"), res.stderr);
    assert.ok(!res.stderr.includes("Uninstall complete"), res.stderr);
    // The release markers must survive so a later retry/rollback works.
    assert.ok(existsSync(active), "active marker must be kept");
    assert.ok(existsSync(previous), "previous marker must be kept");
    // The inventory was actually consulted (no silent bypass), and no
    // removal was attempted.
    const calls = readFileSync(callLog, "utf-8");
    assert.ok(calls.includes("plugins list --json"), calls);
    assert.ok(!calls.includes("plugins uninstall"), calls);
  });

  it("a malformed inventory (invalid JSON, exit 0) aborts and keeps the release markers", () => {
    const { active, previous, callLog, env } = installedSandbox("malformed");
    const res = runUninstall(env);

    assert.notEqual(res.status, 0, "must fail, not report success:\n" + res.stderr);
    assert.ok(res.stderr.includes("cannot read the plugin inventory"), res.stderr);
    assert.ok(!res.stderr.includes("Uninstall complete"), res.stderr);
    assert.ok(existsSync(active), "active marker must be kept");
    assert.ok(existsSync(previous), "previous marker must be kept");
    assert.ok(!readFileSync(callLog, "utf-8").includes("plugins uninstall"));
  });

  it("a listed plugin is removed authoritatively and the markers are cleared", () => {
    const { active, previous, callLog, env } = installedSandbox("listed");
    const res = runUninstall(env);

    assert.equal(res.status, 0, res.stderr);
    assert.ok(res.stderr.includes("Uninstall complete"), res.stderr);
    const calls = readFileSync(callLog, "utf-8");
    assert.ok(calls.includes("plugins uninstall agentteams-matrix --force"), calls);
    assert.ok(!existsSync(active), "active marker must be cleared after removal");
    assert.ok(!existsSync(previous), "previous marker must be cleared after removal");
  });

  it("an established absence is a clean no-op that clears the markers", () => {
    const { active, previous, callLog, env } = installedSandbox("absent");
    const res = runUninstall(env);

    assert.equal(res.status, 0, res.stderr);
    assert.ok(res.stderr.includes("nothing to uninstall"), res.stderr);
    assert.ok(res.stderr.includes("Uninstall complete"), res.stderr);
    assert.ok(!readFileSync(callLog, "utf-8").includes("plugins uninstall"));
    assert.ok(!existsSync(active));
    assert.ok(!existsSync(previous));
  });
});


describe("release preservation", () => {
  it("keeps distinct rollback releases for two fresh builds in the same second", () => {
    const dir = mkdtempSync(join(tmpdir(), "agentteams-release-"));
    const source = join(dir, "source");
    const state = join(dir, "state");
    for (const path of ["dist/src", "src", "scripts", "node_modules"]) {
      mkdirSync(join(source, path), { recursive: true });
    }
    writeFileSync(join(source, "package.json"), '{"version":"0.1.0"}');
    writeFileSync(join(source, "openclaw.plugin.json"), "{}");
    writeFileSync(join(source, "README.md"), "fixture");
    writeFileSync(join(source, "dist/src/index.js"), "fixture");
    const initial = join(state, "releases", "0.1.0", "dist", "src");
    mkdirSync(initial, { recursive: true });
    writeFileSync(join(initial, "index.js"), "original");
    const output = execFileSync("bash", ["-c", `
      set -euo pipefail
      source "$LIFECYCLE_LIB"
      PLUGIN_ROOT="$FIXTURE_SOURCE"
      npm() { :; }
      date() { printf '20261106T000000Z\\n'; }
      first="$(lifecycle_build_release fresh)"
      printf 'preserve active' > "$first/sentinel"
      second="$(lifecycle_build_release fresh)"
      printf '%s\\n' "$first" "$second"
    `], {
      env: {
        ...process.env,
        HOME: join(dir, "home"),
        LIFECYCLE_LIB: LIB,
        FIXTURE_SOURCE: source,
        AGENTTEAMS_OPENCLAW_STATE_DIR: state,
      },
      encoding: "utf-8",
    });
    const [first, second] = output.trim().split("\n");
    assert.notEqual(first, second);
    assert.equal(readFileSync(join(first!, "sentinel"), "utf-8"), "preserve active");
    assert.equal(readFileSync(join(initial, "index.js"), "utf-8"), "original");
    assert.ok(existsSync(join(second!, "dist/src/index.js")));
  });
});
