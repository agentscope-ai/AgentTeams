/**
 * In-process mock of the AgentTeams Matrix sidecar for unit tests.
 *
 * Speaks the same JSON-lines protocol as the real sidecar
 * (`plugins/agentteams-matrix/core/sidecar.py`, protocol version 1) over a
 * Unix socket in a fresh temp directory. Behavior the tests rely on:
 *   - handshake: `hello` -> `ack{event_id:""}` (or silence, for timeout tests);
 *   - version mismatch: `hello` with a different `protocol` ->
 *     `error{code:"protocol_mismatch"}` + close;
 *   - single connection: a second concurrent connection ->
 *     `error{code:"connection_limit"}` + close (a new connection is accepted
 *     again once the current one is closed);
 *   - `outbound` -> `ack{event_id:<generated>}`, recorded in `lastOutbound`.
 */

import * as fs from "node:fs";
import * as os from "node:os";
import * as net from "node:net";
import * as path from "node:path";

export type MockSidecarOptions = {
  /** Protocol version this mock accepts in `hello` (default 1). */
  protocol?: number;
  /** Never respond to `hello` (for handshake-timeout tests). */
  neverAck?: boolean;
};

export type MockSidecar = {
  /** Path of the Unix socket the mock listens on. */
  readonly socketPath: string;
  /** Last `outbound` frame received, or null. */
  readonly lastOutbound: Record<string, unknown> | null;
  /** Total connections accepted, including rejected duplicates. */
  readonly connectionCount: number;
  /** Push an `inbound` frame to the current connection. */
  pushInbound(frame: Record<string, unknown>): void;
  /** Write raw bytes without a trailing newline (partial-line tests). */
  writeRaw(chunk: string): void;
  /** Close the server and any open connection. */
  close(): Promise<void>;
};

let outboundSeq = 0;

export async function startMockSidecar(options: MockSidecarOptions = {}): Promise<MockSidecar> {
  const protocol = options.protocol ?? 1;
  const neverAck = options.neverAck ?? false;

  const dir = await fs.promises.mkdtemp(path.join(os.tmpdir(), "mock-sidecar-"));
  const socketPath = path.join(dir, "sidecar.sock");

  let currentSocket: net.Socket | null = null;
  let lastOutbound: Record<string, unknown> | null = null;
  let connectionCount = 0;

  const server = net.createServer((socket) => {
    connectionCount += 1;

    if (currentSocket !== null && !currentSocket.destroyed) {
      // `resume()` drains the incoming `hello` bytes: destroying a socket
      // with unread data in its receive buffer aborts the connection and
      // the kernel drops our error frame before the client reads it.
      // Flush the frame first, THEN destroy the handle (the write callback
      // guarantees the frame left our side; `destroy()` — not `end()` —
      // is required because a paused stream swallows the peer's EOF and
      // the handle would stay alive, hanging `server.close()`). The error
      // handler is the fallback for a failed write.
      socket.resume();
      socket.once("error", () => socket.destroy());
      socket.write(
        `${JSON.stringify({
          type: "error",
          code: "connection_limit",
          message: "at most one connection per sidecar",
        })}\n`,
        () => {
          socket.destroy();
        },
      );
      return;
    }

    currentSocket = socket;
    socket.setEncoding("utf8");
    let lineBuffer = "";
    let handshakeDone = false;

    const writeFrame = (frame: Record<string, unknown>): void => {
      if (!socket.destroyed) socket.write(`${JSON.stringify(frame)}\n`);
    };

    socket.on("data", (chunk: string) => {
      lineBuffer += chunk;
      let newlineIndex: number;
      while ((newlineIndex = lineBuffer.indexOf("\n")) !== -1) {
        const line = lineBuffer.slice(0, newlineIndex).trim();
        lineBuffer = lineBuffer.slice(newlineIndex + 1);
        if (line === "") continue;

        let frame: Record<string, unknown>;
        try {
          frame = JSON.parse(line);
        } catch {
          writeFrame({ type: "error", code: "invalid_json", message: "invalid JSON line" });
          socket.destroy();
          return;
        }

        if (!handshakeDone) {
          if (frame.type !== "hello") {
            writeFrame({ type: "error", code: "invalid_frame", message: "expected hello first" });
            socket.destroy();
            return;
          }
          if (frame.protocol !== protocol) {
            // Same flush-then-destroy pattern as the connection_limit
            // branch above (this socket reads once for `hello` but never
            // again, so it must not be left half-open).
            socket.once("error", () => socket.destroy());
            socket.write(
              `${JSON.stringify({
                type: "error",
                code: "protocol_mismatch",
                message: `expected protocol ${protocol}`,
              })}\n`,
              () => {
                socket.destroy();
              },
            );
            return;
          }
          handshakeDone = true;
          if (neverAck) continue; // stay silent; the client must time out
          writeFrame({ type: "ack", event_id: "" });
          continue;
        }

        if (frame.type === "outbound") {
          lastOutbound = { ...frame };
          outboundSeq += 1;
          writeFrame({ type: "ack", event_id: `mock-event-${outboundSeq}` });
        }
      }
    });

    socket.on("close", () => {
      if (currentSocket === socket) currentSocket = null;
    });
    socket.on("error", () => {
      // Client-side failures (e.g. destroy mid-test) are exercised by the
      // tests themselves; the mock must not crash on them.
    });
  });

  await new Promise<void>((resolve, reject) => {
    const onError = (err: Error): void => reject(err);
    server.once("error", onError);
    server.listen(socketPath, () => {
      server.removeListener("error", onError);
      resolve();
    });
  });

  // Mirror the real sidecar's 0600 socket mode.
  await fs.promises.chmod(socketPath, 0o600).catch(() => {});

  return {
    get socketPath() {
      return socketPath;
    },
    get lastOutbound() {
      return lastOutbound;
    },
    get connectionCount() {
      return connectionCount;
    },
    pushInbound(frame: Record<string, unknown>) {
      const payload = "type" in frame ? frame : { type: "inbound", ...frame };
      if (currentSocket && !currentSocket.destroyed) {
        currentSocket.write(`${JSON.stringify(payload)}\n`);
      }
    },
    writeRaw(chunk: string) {
      if (currentSocket && !currentSocket.destroyed) {
        currentSocket.write(chunk);
      }
    },
    close() {
      currentSocket?.destroy();
      return new Promise<void>((resolve) => {
        server.close(() => resolve());
      }).then(() => {
        // Best-effort cleanup of the fixture's own socket file.
        return fs.promises.rm(socketPath, { force: true }).catch(() => {});
      });
    },
  };
}
