#!/usr/bin/env bash
# End-to-end gate for the agentteams-matrix OpenClaw plugin (task book G5).
#
# Brings up the full real stack against a live homeserver:
#   1. the AgentTeams Python core (from $MATRIX_CORE_DIR — branch
#      feat/p9-matrix-core) running as the agent's Matrix identity:
#      NioTransport (matrix-nio sync/send, encryption OFF) + MatrixCore +
#      the real SidecarServer on a temporary 0600 Unix socket;
#   2. the OpenClaw gateway in an ISOLATED home (OPENCLAW_CONFIG_PATH and
#      OPENCLAW_STATE_DIR under a mktemp dir), with this plugin installed
#      from the local source directory via the documented local-install
#      path (`plugins install --force --accept-capabilities <dir>`);
#   3. a model endpoint for the agent's turn:
#        - if E2E_LLM_BASE_URL + E2E_LLM_API_KEY + E2E_LLM_MODEL are all
#          set: that OpenAI-compatible endpoint (e.g. a Higress proxy);
#        - otherwise: a built-in deterministic echo server (127.0.0.1,
#          Python stdlib only) that answers every chat completion with a
#          fixed marker text.
#
# Then, as a second Matrix user, it sends one room message via the
# client-server REST API and asserts:
#   - the agent replies in the same room as m.text, and NOT in a thread;
#   - the reply's m.relates_to.m.in_reply_to.event_id == the sent event;
#   - (echo mode only) the reply text carries the echo marker, proving the
#     model output actually travelled through the whole chain;
#   - a duplicate delivery of the same event at the adapter boundary
#     produces NO second reply. Note: the core has no event-id dedupe of
#     its own (verified in core/transport.py, core/sync.py, core/core.py);
#     the dedupe boundary the plugin's no-requeue design relies on is the
#     adapter side, so the harness enforces it in its bridge and the
#     duplicate check validates that boundary end to end.
#
# Required environment:
#   AGENTTEAMS_MATRIX_URL   homeserver base URL (e.g. http://127.0.0.1:8008)
#   E2E_MATRIX_USER         agent MXID (@agent:example.com)
#   E2E_MATRIX_TOKEN        agent access token
#   E2E_SENDER_USER         sender MXID (a second user, already in the room)
#   E2E_SENDER_TOKEN        sender access token
#   E2E_ROOM_ID             room id (!room:example.com) both users joined
#   MATRIX_CORE_DIR         path to plugins/agentteams-matrix (branch
#                           feat/p9-matrix-core); must contain core/
#
# Optional environment:
#   E2E_LLM_BASE_URL / E2E_LLM_API_KEY / E2E_LLM_MODEL
#       all three set -> real OpenAI-compatible endpoint (baseUrl used
#       verbatim); any missing -> built-in echo model.
#   E2E_AGENT_NAME              sidecar agent name (default "e2e-agent")
#   E2E_CONNECT_TIMEOUT_SECONDS gateway->sidecar connect deadline (180)
#   E2E_REPLY_TIMEOUT_SECONDS   reply polling deadline (120)
#   E2E_SETTLE_SECONDS          duplicate/settle window (30)
#   OPENCLAW_BIN                (default "openclaw")
#
# The plugin must be built (dist/src/index.js); when the build is missing
# the script runs `npm install` + `npm run build` in the plugin root first.
#
# Exit codes: 0 = pass, 1 = e2e failure, 2 = usage/preflight failure.
# On failure the harness and gateway log tails are printed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

e2e_die() { echo "e2e: $*" >&2; exit "${2:-1}"; }

# --- required environment ---------------------------------------------------
for v in AGENTTEAMS_MATRIX_URL E2E_MATRIX_USER E2E_MATRIX_TOKEN \
         E2E_SENDER_USER E2E_SENDER_TOKEN E2E_ROOM_ID MATRIX_CORE_DIR; do
  if [ -z "${!v:-}" ]; then
    echo "e2e: missing required environment variable: $v" >&2
    echo "     (see the header of $(basename "$0") for the full list)" >&2
    exit 2
  fi
done

OPENCLAW_BIN="${OPENCLAW_BIN:-openclaw}"
E2E_AGENT_NAME="${E2E_AGENT_NAME:-e2e-agent}"
E2E_CONNECT_TIMEOUT_SECONDS="${E2E_CONNECT_TIMEOUT_SECONDS:-180}"
E2E_REPLY_TIMEOUT_SECONDS="${E2E_REPLY_TIMEOUT_SECONDS:-120}"
E2E_SETTLE_SECONDS="${E2E_SETTLE_SECONDS:-30}"

command -v python3 >/dev/null || e2e_die "python3 is required" 2
command -v "$OPENCLAW_BIN" >/dev/null || e2e_die "openclaw binary not found: $OPENCLAW_BIN" 2
[ -f "${MATRIX_CORE_DIR}/core/__init__.py" ] \
  || e2e_die "MATRIX_CORE_DIR must point at plugins/agentteams-matrix (needs core/__init__.py): ${MATRIX_CORE_DIR}" 2

# --- LLM endpoint selection ---------------------------------------------------
if [ -n "${E2E_LLM_BASE_URL:-}" ] && [ -n "${E2E_LLM_API_KEY:-}" ] && [ -n "${E2E_LLM_MODEL:-}" ]; then
  LLM_MODE="real"
  E2E_PROVIDER_ID="e2e-llm"
  E2E_MODEL_ID="${E2E_LLM_MODEL}"
  E2E_MODEL_REF="e2e-llm/${E2E_LLM_MODEL}"
  E2E_ECHO_CHECK=0
else
  LLM_MODE="echo"
  E2E_PROVIDER_ID="e2e"
  E2E_MODEL_ID="e2e-echo"
  E2E_MODEL_REF="e2e/e2e-echo"
  E2E_ECHO_CHECK=1
fi

# --- work dir, ports, cleanup -------------------------------------------------
WORK="$(mktemp -d "${TMPDIR:-/tmp}/agentteams-e2e.XXXXXX")"
CLEANUP_PIDS=()
GW_PID=""
HARNESS_PID=""
LLM_PID=""

cleanup() {
  for pid in "${CLEANUP_PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" >/dev/null 2>&1 || true
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

GW_PORT="$(free_port)"

if [ "$LLM_MODE" = "echo" ]; then
  LLM_PORT="$(free_port)"
  cat > "$WORK/echo_llm.py" <<'PYEOF'
"""Deterministic OpenAI-compatible echo model for the e2e gate (stdlib only).

Answers every POST */chat/completions with a fixed assistant text (streaming
or not) and a minimal GET */models list. The reply marker is asserted by the
harness, which proves the model output travelled through the whole chain:
model -> OpenClaw agent turn -> plugin delivery -> sidecar -> Matrix core.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
MARKER = "AGENTTEAMS_E2E_ACK"
REPLY = f"Got it: {MARKER}"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # keep the log quiet
        pass

    def _send(self, code, content_type, body):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.split("?")[0].endswith("/models"):
            body = json.dumps(
                {"object": "list", "data": [{"id": "e2e-echo", "object": "model"}]}
            ).encode()
            self._send(200, "application/json", body)
        else:
            self._send(404, "application/json", b"{}")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except ValueError:
            req = {}
        if not self.path.split("?")[0].endswith("/chat/completions"):
            self._send(404, "application/json", b"{}")
            return
        if req.get("stream"):
            c1 = {
                "id": "e2e",
                "object": "chat.completion.chunk",
                "choices": [
                    {"index": 0, "delta": {"role": "assistant", "content": REPLY}, "finish_reason": None}
                ],
            }
            c2 = {
                "id": "e2e",
                "object": "chat.completion.chunk",
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
            }
            body = (
                f"data: {json.dumps(c1)}\n\n"
                f"data: {json.dumps(c2)}\n\n"
                "data: [DONE]\n\n"
            ).encode()
            self._send(200, "text/event-stream", body)
        else:
            body = json.dumps(
                {
                    "id": "e2e",
                    "object": "chat.completion",
                    "created": 1,
                    "model": "e2e-echo",
                    "choices": [
                        {
                            "index": 0,
                            "message": {"role": "assistant", "content": REPLY},
                            "finish_reason": "stop",
                        }
                    ],
                    "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
                }
            ).encode()
            self._send(200, "application/json", body)


print(f"echo llm listening on 127.0.0.1:{PORT}", flush=True)
HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PYEOF
  python3 "$WORK/echo_llm.py" "$LLM_PORT" > "$WORK/echo_llm.log" 2>&1 &
  LLM_PID=$!
  CLEANUP_PIDS+=("$LLM_PID")
  E2E_LLM_BASE_URL="http://127.0.0.1:${LLM_PORT}/v1"
  E2E_LLM_API_KEY="e2e"
  # Wait for the echo server to accept connections.
  python3 - "$LLM_PORT" <<'PYEOF' || e2e_die "echo model server did not come up"
import socket
import sys
import time

port = int(sys.argv[1])
deadline = time.monotonic() + 30
while time.monotonic() < deadline:
    try:
        socket.create_connection(("127.0.0.1", port), timeout=1).close()
        sys.exit(0)
    except OSError:
        time.sleep(0.2)
sys.exit(1)
PYEOF
fi
E2E_LLM_BASE_URL="$E2E_LLM_BASE_URL" E2E_LLM_API_KEY="$E2E_LLM_API_KEY" \
E2E_MODEL_ID="$E2E_MODEL_ID" E2E_PROVIDER_ID="$E2E_PROVIDER_ID" E2E_MODEL_REF="$E2E_MODEL_REF" \
E2E_WORK="$WORK" E2E_AGENT_NAME="$E2E_AGENT_NAME" python3 - <<'PYEOF'
"""Write the isolated OpenClaw config for the e2e home (env-driven)."""
import json
import os

work = os.environ["E2E_WORK"]
cfg = {
    "gateway": {"mode": "local", "auth": {"mode": "none"}},
    "agents": {"defaults": {"model": {"primary": os.environ["E2E_MODEL_REF"]}}},
    "models": {
        "providers": {
            os.environ["E2E_PROVIDER_ID"]: {
                "baseUrl": os.environ["E2E_LLM_BASE_URL"],
                "apiKey": os.environ["E2E_LLM_API_KEY"],
                "api": "openai-completions",
                "models": [{"id": os.environ["E2E_MODEL_ID"], "name": "E2E"}],
            }
        }
    },
    "channels": {
        "agentteams-matrix": {
            "enabled": True,
            "socketPath": os.path.join(work, "sidecar.sock"),
            "agent": os.environ["E2E_AGENT_NAME"],
        }
    },
}
with open(os.path.join(work, "openclaw.json"), "w", encoding="utf-8") as fh:
    json.dump(cfg, fh, indent=2)
    fh.write("\n")
PYEOF

export OPENCLAW_CONFIG_PATH="$WORK/openclaw.json"
export OPENCLAW_STATE_DIR="$WORK/openclaw-state"

# --- build the plugin when needed --------------------------------------------
if [ ! -f "$PLUGIN_ROOT/dist/src/index.js" ]; then
  echo "e2e: dist/src/index.js missing — building the plugin (npm install + tsc)"
  (cd "$PLUGIN_ROOT" && npm install --no-audit --no-fund && npm run build) \
    || e2e_die "plugin build failed"
fi

# --- install the plugin into the isolated home --------------------------------
echo "e2e: installing the plugin into the isolated OpenClaw home"
"$OPENCLAW_BIN" plugins install --force --accept-capabilities "$PLUGIN_ROOT" \
  || e2e_die "openclaw plugins install failed"
"$OPENCLAW_BIN" plugins enable agentteams-matrix || e2e_die "openclaw plugins enable failed"

# --- the harness (core + sidecar + REST assertions) ----------------------------
cat > "$WORK/harness.py" <<'PYEOF'
"""E2E harness: real sidecar + real Matrix core against a live homeserver.

Owns the agent side of the room:
  - the core SidecarServer on $E2E_WORK/sidecar.sock (0600),
  - MatrixCore + NioTransport (matrix-nio, encryption OFF) as the agent
    Matrix user,
  - the RuntimeBridge: filters inbound events for the e2e room, suppresses
    the agent's own echoes, dedupes by event id (the adapter-boundary
    dedupe the plugin's no-requeue design relies on), and forwards to the
    sidecar,
  - the sidecar worker thread: for each `outbound` frame, submits the
    message to Matrix through the core and acks with the resulting event
    id (the TS client's sendOutbound awaits this ack).

Then it drives the Matrix client-server REST API as the sender user:
  - preflight (whoami for both users, room membership),
  - send one m.text message,
  - poll for the agent reply (m.text, in the room, NOT a thread, with
    m.relates_to.m.in_reply_to.event_id == the sent event id),
  - (echo mode) assert the echo marker is in the reply body,
  - inject a duplicate delivery of the same event at the adapter boundary
    and assert that no second reply appears after the settle window.

Prints "E2E_RESULT pass" and exits 0 on success; prints
"E2E_RESULT fail: <reason>" and exits 1 on any assertion failure.
"""

import asyncio
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.environ["MATRIX_CORE_DIR"])

try:
    from core import (
        CoreConfig,
        FileSessionStore,
        MatrixCore,
        SidecarServer,
        frame_to_outbound,
        make_ack,
        make_inbound,
    )
    from core.transport import NioTransport, TransportConfig
except ImportError as exc:
    print(
        f"E2E_RESULT fail: cannot import the matrix core "
        f"(MATRIX_CORE_DIR={os.environ['MATRIX_CORE_DIR']!r}): {exc!r} "
        f"— the core needs its own dependencies (matrix-nio)",
        flush=True,
    )
    sys.exit(1)

WORK = os.environ["E2E_WORK"]
HS = os.environ["AGENTTEAMS_MATRIX_URL"].rstrip("/")
AGENT_MXID = os.environ["E2E_MATRIX_USER"]
AGENT_TOKEN = os.environ["E2E_MATRIX_TOKEN"]
SENDER_MXID = os.environ["E2E_SENDER_USER"]
SENDER_TOKEN = os.environ["E2E_SENDER_TOKEN"]
ROOM_ID = os.environ["E2E_ROOM_ID"]
AGENT_NAME = os.environ.get("E2E_AGENT_NAME", "e2e-agent")
CONNECT_TIMEOUT = float(os.environ.get("E2E_CONNECT_TIMEOUT_SECONDS", "180"))
REPLY_TIMEOUT = float(os.environ.get("E2E_REPLY_TIMEOUT_SECONDS", "120"))
SETTLE = float(os.environ.get("E2E_SETTLE_SECONDS", "30"))
ECHO_CHECK = os.environ.get("E2E_ECHO_CHECK", "0") == "1"
ECHO_MARKER = "AGENTTEAMS_E2E_ACK"

SOCKET = os.path.join(WORK, "sidecar.sock")
STATE_DIR = os.path.join(WORK, "agent-state")


def log(msg):
    print(f"[harness] {msg}", flush=True)


def fail(msg):
    print(f"E2E_RESULT fail: {msg}", flush=True)
    sys.exit(1)


# --- Matrix client-server REST (synchronous; called via asyncio.to_thread) ----

def rest(method, path, token, body=None):
    url = HS + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, method=method, data=data)
    req.add_header("Authorization", f"Bearer {token}")
    if body is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
            return json.loads(payload) if payload else {}
    except urllib.error.HTTPError as exc:
        detail = exc.read()[:500]
        raise RuntimeError(f"{method} {path} -> HTTP {exc.code}: {detail!r}") from None


def whoami(token):
    return rest("GET", "/_matrix/client/v3/account/whoami", token)


def membership(token, user):
    data = rest(
        "GET",
        f"/_matrix/client/v3/rooms/{urllib.parse.quote(ROOM_ID)}/members/{urllib.parse.quote(user)}",
        token,
    )
    return data.get("membership")


def send_message(text):
    txn = f"e2e-{int(time.time() * 1000)}-{os.getpid()}"
    data = rest(
        "PUT",
        f"/_matrix/client/v3/rooms/{urllib.parse.quote(ROOM_ID)}/send/m.room.message/{txn}",
        SENDER_TOKEN,
        {"msgtype": "m.text", "body": text},
    )
    return data["event_id"]


def list_recent_messages():
    data = rest(
        "GET",
        f"/_matrix/client/v3/rooms/{urllib.parse.quote(ROOM_ID)}/messages?dir=b&limit=100",
        SENDER_TOKEN,
    )
    return [ev for ev in data.get("chunk", []) if ev.get("type") == "m.room.message"]


def replies_to(sent_event_id):
    """Agent replies (m.text, in the room, not a thread) to the sent event."""
    out = []
    for ev in list_recent_messages():
        if ev.get("sender") != AGENT_MXID:
            continue
        content = ev.get("content", {})
        if content.get("msgtype") != "m.text":
            continue
        relates = content.get("m.relates_to") or {}
        if relates.get("rel_type") == "m.thread":
            continue
        if (relates.get("m.in_reply_to") or {}).get("event_id") == sent_event_id:
            out.append(ev)
    return out


# --- the runtime bridge (adapter-boundary filter + dedupe) ---------------------

class E2EBridge:
    """RuntimeBridge for the e2e: forwards e2e-room text events to the
    sidecar, once per event id."""

    def __init__(self, server):
        self.server = server
        self.seen = set()
        self.last_event = None
        self.forwarded = 0
        self.dropped = 0

    async def on_inbound(self, event):
        if event.room_id != ROOM_ID:
            return
        if event.kind != "text":
            return
        if event.sender == AGENT_MXID:
            return  # our own outbound echoing back through sync
        if event.event_id in self.seen:
            self.dropped += 1
            log(f"dedupe: dropping duplicate delivery of {event.event_id}")
            return
        self.seen.add(event.event_id)
        self.last_event = event
        self.forwarded += 1
        self.server.send(make_inbound(event))
        log(f"forwarded inbound {event.event_id} ({len(event.text)} chars) to sidecar")

    async def on_runtime_output(self, msg):
        log(f"runtime output delivered to {msg.room_id} (kind={msg.kind})")


def sidecar_worker(server, core, loop):
    """Read sidecar frames; answer every outbound with a real Matrix send."""
    while True:
        try:
            frame = server.recv()
        except Exception as exc:
            log(f"sidecar worker: connection ended ({exc})")
            return
        ftype = frame.get("type")
        if ftype == "outbound":
            msg = frame_to_outbound(frame)
            log(f"outbound to {msg.room_id}: {msg.text[:120]!r} (reply_to={msg.reply_to})")
            try:
                event_id = asyncio.run_coroutine_threadsafe(
                    core.submit(msg), loop
                ).result(timeout=120)
            except Exception as exc:
                # No ack: a wrong ack would desync the client's FIFO
                # correlation. The connection will surface the failure.
                log(f"sidecar worker: submit failed: {exc!r}")
                return
            server.send(make_ack(event_id))
            log(f"acked outbound with {event_id}")
        elif ftype == "typing":
            log(f"typing frame (ignored): state={frame.get('state')}")


# --- orchestration -------------------------------------------------------------

def preflight():
    agent = whoami(AGENT_TOKEN)
    if agent.get("user_id") != AGENT_MXID:
        fail(f"agent token whoami {agent.get('user_id')!r} != E2E_MATRIX_USER {AGENT_MXID!r}")
    sender = whoami(SENDER_TOKEN)
    if sender.get("user_id") != SENDER_MXID:
        fail(f"sender token whoami {sender.get('user_id')!r} != E2E_SENDER_USER {SENDER_MXID!r}")
    if membership(SENDER_TOKEN, AGENT_MXID) != "join":
        fail(f"agent {AGENT_MXID} is not joined to {ROOM_ID}")
    if membership(SENDER_TOKEN, SENDER_MXID) != "join":
        fail(f"sender {SENDER_MXID} is not joined to {ROOM_ID}")
    log("preflight ok: both users valid and joined")


async def orchestrate(server, bridge):
    log("waiting for the OpenClaw plugin to connect to the sidecar")
    deadline = time.monotonic() + CONNECT_TIMEOUT
    while not server.connected:
        if time.monotonic() > deadline:
            fail(
                f"timed out after {CONNECT_TIMEOUT:.0f}s waiting for the "
                "OpenClaw plugin to connect to the sidecar"
            )
        await asyncio.sleep(0.5)
    log(f"sidecar connected: agent={server.agent!r} capabilities={server.capabilities!r}")
    if server.agent != AGENT_NAME:
        log(f"note: plugin advertised agent={server.agent!r}, config expects {AGENT_NAME!r}")

    sent_text = f"e2e probe {int(time.time())}"
    sent_event_id = await asyncio.to_thread(send_message, sent_text)
    log(f"sent {sent_event_id} as {SENDER_MXID}: {sent_text!r}")

    deadline = time.monotonic() + REPLY_TIMEOUT
    reply = None
    while True:
        replies = await asyncio.to_thread(replies_to, sent_event_id)
        if replies:
            reply = replies[0]
            break
        if time.monotonic() > deadline:
            fail(f"timed out after {REPLY_TIMEOUT:.0f}s waiting for the agent reply")
        await asyncio.sleep(2)
    log(f"reply found: {reply['event_id']} (sender {reply.get('sender')})")

    if ECHO_CHECK:
        body = reply.get("content", {}).get("body", "")
        if ECHO_MARKER not in body:
            fail(f"echo marker {ECHO_MARKER!r} not in reply body {body!r}")
        log("echo marker present in the reply body (model output travelled the full chain)")

    # Settle, then a single send must have produced exactly one reply.
    await asyncio.sleep(SETTLE)
    count = len(await asyncio.to_thread(replies_to, sent_event_id))
    if count != 1:
        fail(f"expected exactly 1 reply after settle, got {count}")
    log(f"pass: exactly 1 reply after the settle window")

    # Duplicate delivery at the adapter boundary: the same event, once more.
    dup = bridge.last_event
    if dup is None or dup.event_id != sent_event_id:
        fail("could not identify the inbound event the reply was produced from")
    log("injecting duplicate delivery of the same event (adapter-boundary dedupe)")
    await bridge.on_inbound(dup)
    await asyncio.sleep(SETTLE)
    count = len(await asyncio.to_thread(replies_to, sent_event_id))
    if count != 1:
        fail(f"duplicate delivery produced {count} replies (expected exactly 1)")
    log(f"pass: duplicate delivery of {sent_event_id} produced no additional reply")
    print("E2E_RESULT pass", flush=True)


async def main():
    await asyncio.to_thread(preflight)

    server = SidecarServer(SOCKET)
    server.start()
    log(f"sidecar listening on {SOCKET}")

    store = FileSessionStore(STATE_DIR)
    transport = NioTransport(
        TransportConfig(
            homeserver=HS,
            user_id=AGENT_MXID,
            access_token=AGENT_TOKEN,
            encryption=False,
        ),
        store=store,
        state_dir=STATE_DIR,
        encryption=False,
    )
    bridge = E2EBridge(server)
    core = MatrixCore(
        transport,
        store,
        bridge,
        CoreConfig(state_dir=STATE_DIR, agent_id=AGENT_NAME, socket_path=SOCKET),
    )

    loop = asyncio.get_running_loop()
    worker = threading.Thread(
        target=sidecar_worker, args=(server, core, loop), daemon=True, name="sidecar-worker"
    )
    worker.start()

    core_task = asyncio.create_task(core.run())
    try:
        await orchestrate(server, bridge)
    finally:
        await core.stop()
        server.stop()
        worker.join(timeout=5)
        try:
            await asyncio.wait_for(core_task, timeout=10)
        except Exception as exc:
            log(f"note: core shutdown: {exc!r}")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except SystemExit:
        raise
    except Exception as exc:
        fail(f"harness crashed: {exc!r}")
PYEOF

# --- run: harness first (binds the socket), then the gateway --------------------
echo "e2e: starting the core/sidecar harness"
E2E_WORK="$WORK" \
E2E_ECHO_CHECK="$E2E_ECHO_CHECK" \
E2E_AGENT_NAME="$E2E_AGENT_NAME" \
E2E_CONNECT_TIMEOUT_SECONDS="$E2E_CONNECT_TIMEOUT_SECONDS" \
E2E_REPLY_TIMEOUT_SECONDS="$E2E_REPLY_TIMEOUT_SECONDS" \
E2E_SETTLE_SECONDS="$E2E_SETTLE_SECONDS" \
  python3 "$WORK/harness.py" > "$WORK/harness.log" 2>&1 &
HARNESS_PID=$!
CLEANUP_PIDS+=("$HARNESS_PID")

# The harness binds the socket almost immediately; wait for it before the
# gateway starts so the plugin's first connect cannot race an ENOENT.
for _ in $(seq 1 60); do
  [ -S "$WORK/sidecar.sock" ] && break
  if ! kill -0 "$HARNESS_PID" 2>/dev/null; then
    echo "e2e: harness died before the sidecar socket appeared:" >&2
    cat "$WORK/harness.log" >&2
    exit 1
  fi
  sleep 0.5
done
[ -S "$WORK/sidecar.sock" ] || e2e_die "sidecar socket did not appear"

echo "e2e: starting the OpenClaw gateway (isolated home, port ${GW_PORT})"
OPENCLAW_NO_RESPAWN=1 "$OPENCLAW_BIN" gateway run --verbose --force --port "$GW_PORT" \
  > "$WORK/gateway.log" 2>&1 &
GW_PID=$!
CLEANUP_PIDS+=("$GW_PID")

# --- monitor: fail fast when the gateway dies, otherwise wait for the harness ---
while :; do
  if ! kill -0 "$HARNESS_PID" 2>/dev/null; then
    break
  fi
  if ! kill -0 "$GW_PID" 2>/dev/null; then
    echo "e2e: gateway process died early; aborting" >&2
    kill "$HARNESS_PID" >/dev/null 2>&1 || true
    break
  fi
  sleep 1
done

set +e
wait "$HARNESS_PID"
RC=$?
set -e

if [ "$RC" -eq 0 ]; then
  echo "e2e: PASS — real Matrix inbound -> OpenClaw agent -> Matrix reply"
  echo "e2e:       (room correct, reply_to correct, not a thread, duplicate delivery -> no duplicate reply)"
else
  echo "e2e: FAIL (harness exit ${RC})" >&2
  echo "--- harness.log (tail) ---" >&2
  tail -n 60 "$WORK/harness.log" >&2 || true
  echo "--- gateway.log (tail) ---" >&2
  tail -n 60 "$WORK/gateway.log" >&2 || true
  exit 1
fi
