/**
 * E2E bootstrap ordering (#1470).
 *
 * `scripts/e2e-openclaw.sh` used to write `channels.agentteams-matrix` into
 * the isolated config and then run `openclaw plugins install` directly. The
 * real CLI validates the whole config on every command, so a fresh home
 * aborted with `unknown channel id: agentteams-matrix` before the plugin or
 * the Gateway ever ran. The bootstrap now goes through
 * `scripts/e2e-bootstrap.sh`, which installs first, writes the channel with
 * the shared lifecycle helper, enables, and validates.
 *
 * The fake CLI below applies the same rule (`unknown channel id` for an
 * unregistered plugin, `config validate` re-checks it), so the old order
 * fails here exactly as it failed natively.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, writeFileSync, mkdirSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const PLUGIN_ROOT = resolve(HERE, "..", "..");
const BOOTSTRAP = join(PLUGIN_ROOT, "scripts", "e2e-bootstrap.sh");

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

has_channel() { jq -e --arg id "$ID" '.channels[$id] // empty' "$CONFIG" >/dev/null 2>&1; }

case "\${1:-}" in
  plugins)
    case "\${2:-}" in
      install)
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
      reload) : ;;
      list)
        if [ "\${3:-}" = "--json" ]; then
          [ -f "$registered" ] && printf '[{"id":"%s"}]\\n' "$ID" || printf '[]\\n'
        fi
        ;;
    esac
    ;;
  config)
    if [ "\${2:-}" = "validate" ]; then
      if has_channel && [ ! -f "$registered" ]; then
        echo '{"valid":false,"issues":["unknown channel id: '"$ID"'"]}'
        exit 1
      fi
      echo '{"valid":true,"issues":[]}'
    fi
    ;;
esac
exit 0
`;

interface Box {
  config: string;
  env: Record<string, string>;
  calls(): string[];
}

function sandbox(baseConfig: Record<string, unknown>): Box {
  const dir = mkdtempSync(join(tmpdir(), "agentteams-e2e-bootstrap-"));
  const bin = join(dir, "bin");
  mkdirSync(bin, { recursive: true });
  const openclaw = join(bin, "openclaw");
  writeFileSync(openclaw, FAKE_CLI, { mode: 0o755 });
  chmodSync(openclaw, 0o755);
  const config = join(dir, "openclaw.json");
  writeFileSync(config, JSON.stringify(baseConfig, null, 2) + "\n");
  const state = join(dir, "cli-state");
  return {
    config,
    env: {
      OPENCLAW_BIN: openclaw,
      OPENCLAW_CONFIG_PATH: config,
      FAKE_OPENCLAW_STATE: state,
      E2E_RELEASE_DIR: PLUGIN_ROOT,
      AGENTTEAMS_OPENCLAW_STATE_DIR: join(dir, "state"),
      AGENTTEAMS_MATRIX_SOCKET: "/tmp/e2e/sidecar.sock",
      AGENTTEAMS_MATRIX_AGENT: "e2e-agent",
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

function runBootstrap(box: Box, extra: Record<string, string> = {}): void {
  execFileSync("bash", [BOOTSTRAP], {
    env: { ...process.env, ...box.env, ...extra },
    stdio: "pipe",
    encoding: "utf-8",
  });
}

describe("e2e bootstrap", () => {
  it("installs first, then writes and enables the channel, then validates", () => {
    const box = sandbox({ gateway: { mode: "local", auth: { mode: "none" } } });
    runBootstrap(box);

    const cfg = JSON.parse(readFileSync(box.config, "utf-8"));
    assert.equal("agentteams-matrix" in cfg, false);
    assert.equal(cfg.channels["agentteams-matrix"].enabled, true);
    assert.equal(cfg.channels["agentteams-matrix"].agent, "e2e-agent");
    assert.equal(cfg.channels["agentteams-matrix"].socketPath, "/tmp/e2e/sidecar.sock");

    const calls = box.calls();
    const install = calls.findIndex((line) => line.startsWith("plugins install"));
    const enable = calls.findIndex((line) => line.startsWith("plugins enable"));
    const validate = calls.findIndex((line) => line.startsWith("config validate"));
    assert.ok(install >= 0, `calls: ${calls.join(" | ")}`);
    assert.ok(enable > install, `calls: ${calls.join(" | ")}`);
    assert.ok(validate > enable, `calls: ${calls.join(" | ")}`);
  });

  it("refuses a base config that already carries the channel", () => {
    const box = sandbox({
      channels: { "agentteams-matrix": { enabled: true, socketPath: "/old", agent: "old" } },
    });
    assert.throws(() => runBootstrap(box), /already contains channels\.agentteams-matrix/);
    // The config is untouched.
    assert.equal(
      JSON.parse(readFileSync(box.config, "utf-8")).channels["agentteams-matrix"].agent,
      "old",
    );
  });

  it("the e2e script bootstraps through the helper and writes no channel itself", () => {
    const script = readFileSync(join(PLUGIN_ROOT, "scripts", "e2e-openclaw.sh"), "utf-8");
    assert.match(script, /e2e-bootstrap\.sh/, "the e2e must delegate its bootstrap");
    // The base-config heredoc must not carry the channel section: writing it
    // before the install is exactly what aborted the native run.
    const configBlock = script.slice(script.indexOf("Write the isolated OpenClaw config"));
    const heredoc = configBlock.slice(0, configBlock.indexOf("PYEOF"));
    assert.equal(/"channels"\s*:/.test(heredoc), false, "base config must not contain channels.<id>");
  });

  it("leaves the config untouched when the install fails", () => {
    const box = sandbox({ gateway: { mode: "local" } });
    const before = readFileSync(box.config, "utf-8");
    assert.throws(() => runBootstrap(box, { FAKE_FAIL_INSTALL: "1" }));
    assert.equal(readFileSync(box.config, "utf-8"), before);
    assert.equal(JSON.parse(readFileSync(box.config, "utf-8")).channels, undefined);
  });
});
