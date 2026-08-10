"""Host-side WeChat group message simulator.

Pushes simulated WeChat group messages into the Agent Team running inside
Docker, at a configurable interval, then collects whatever the team sends
back. Transport is the Matrix Client-Server API exposed by the Higress
gateway, i.e. the exact same channel a real IM gateway would use.

Usage
    python wechat_sim.py                          # full scenario, 90s interval
    python wechat_sim.py --interval 45            # custom interval
    python wechat_sim.py --count 2                # only first 2 messages
    python wechat_sim.py --text "打印机坏了" --sender "陈杰(市场部)"
    python wechat_sim.py --watch-only             # just tail the replies
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

BRIDGE_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "bridge"
)
sys.path.insert(0, BRIDGE_DIR)

from matrix_client import MatrixClient, MatrixError, build_client_from_env, load_env_file  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_SCENARIO = os.path.join(HERE, "messages.json")

C_DIM = "\033[90m"
C_IN = "\033[36m"
C_OUT = "\033[32m"
C_WARN = "\033[33m"
C_ERR = "\033[31m"
C_END = "\033[0m"


def envelope(group: str, sender: str, text: str, message_id: str) -> str:
    """Wire format understood by the Manager and parsed back by the viewer."""
    return (
        f"[微信群消息] 群: {group} | 成员: {sender} | 消息ID: {message_id} | "
        f"时间: {time.strftime('%Y-%m-%d %H:%M:%S')}\n内容: {text}"
    )


def find_gateway_room(client: MatrixClient, room_name: str, create: bool = True) -> str:
    manager_id = f"@manager:{client.server_name}"
    for room_id in client.joined_rooms():
        if client.room_name(room_id) == room_name:
            return room_id
    if not create:
        raise MatrixError(f"gateway room '{room_name}' not found")
    return client.create_room(
        name=room_name,
        invite=[manager_id],
        topic="WeChat group gateway - inbound IT service desk requests",
    )


def tail_replies(client: MatrixClient, room_id: str, seen: set[str], admin_local: str) -> None:
    try:
        chunk = client.messages(room_id, limit=30, direction="b")
    except MatrixError as exc:
        print(f"{C_ERR}[sim] read failed: {exc}{C_END}")
        return
    for event in reversed(chunk):
        eid = event.get("event_id")
        if not eid or eid in seen:
            continue
        seen.add(eid)
        if event.get("type") != "m.room.message":
            continue
        sender = (event.get("sender") or "").lstrip("@").split(":", 1)[0]
        if sender == admin_local:
            continue
        body = (event.get("content") or {}).get("body") or ""
        stamp = time.strftime("%H:%M:%S", time.localtime((event.get("origin_server_ts") or 0) / 1000))
        print(f"{C_OUT}[{stamp}] <- {sender}:{C_END} {body.strip()[:900]}")


def main() -> int:
    parser = argparse.ArgumentParser(description="Simulate WeChat group traffic into AgentTeams")
    parser.add_argument("--interval", type=float, default=90.0,
                        help="seconds between simulated group messages (default 90)")
    parser.add_argument("--scenario", default=DEFAULT_SCENARIO)
    parser.add_argument("--group-room", default="微信群-IT服务台支持群")
    parser.add_argument("--count", type=int, default=0, help="limit number of messages (0 = all)")
    parser.add_argument("--text", default=None, help="send one ad-hoc message and exit")
    parser.add_argument("--sender", default="访客", help="sender nickname for --text")
    parser.add_argument("--watch-only", action="store_true", help="do not send, just tail replies")
    parser.add_argument("--watch-after", type=float, default=120.0,
                        help="seconds to keep tailing replies after the last send")
    parser.add_argument("--env-file", default=None)
    args = parser.parse_args()

    env = load_env_file(args.env_file)
    if not env:
        print(f"{C_ERR}[sim] agentteams-manager.env not found — is AgentTeams installed?{C_END}")
        return 2
    client, env = build_client_from_env(env)
    admin_user = env.get("AGENTTEAMS_ADMIN_USER", "admin")

    try:
        client.login(admin_user, env.get("AGENTTEAMS_ADMIN_PASSWORD", ""))
    except MatrixError as exc:
        print(f"{C_ERR}[sim] login failed: {exc}{C_END}")
        return 3
    print(f"{C_DIM}[sim] connected {client.connect_url} as {client.user_id}{C_END}")

    room_id = find_gateway_room(client, args.group_room)
    print(f"{C_DIM}[sim] gateway room: {args.group_room} -> {room_id}{C_END}")

    seen: set[str] = set()
    try:
        for event in client.messages(room_id, limit=50, direction="b"):
            if event.get("event_id"):
                seen.add(event["event_id"])
    except MatrixError:
        pass

    if args.watch_only:
        print(f"{C_DIM}[sim] watch-only mode, Ctrl+C to stop{C_END}")
        try:
            while True:
                tail_replies(client, room_id, seen, admin_user)
                time.sleep(5)
        except KeyboardInterrupt:
            return 0

    if args.text:
        queue = [{"sender": args.sender, "text": args.text}]
        group_name = args.group_room
    else:
        with open(args.scenario, "r", encoding="utf-8") as handle:
            scenario = json.load(handle)
        group_name = scenario.get("group_name", args.group_room)
        queue = scenario.get("messages", [])
        if args.count > 0:
            queue = queue[: args.count]

    print(f"{C_DIM}[sim] {len(queue)} message(s), interval={args.interval}s{C_END}\n")

    try:
        for idx, item in enumerate(queue, start=1):
            mid = f"wx-{int(time.time() * 1000)}-{idx}"
            body = envelope(group_name, item["sender"], item["text"], mid)
            try:
                client.send_text(room_id, body)
            except MatrixError as exc:
                print(f"{C_ERR}[sim] send failed: {exc}{C_END}")
                continue
            stamp = time.strftime("%H:%M:%S")
            print(f"{C_IN}[{stamp}] -> {item['sender']}:{C_END} {item['text']}")

            if idx < len(queue):
                deadline = time.time() + args.interval
                while time.time() < deadline:
                    tail_replies(client, room_id, seen, admin_user)
                    time.sleep(5)

        print(f"\n{C_DIM}[sim] all messages sent, collecting replies for {args.watch_after}s{C_END}")
        deadline = time.time() + args.watch_after
        while time.time() < deadline:
            tail_replies(client, room_id, seen, admin_user)
            time.sleep(5)
    except KeyboardInterrupt:
        print(f"\n{C_WARN}[sim] interrupted{C_END}")

    print(f"{C_DIM}[sim] done{C_END}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
