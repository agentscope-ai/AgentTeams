/**
 * sidecar.sh process identity (#1470 P1).
 *
 * `sidecar.sh` used to treat any live PID found in `sidecar.pid` as the
 * sidecar: a stale PID that the kernel reused for an unrelated process
 * was reported as running, `start` skipped startup, and `stop` /
 * `restart` sent SIGTERM (then SIGKILL) to that unrelated process.
 *
 * These tests run the REAL `sidecar.sh` against the new contract: the
 * pid file records `<pid> <starttime>` (field 22 of /proc/<pid>/stat)
 * and every command re-verifies the process's start identity and its
 * command line before reporting "running" or sending a signal. The
 * "genuine sidecar" controls boot the real `python3 -m sidecar` against
 * the in-tree fake core (sidecar/tests/fake_core) — no network, no real
 * Matrix. Liveness is asserted on the processes themselves (signal 0),
 * not on the scripts' output.
 */

import { describe, it } from "node:test";
import * as assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import {
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const PLUGIN_ROOT = resolve(HERE, "..", "..");
const SIDECAR_SH = join(PLUGIN_ROOT, "sidecar.sh");
const FAKE_CORE = join(PLUGIN_ROOT, "sidecar", "tests", "fake_core");

// ---------------------------------------------------------------------------
// Sandbox
// ---------------------------------------------------------------------------

interface Sandbox {
  root: string;
  stateDir: string;
  configPath: string;
  pidFile: string;
  socketPath: string;
  env: Record<string, string>;
}

function makeSandbox(): Sandbox {
  const root = mkdtempSync(join(tmpdir(), "agentteams-sidecar-identity-"));
  const stateDir = join(root, "state");
  const matrixDir = join(root, "matrix");
  const configPath = join(stateDir, "sidecar-config.json");
  const socketPath = join(matrixDir, "sidecar.sock");
  mkdirSync(stateDir, { recursive: true });
  // The sidecar refuses a config that is not exactly 0600.
  writeFileSync(
    configPath,
    JSON.stringify(
      {
        homeserver_url: "http://fake-homeserver:8008",
        agent_user: "@identity:fake-homeserver",
        agent_access_token: "syt-identity-test",
        socket_path: socketPath,
        agent_name: "identity-test",
      },
      null,
      2,
    ) + "\n",
    { mode: 0o600 },
  );
  return {
    root,
    stateDir,
    configPath,
    pidFile: join(stateDir, "sidecar.pid"),
    socketPath,
    // AGENTTEAMS_MATRIX_STATE_DIR points at the same directory as the
    // config's socket_path, so sidecar.sh's status socket line names the
    // real socket whether it reads the config (jq) or falls back to the
    // documented default.
    env: {
      HOME: root,
      AGENTTEAMS_OPENCLAW_STATE_DIR: stateDir,
      AGENTTEAMS_MATRIX_SIDECAR_CONFIG: configPath,
      AGENTTEAMS_MATRIX_STATE_DIR: matrixDir,
      MATRIX_CORE_DIR: FAKE_CORE,
    },
  };
}

interface CmdResult {
  code: number;
  out: string;
  err: string;
}

function runSidecar(box: Sandbox, cmd: string, extraEnv: Record<string, string> = {}): CmdResult {
  try {
    const out = execFileSync("bash", [SIDECAR_SH, cmd], {
      env: { ...process.env, ...box.env, ...extraEnv },
      stdio: "pipe",
      encoding: "utf-8",
    });
    return { code: 0, out, err: "" };
  } catch (error) {
    const err = error as { status?: number; stdout?: string | Buffer; stderr?: string | Buffer };
    return {
      code: typeof err.status === "number" ? err.status : 1,
      out: err.stdout === undefined ? "" : err.stdout.toString(),
      err: err.stderr === undefined ? "" : err.stderr.toString(),
    };
  }
}

// ---------------------------------------------------------------------------
// Process helpers (the assertions are about processes, not about text)
// ---------------------------------------------------------------------------

function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
}

/** Field 22 of /proc/<pid>/stat (starttime), or null when unreadable. */
function procStarttime(pid: number): string | null {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
    // The "pid (comm)" prefix is stripped at the LAST ")" because comm
    // may contain spaces or parentheses; starttime is the 20th field
    // after it.
    const rest = stat.slice(stat.lastIndexOf(")") + 1);
    const fields = rest
      .trim()
      .split(/\s+/)
      .filter((f) => f.length > 0);
    return fields.length >= 20 ? fields[19] : null;
  } catch {
    return null;
  }
}

/** A starttime guaranteed to differ from the process's own one. */
function wrongStarttime(pid: number): string {
  const real = procStarttime(pid);
  if (real === null) return "999999999";
  const n = Number(real);
  return Number.isFinite(n) ? String(n + 1) : "999999999";
}

/** Block the test thread without spinning the event loop. */
function waitMs(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

function waitForDeath(pid: number, timeoutMs: number): boolean {
  const deadline = Date.now() + timeoutMs;
  while (isAlive(pid)) {
    if (Date.now() > deadline) return false;
    waitMs(50);
  }
  return true;
}

function waitForExit(child: ChildProcess, timeoutMs: number): Promise<void> {
  return new Promise((resolvePromise) => {
    if (child.exitCode !== null || child.signalCode !== null) {
      resolvePromise();
      return;
    }
    const timer = setTimeout(() => resolvePromise(), timeoutMs);
    child.once("exit", () => {
      clearTimeout(timer);
      resolvePromise();
    });
  });
}

function readPidFile(box: Sandbox): string | null {
  try {
    return readFileSync(box.pidFile, "utf-8");
  } catch {
    return null;
  }
}

function pidFilePid(box: Sandbox): number | null {
  const text = readPidFile(box);
  if (text === null) return null;
  const pid = Number(text.trim().split(/\s+/)[0]);
  return Number.isInteger(pid) && pid > 0 ? pid : null;
}

function requirePidFile(box: Sandbox): number {
  const pid = pidFilePid(box);
  if (pid === null) throw new Error(`unexpected pid file: ${readPidFile(box) ?? "<absent>"}`);
  return pid;
}

function isSocket(path: string): boolean {
  try {
    return lstatSync(path).isSocket();
  } catch {
    return false;
  }
}

/** How many live processes look exactly like "-m sidecar --config <config>". */
function countSidecarProcesses(configPath: string): number {
  let count = 0;
  for (const entry of readdirSync("/proc")) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const tokens = readFileSync(`/proc/${entry}/cmdline`, "utf-8")
        .split("\0")
        .filter((t) => t.length > 0);
      const m = tokens.indexOf("-m");
      const c = tokens.indexOf("--config");
      if (
        m >= 0 &&
        m + 1 < tokens.length &&
        tokens[m + 1] === "sidecar" &&
        c >= 0 &&
        c + 1 < tokens.length &&
        tokens[c + 1] === configPath
      ) {
        count += 1;
      }
    } catch {
      // process exited or the entry is unreadable — not a sidecar
    }
  }
  return count;
}

interface Sleeper {
  pid: number;
  child: ChildProcess;
  dispose(): void;
}

/** An unrelated long-lived process whose fate the tests must not touch. */
function spawnSleeper(): Sleeper {
  const child = spawn("sleep", ["300"], { stdio: "ignore" });
  const pid = child.pid;
  if (pid === undefined) throw new Error("the sleep child has no pid");
  const sleeper: Sleeper = {
    pid,
    child,
    dispose() {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        /* already gone */
      }
    },
  };
  return sleeper;
}

/** Kill whatever the pid file still names (safety net for failed tests). */
function killLeftover(box: Sandbox): void {
  const pid = pidFilePid(box);
  if (pid !== null && isAlive(pid)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      /* already gone */
    }
  }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("sidecar.sh process identity", () => {
  it("a stale/reused PID is never 'running' and never receives a signal", () => {
    const box = makeSandbox();
    const sleeper = spawnSleeper();
    try {
      // The pid file names the sleeper with a starttime that is NOT the
      // sleeper's own: the kernel handed this number to a different
      // process than the one recorded (i.e. PID reuse).
      writeFileSync(box.pidFile, `${sleeper.pid} ${wrongStarttime(sleeper.pid)}\n`);

      const status = runSidecar(box, "status");
      assert.notEqual(status.code, 0, `status must fail for a reused PID: ${status.out}`);
      assert.ok(status.out.includes("sidecar not running"), status.out);
      assert.ok(status.out.includes("stale pid file"), status.out);
      assert.ok(!status.out.includes("sidecar running"), status.out);
      assert.ok(existsSync(box.pidFile), "status must not delete the pid file");

      const stop = runSidecar(box, "stop");
      assert.equal(stop.code, 0, `${stop.out} ${stop.err}`);
      assert.ok(stop.out.includes("stale pid file ignored"), stop.out);
      assert.ok(isAlive(sleeper.pid), "stop must not signal the reused PID");
      assert.equal(existsSync(box.pidFile), false, "stop removes the stale pid file");

      // restart = stop + start: the stop phase must not touch the
      // sleeper either, and the start phase must really start. The
      // stale pid file is consumed by the stop phase (deleted there),
      // so the start phase simply starts a fresh sidecar.
      writeFileSync(box.pidFile, `${sleeper.pid} ${wrongStarttime(sleeper.pid)}\n`);
      const restart = runSidecar(box, "restart");
      assert.equal(restart.code, 0, `${restart.out} ${restart.err}`);
      assert.ok(
        restart.out.includes("sidecar not running (stale pid file ignored)"),
        restart.out,
      );
      assert.ok(restart.out.includes("sidecar started (pid "), restart.out);
      assert.ok(isAlive(sleeper.pid), "restart must not signal the reused PID");
      assert.notEqual(pidFilePid(box), sleeper.pid, "the pid file must name the new process");
    } finally {
      killLeftover(box);
      sleeper.dispose();
    }
  });

  it("an old single-field pid file naming an unrelated process is not signaled", () => {
    const box = makeSandbox();
    const sleeper = spawnSleeper();
    try {
      // Old format: a bare pid, no starttime. The command-line check
      // must still protect the process.
      writeFileSync(box.pidFile, `${sleeper.pid}\n`);

      const status = runSidecar(box, "status");
      assert.notEqual(status.code, 0, status.out);
      assert.ok(status.out.includes("sidecar not running"), status.out);

      const stop = runSidecar(box, "stop");
      assert.equal(stop.code, 0, `${stop.out} ${stop.err}`);
      assert.ok(stop.out.includes("stale pid file ignored"), stop.out);
      assert.ok(
        isAlive(sleeper.pid),
        "old-format pid file: the command-line check must still protect the process",
      );
      assert.equal(existsSync(box.pidFile), false, "stop removes the stale pid file");
    } finally {
      sleeper.dispose();
    }
  });

  it("non-numeric and dead PIDs report not running and are never signaled", async () => {
    const box = makeSandbox();
    const victim = spawnSleeper();
    try {
      // (a) a non-numeric pid
      writeFileSync(box.pidFile, "not-a-pid\n");
      let status = runSidecar(box, "status");
      assert.notEqual(status.code, 0, status.out);
      assert.ok(status.out.includes("sidecar not running"), status.out);
      let stop = runSidecar(box, "stop");
      assert.equal(stop.code, 0, `${stop.out} ${stop.err}`);
      assert.ok(stop.out.includes("stale pid file ignored"), stop.out);
      assert.equal(existsSync(box.pidFile), false, "stop removes the stale pid file");

      // (b) a dead pid
      const dead = spawnSleeper();
      dead.child.kill("SIGTERM");
      await waitForExit(dead.child, 5000);
      assert.ok(!isAlive(dead.pid), "the child must actually be dead");
      writeFileSync(box.pidFile, `${dead.pid}\n`);
      status = runSidecar(box, "status");
      assert.notEqual(status.code, 0, status.out);
      assert.ok(status.out.includes("sidecar not running"), status.out);
      stop = runSidecar(box, "stop");
      assert.equal(stop.code, 0, `${stop.out} ${stop.err}`);
      assert.ok(stop.out.includes("stale pid file ignored"), stop.out);
      assert.equal(existsSync(box.pidFile), false, "stop removes the stale pid file");
      assert.ok(isAlive(victim.pid), "an unrelated live process must not be signaled");
    } finally {
      victim.dispose();
    }
  });

  it("a genuine sidecar: start, status, idempotent start, stop", () => {
    const box = makeSandbox();
    try {
      const start = runSidecar(box, "start");
      assert.equal(start.code, 0, `${start.out} ${start.err}`);
      assert.ok(start.out.includes("sidecar started (pid "), start.out);

      const text = (readPidFile(box) ?? "").trim();
      const parts = text.split(" ");
      const pid = Number(parts[0]);
      assert.ok(Number.isInteger(pid) && pid > 1, `pid file: ${text}`);
      assert.ok(
        parts.length === 2 && /^\d+$/.test(parts[1]),
        `pid file must record '<pid> <starttime>': ${text}`,
      );
      assert.ok(isAlive(pid), "the pid the pid file names must be alive");

      const status = runSidecar(box, "status");
      assert.equal(status.code, 0, `${status.out} ${status.err}`);
      assert.ok(status.out.includes(`sidecar running (pid ${pid})`), status.out);
      assert.ok(status.out.includes("socket present"), status.out);
      assert.ok(isSocket(box.socketPath), "the sidecar socket must exist");

      // start is idempotent: same pid file, no second process
      const again = runSidecar(box, "start");
      assert.equal(again.code, 0, `${again.out} ${again.err}`);
      assert.ok(again.out.includes("sidecar already running"), again.out);
      assert.equal((readPidFile(box) ?? "").trim(), text, "the pid file must be untouched");
      assert.equal(countSidecarProcesses(box.configPath), 1, "exactly one sidecar process");

      const stop = runSidecar(box, "stop");
      assert.equal(stop.code, 0, `${stop.out} ${stop.err}`);
      assert.ok(waitForDeath(pid, 8000), "the sidecar must be gone after stop");
      assert.equal(existsSync(box.pidFile), false, "stop removes the pid file");
      assert.equal(isSocket(box.socketPath), false, "stop removes the socket");
    } finally {
      killLeftover(box);
    }
  });

  it("an old single-field pid file naming the real sidecar is honored", () => {
    const box = makeSandbox();
    try {
      const start = runSidecar(box, "start");
      assert.equal(start.code, 0, `${start.out} ${start.err}`);
      const pid = requirePidFile(box);
      assert.ok(isAlive(pid), `pid file: ${readPidFile(box)}`);

      // Downgrade the pid file to the legacy single-field format.
      writeFileSync(box.pidFile, `${pid}\n`);

      const status = runSidecar(box, "status");
      assert.equal(status.code, 0, `${status.out} ${status.err}`);
      assert.ok(status.out.includes(`sidecar running (pid ${pid})`), status.out);

      const stop = runSidecar(box, "stop");
      assert.equal(stop.code, 0, `${stop.out} ${stop.err}`);
      assert.ok(stop.out.includes("sidecar stopped"), stop.out);
      assert.ok(waitForDeath(pid, 8000), "the sidecar must be gone");
      assert.equal(existsSync(box.pidFile), false, "stop removes the pid file");
    } finally {
      killLeftover(box);
    }
  });

  it("start over a stale pid file clears it and really starts", () => {
    const box = makeSandbox();
    const sleeper = spawnSleeper();
    const bin = join(box.root, "bin");
    mkdirSync(bin, { recursive: true });
    // Fake `python3`: a bash script that stays alive, so its argv carries
    // exactly the tokens sidecar.sh verifies (-m sidecar --config <cfg>).
    // It honors SIGTERM (clean exit) so stop stays fast.
    const fakePython = join(bin, "python3");
    writeFileSync(
      fakePython,
      `#!/usr/bin/env bash
trap 'if [ -n "\${child:-}" ]; then kill "$child" 2>/dev/null; fi; exit 0' TERM INT
sleep 300 &
child=$!
wait "$child" 2>/dev/null
exit 0
`,
      { mode: 0o755 },
    );
    const env = { PATH: `${bin}:${process.env.PATH ?? ""}` };
    try {
      writeFileSync(box.pidFile, `${sleeper.pid} ${wrongStarttime(sleeper.pid)}\n`);

      const start = runSidecar(box, "start", env);
      assert.equal(start.code, 0, `${start.out} ${start.err}`);
      assert.ok(
        start.out.includes(`ignoring stale pid file (pid ${sleeper.pid} is not this sidecar)`),
        start.out,
      );
      assert.ok(start.out.includes("sidecar started (pid "), start.out);
      assert.ok(isAlive(sleeper.pid), "the stale process must not be touched");

      const text = (readPidFile(box) ?? "").trim();
      const parts = text.split(" ");
      const pid = Number(parts[0]);
      assert.ok(
        Number.isInteger(pid) && pid > 1 && pid !== sleeper.pid,
        `pid file must name the new process: ${text}`,
      );
      assert.ok(
        parts.length === 2 && /^\d+$/.test(parts[1]),
        `the new pid file must carry a starttime: ${text}`,
      );
      assert.ok(isAlive(pid), "the new process must be alive");

      const status = runSidecar(box, "status", env);
      assert.equal(status.code, 0, `${status.out} ${status.err}`);
      assert.ok(status.out.includes(`sidecar running (pid ${pid})`), status.out);
    } finally {
      runSidecar(box, "stop", env);
      sleeper.dispose();
    }
  });

  it("restart replaces a running sidecar with a fresh process", () => {
    const box = makeSandbox();
    try {
      const start = runSidecar(box, "start");
      assert.equal(start.code, 0, `${start.out} ${start.err}`);
      const oldPid = requirePidFile(box);
      assert.ok(isAlive(oldPid), "the initial sidecar must be alive");

      const restart = runSidecar(box, "restart");
      assert.equal(restart.code, 0, `${restart.out} ${restart.err}`);

      const newPid = requirePidFile(box);
      assert.notEqual(newPid, oldPid, "restart must produce a fresh process");
      assert.ok(waitForDeath(oldPid, 10000), "the old sidecar must be gone");
      assert.ok(isAlive(newPid), "the new sidecar must be alive");

      const status = runSidecar(box, "status");
      assert.equal(status.code, 0, `${status.out} ${status.err}`);
      assert.ok(status.out.includes(`sidecar running (pid ${newPid})`), status.out);
    } finally {
      killLeftover(box);
    }
  });
});
