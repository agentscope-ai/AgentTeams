"""Matrix client-server REST helpers and the e2e preflight.

Shared by ``scripts/e2e-openclaw.sh`` (the harness imports them so the
preflight is not implemented twice) and runnable on its own:

    python3 scripts/e2e_preflight.py

which validates the environment (both users' tokens, both memberships)
without starting OpenClaw, the sidecar or the Matrix core. Exit codes:
0 = ok, 2 = preflight/usage failure (same convention as the e2e script).

Environment:
    AGENTTEAMS_MATRIX_URL   homeserver base URL
    E2E_MATRIX_USER         agent MXID
    E2E_MATRIX_TOKEN        agent access token
    E2E_SENDER_USER         sender MXID (drives the conversation)
    E2E_SENDER_TOKEN        sender access token
    E2E_ROOM_ID             the room both users are joined to
"""

import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HS = os.environ.get("AGENTTEAMS_MATRIX_URL", "").rstrip("/")
AGENT_MXID = os.environ.get("E2E_MATRIX_USER", "")
AGENT_TOKEN = os.environ.get("E2E_MATRIX_TOKEN", "")
SENDER_MXID = os.environ.get("E2E_SENDER_USER", "")
SENDER_TOKEN = os.environ.get("E2E_SENDER_TOKEN", "")
ROOM_ID = os.environ.get("E2E_ROOM_ID", "")


class PreflightError(RuntimeError):
    """An environment problem the operator must fix (token, membership, room)."""


def log(msg):
    print(f"[preflight] {msg}", flush=True)


def encode_component(value):
    """Percent-encode ONE path segment (room ids start with '!', MXIDs '@')."""
    return urllib.parse.quote(str(value), safe="")


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
    """The membership of ``user`` in ROOM_ID, read from the member state event.

    ``GET /rooms/{roomId}/state/m.room.member/{userId}`` is the documented
    client-server route for a single membership. The older
    ``/rooms/{roomId}/members/{userId}`` shape (what this harness used to
    call) has never been part of the spec: Tuwunel answers it with
    404 M_UNRECOGNIZED, which aborted every e2e run before it started. The
    ``/joined_members`` collection is the documented fallback for servers
    that do not expose member state.
    """
    room = encode_component(ROOM_ID)
    subject = encode_component(user)
    try:
        data = rest(
            "GET",
            f"/_matrix/client/v3/rooms/{room}/state/m.room.member/{subject}",
            token,
        )
        return data.get("membership")
    except RuntimeError as exc:
        message = str(exc)
        # Only a "this route/state is not available" answer may fall back;
        # an auth or network failure must surface unchanged.
        if "HTTP 404" not in message and "HTTP 400" not in message:
            raise
    data = rest("GET", f"/_matrix/client/v3/rooms/{room}/joined_members", token)
    joined = data.get("joined") if isinstance(data.get("joined"), dict) else {}
    return "join" if user in joined else None


def send_message(text):
    txn = f"e2e-{int(time.time() * 1000)}-{os.getpid()}"
    data = rest(
        "PUT",
        f"/_matrix/client/v3/rooms/{encode_component(ROOM_ID)}/send/m.room.message/{encode_component(txn)}",
        SENDER_TOKEN,
        {"msgtype": "m.text", "body": text},
    )
    return data["event_id"]


def list_recent_messages():
    data = rest(
        "GET",
        f"/_matrix/client/v3/rooms/{encode_component(ROOM_ID)}/messages?dir=b&limit=100",
        SENDER_TOKEN,
    )
    return [ev for ev in data.get("chunk", []) if ev.get("type") == "m.room.message"]


def preflight():
    """Validate both identities and both memberships; raise PreflightError."""
    agent = whoami(AGENT_TOKEN)
    if agent.get("user_id") != AGENT_MXID:
        raise PreflightError(
            f"agent token whoami {agent.get('user_id')!r} != E2E_MATRIX_USER {AGENT_MXID!r}"
        )
    sender = whoami(SENDER_TOKEN)
    if sender.get("user_id") != SENDER_MXID:
        raise PreflightError(
            f"sender token whoami {sender.get('user_id')!r} != E2E_SENDER_USER {SENDER_MXID!r}"
        )
    if membership(SENDER_TOKEN, AGENT_MXID) != "join":
        raise PreflightError(f"agent {AGENT_MXID} is not joined to {ROOM_ID}")
    if membership(SENDER_TOKEN, SENDER_MXID) != "join":
        raise PreflightError(f"sender {SENDER_MXID} is not joined to {ROOM_ID}")
    log("preflight ok: both users valid and joined")


REQUIRED_ENV = (
    ("AGENTTEAMS_MATRIX_URL", HS),
    ("E2E_MATRIX_USER", AGENT_MXID),
    ("E2E_MATRIX_TOKEN", AGENT_TOKEN),
    ("E2E_SENDER_USER", SENDER_MXID),
    ("E2E_SENDER_TOKEN", SENDER_TOKEN),
    ("E2E_ROOM_ID", ROOM_ID),
)


def main():
    missing = [name for name, value in REQUIRED_ENV if not value]
    if missing:
        print(
            "e2e: missing required environment variable: " + ", ".join(missing),
            file=sys.stderr,
        )
        return 2
    try:
        preflight()
    except PreflightError as exc:
        print(f"e2e: preflight failed: {exc}", file=sys.stderr)
        return 2
    except Exception as exc:  # network, TLS, malformed JSON, ...
        print(f"e2e: preflight error: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
