"""Send a one-off instruction / task to the Manager Agent via a DM room.

Used to deliver the team-building prompt (prompts/manager-team-prompt.md) to
the Manager. The Manager receives it like a human admin would send a task.
Transport = Matrix Client-Server API (same channel a real IM gateway uses).

Usage
    python feed_manager.py                       # sends prompts/manager-team-prompt.md
    python feed_manager.py --file path/to.md     # send a custom instruction
    python feed_manager.py --text "do something" # send ad-hoc text
"""

from __future__ import annotations

import argparse
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

from matrix_client import MatrixClient, MatrixError, build_client_from_env, load_env_file  # noqa: E402

DEFAULT_PROMPT = os.path.join(
    os.path.dirname(HERE), "prompts", "manager-team-prompt.md"
)


def find_dm_room(client: MatrixClient, manager_id: str) -> str | None:
    for room_id in client.joined_rooms():
        members = client.room_members(room_id)
        if manager_id in members and len(members) == 2:
            return room_id
    return None


def main() -> int:
    parser = argparse.ArgumentParser(description="Feed an instruction to the Manager Agent via DM")
    parser.add_argument("--file", default=DEFAULT_PROMPT)
    parser.add_argument("--text", default=None)
    parser.add_argument("--env-file", default=None)
    parser.add_argument("--wait", type=float, default=0.0,
                        help="seconds to wait and print manager's reply after sending")
    args = parser.parse_args()

    env = load_env_file(args.env_file)
    if not env:
        print("[feed] ERROR: agentteams-manager.env not found")
        return 2
    client, env = build_client_from_env(env)
    admin_user = env.get("AGENTTEAMS_ADMIN_USER", "admin")
    manager_id = f"@manager:{client.server_name}"

    try:
        client.login(admin_user, env.get("AGENTTEAMS_ADMIN_PASSWORD", ""))
    except MatrixError as exc:
        print(f"[feed] login failed: {exc}")
        return 3
    print(f"[feed] connected as {client.user_id}")

    room_id = find_dm_room(client, manager_id)
    if not room_id:
        print(f"[feed] creating DM room with {manager_id} ...")
        room_id = client.create_room(
            name="Manager: default",
            invite=[manager_id],
            is_direct=True,
            topic="Admin -> Manager control channel",
        )
        print(f"[feed] DM room created: {room_id}")
    else:
        print(f"[feed] reusing DM room: {room_id}")

    if args.text:
        body = args.text
    else:
        with open(args.file, "r", encoding="utf-8") as handle:
            body = handle.read().strip()
    if not body:
        print("[feed] nothing to send")
        return 1

    try:
        ev = client.send_text(room_id, body)
        print(f"[feed] sent instruction (event {ev})")
    except MatrixError as exc:
        print(f"[feed] send failed: {exc}")
        return 4

    if args.wait > 0:
        print(f"[feed] waiting {args.wait}s for manager reply ...")
        seen = {ev}
        deadline = time.time() + args.wait
        while time.time() < deadline:
            time.sleep(6)
            try:
                chunk = client.messages(room_id, limit=20, direction="b")
            except MatrixError:
                continue
            for event in reversed(chunk):
                eid = event.get("event_id")
                if not eid or eid in seen:
                    continue
                seen.add(eid)
                if (event.get("sender") or "").lstrip("@").split(":", 1)[0] == "manager":
                    body2 = (event.get("content") or {}).get("body") or ""
                    ts = time.strftime("%H:%M:%S", time.localtime((event.get("origin_server_ts") or 0) / 1000))
                    print(f"[manager {ts}] {body2.strip()[:1200]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
