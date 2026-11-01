/**
 * Install order regressions (#1470).
 *
 * A fresh OpenClaw home rejects a config that references a channel whose
 * plugin is not registered yet (`unknown channel id: <id>`), and the CLI
 * validates the config on every command. `lifecycle_install_release()` used
 * to write `channels.agentteams-matrix` BEFORE `openclaw plugins install`,
 * so the install failed and left the invalid section behind.
 *
 * These tests run the real `scripts/lib-lifecycle.sh` against a fake
 * `openclaw` binary that reproduces that validation rule:
 *
 *   - install on a fresh config registers the plugin and succeeds, and the
 *     channel section is only written afterwards;
 *   - a failed install leaves the config untouched (no dangling channel);
 *   - an existing unrelated channel survives both paths.
 *
 * The `unknown channel id` check is the same rule the real CLI applies, so
 * the old order fails here exactly as it failed on the maintainer's host.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const PLUGIN_ROOT = resolve(HERE, "..", "..");
const LIB = join(PLUGIN_ROOT, "scripts", "lib-lifecycle.sh");

const FAKE_CLI = `#!/usr/bin/env bash
set -u
CONFIG="\${OPENCLAW_CONFIG_PATH:?}"
STATE="\${FAKE_OPENCLAW_STATE:?}"
ID="agentteams-matrix"
mkdir -p "$STATE"
registered="$STATE/registered"
enabled="$STATE/enabled"
log="$STATE/calls.log"
echo "$*" >> "$log"

has_channel() {
  jq -e --arg id "$ID" '.channels[$id] // empty' "$CONFIG" >/dev/null 2>&1
}

case "\${1:-}" in
  plugins)
    case "\${2:-}" in
      install)
        # The real CLI validates the whole config first: a channel section
        # for an unregistered plugin is rejected outright.
        if has_channel && [ ! -f "$registered" ]; then
          echo "unknown channel id: $ID" >&2
          exit 1
        fi
        if [ "\${FAKE_FAIL_INSTALL:-0}" = "1" ]; then
          echo "install failed (scripted)" >&2
          exit 1
        fi
        touch "$registered"
        ;;
      enable)
        [ -f "$registered" ] || { echo "unknown plugin: $ID" >&2; exit 1; }
        touch "$enabled"
        ;;
      reload)
        : ;;
      list)
        if [ "\${3:-}" = "--json" ]; then
          if [ -f "$registered" ]; then
            printf '[{"id":"%s"}]\n' "$ID"
          else
            printf '[]\n'
          fi
        elif [ -f "$enabled" ]; then
          echo "$ID"
        fi
        ;;
    esac
    ;;
esac
exit 0
`;

interface Sandbox {
  config: string;
  env: Record<string, string>;
  calls(): string[];
}

function sandbox(seed?: Record<string, unknown>): Sandbox {
  const dir = mkdtempSync(join(tmpdir(), "agentteams-install-order-"));
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  const openclaw = join(bin, "openclaw");
  writeFileSync(openclaw, FAKE_CLI, { mode: 0o755 });
  chmodSync(openclaw, 0o755);
  const config = join(dir, "openclaw.json");
  writeFileSync(config, JSON.stringify(seed ?? {}, null, 2) + "\n");
  const state = join(dir, "cli-state");
  return {
    config,
    env: {
      OPENCLAW_BIN: openclaw,
      OPENCLAW_CONFIG_PATH: config,
      FAKE_OPENCLAW_STATE: state,
      AGENTTEAMS_OPENCLAW_STATE_DIR: join(dir, "state"),
      AGENTTEAMS_MATRIX_SOCKET: "/run/agentteams/matrix.sock",
      AGENTTEAMS_MATRIX_AGENT: "agentteams-test",
    },
    calls: () => {
      try {
        return readFileSync(join(state, "calls.log"), "utf-8").trim().split("\n");
      } catch {
        return [];
      }
    },
  };
}

function runInstall(env: Record<string, string>, extraEnv: Record<string, string> = {}) {
  return execFileSync(
    "bash",
    ["-c", `set -euo pipefail; source "${LIB}"; lifecycle_install_release "${PLUGIN_ROOT}"`],
    {
      env: { ...process.env, ...env, ...extraEnv },
      stdio: "pipe",
      encoding: "utf-8",
    },
  );
}

function configOf(path: string): Record<string, any> {
  return JSON.parse(readFileSync(path, "utf-8"));
}

describe("install order", () => {
  it("installs a fresh home: plugin first, then the channel section", () => {
    const box = sandbox();
    runInstall(box.env);

    const cfg = configOf(box.config);
    assert.equal("agentteams-matrix" in cfg, false, "channel section must stay nested under channels/");
    assert.equal(cfg.channels["agentteams-matrix"].enabled, true);

    const calls = box.calls();
    const installIndex = calls.findIndex((line) => line.startsWith("plugins install"));
    const enableIndex = calls.findIndex((line) => line.startsWith("plugins enable"));
    assert.ok(installIndex >= 0 && enableIndex > installIndex, `calls: ${calls.join(" | ")}`);
  });

  it("writes the channel only after a successful install", () => {
    const box = sandbox();
    // The fake refuses the config mid-install if the section is already
    // there; a success proves the write happened after registration.
    runInstall(box.env);
    assert.ok(box.calls().some((line) => line.startsWith("plugins install")));
    assert.equal(configOf(box.config).channels["agentteams-matrix"].agent, "agentteams-test");
  });

  it("leaves the config untouched when the install fails", () => {
    const seed = { gateway: { mode: "local" }, channels: { slack: { enabled: true } } };
    const box = sandbox(seed);
    const before = readFileSync(box.config, "utf-8");

    assert.throws(() => runInstall(box.env, { FAKE_FAIL_INSTALL: "1" }));

    assert.equal(readFileSync(box.config, "utf-8"), before);
    assert.equal(configOf(box.config).channels["agentteams-matrix"], undefined);
  });

  it("keeps unrelated channels on success", () => {
    const box = sandbox({ channels: { slack: { enabled: true, botToken: "xoxb-keep" } } });
    runInstall(box.env);
    const cfg = configOf(box.config);
    assert.deepEqual(cfg.channels.slack, { enabled: true, botToken: "xoxb-keep" });
    assert.equal(cfg.channels["agentteams-matrix"].enabled, true);
  });
});
