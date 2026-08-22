#!/usr/bin/env python3
from __future__ import annotations
"""
Export HiClaw debug logs: Matrix messages + agent session logs.

Usage:
    # Export last 1 hour (default)
    python scripts/export-debug-log.py --range 1h

    # Export last 1 day
    python scripts/export-debug-log.py --range 1d

    # Filter by container or room
    python scripts/export-debug-log.py --range 1h --container hiclaw-manager --room Worker

    # Disable PII redaction
    python scripts/export-debug-log.py --range 1h --no-redact

Output structure:
    debug-log/20260319-153000/
    ├── summary.txt
    ├── matrix-messages/
    │   └── RoomName_!roomid.jsonl
    └── agent-sessions/
        ├── hiclaw-manager/
        │   └── {session-id}.jsonl
        └── hiclaw-worker-xxx/
            └── {session-key}.jsonl
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request
import urllib.error
from datetime import datetime, timezone
from pathlib import Path


# ---------------------------------------------------------------------------
# PII redaction
# ---------------------------------------------------------------------------

_PII_PATTERNS: list[tuple[str, re.Pattern]] = [
    ("ID_CARD",       re.compile(r'\b[1-9]\d{5}(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b')),
    ("PHONE",         re.compile(r'(?<!\d)(?:\+?86[-\s]?)?1[3-9]\d{9}(?!\d)')),
    ("EMAIL",         re.compile(r'\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b')),
    ("BANK_CARD",     re.compile(r'\b(?:6[0-9]{15,18}|4[0-9]{15}|5[1-5][0-9]{14}|3[47][0-9]{13}|62[0-9]{14,17})\b')),
    ("IP",            re.compile(r'\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b')),
    ("ALIYUN_AK",     re.compile(r'\bLTAI[A-Za-z0-9]{12,30}\b')),
    ("ALIYUN_SK",     re.compile(r'(?i)(?:access_?key_?secret|secret_?access_?key)\s*[:=]\s*(\S{20,})')),
    ("AWS_AK",        re.compile(r'\b(?:AKIA|ASIA)[A-Z0-9]{16}\b')),
    ("OPENAI_KEY",    re.compile(r'\bsk-[A-Za-z0-9]{20,}\b')),
    ("ANTHROPIC_KEY", re.compile(r'\bsk-ant-[A-Za-z0-9\-]{20,}\b')),
    ("DASHSCOPE_KEY", re.compile(r'\bsk-sp-[A-Za-z0-9]{20,}\b')),
    ("DEEPSEEK_KEY",  re.compile(r'\bsk-[a-f0-9]{32,}\b')),
    ("BEARER",        re.compile(r'(?i)(Bearer\s+)([A-Za-z0-9\-_.]{20,})')),
    ("SECRET_KV",     re.compile(
        r'(?i)((?:password|passwd|pwd|secret|token|api_?key|access_?key|secret_?key'
        r'|private_?key|credential|appkey|app_?secret|auth_?token|signing_?key'
        r'|client_?secret|master_?key)\s*[:=]\s*)'
        r'(\S+)'
    )),
    ("MATRIX_TOKEN",  re.compile(r'\bsyt_[A-Za-z0-9_\-]{10,}\b')),
    ("HEX_SECRET",    re.compile(r'\b[A-Fa-f0-9]{32,}\b')),
    ("PASSPORT",      re.compile(r'\b[EeGg]\d{8}\b')),
    ("SSN",           re.compile(r'\b\d{3}-\d{2}-\d{4}\b')),
]


def redact_pii(text: str) -> str:
    if not text:
        return text
    for name, pattern in _PII_PATTERNS:
        if name in ("SECRET_KV", "ALIYUN_SK", "BEARER"):
            text = pattern.sub(r'\1****', text)
        else:
            text = pattern.sub('****', text)
    return text


def redact_json_strings(obj):
    if isinstance(obj, str):
        return redact_pii(obj)
    if isinstance(obj, list):
        return [redact_json_strings(v) for v in obj]
    if isinstance(obj, dict):
        return {k: redact_json_strings(v) for k, v in obj.items()}
    return obj


# ---------------------------------------------------------------------------
# Common helpers
# ---------------------------------------------------------------------------

def parse_range(range_str: str) -> int:
    m = re.fullmatch(r"(\d+)\s*(m|min|h|hr|hour|d|day)s?", range_str.strip(), re.IGNORECASE)
    if not m:
        raise ValueError(f"Invalid range format: '{range_str}'. Use e.g. 10m, 1h, 1d")
    value, unit = int(m.group(1)), m.group(2)[0].lower()
    return value * {"m": 60, "h": 3600, "d": 86400}[unit]


def parse_ts(ts_str: str) -> float:
    if not ts_str:
        return 0
    ts_str = ts_str.replace("Z", "+00:00")
    try:
        dt = datetime.fromisoformat(ts_str)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.timestamp()
    except Exception:
        return 0


def sanitize_filename(name: str) -> str:
    return re.sub(r'[^\w\-. ]', '_', name).strip()[:80]


def docker_exec(container: str, cmd: str) -> str:
    result = subprocess.run(
        ["docker", "exec", container, "sh", "-c", cmd],
        capture_output=True, text=True, timeout=30,
    )
    return result.stdout


def list_hiclaw_containers() -> list[str]:
    result = subprocess.run(
        ["docker", "ps", "--format", "{{.Names}}", "--filter", "name=hiclaw-"],
        capture_output=True, text=True, timeout=10,
    )
    return [n.strip() for n in result.stdout.splitlines() if n.strip()]


# ---------------------------------------------------------------------------
# Matrix messages export
# ---------------------------------------------------------------------------

def load_env_file(path: str) -> dict[str, str]:
    env = {}
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                if "=" in line:
                    key, _, value = line.partition("=")
                    env[key.strip()] = value.strip()
    except FileNotFoundError:
        pass
    return env


def matrix_login(homeserver: str, user: str, password: str) -> str:
    url = f"{homeserver.rstrip('/')}/_matrix/client/v3/login"
    payload = json.dumps({
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": user},
        "password": password,
    }).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as resp:
        data = json.loads(resp.read())
    return data["access_token"]


def matrix_api(homeserver: str, token: str, endpoint: str, params: dict | None = None) -> dict:
    url = f"{homeserver.rstrip('/')}/_matrix/client/v3/{endpoint}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as e:
        body = e.read().decode(errors="replace")
        print(f"  API error {e.code} on {endpoint}: {body}", file=sys.stderr)
        raise


def fetch_room_messages(homeserver: str, token: str, room_id: str, since_ts: int) -> list[dict]:
    encoded = urllib.parse.quote(room_id)
    messages = []
    from_token = ""
    while True:
        params = {"dir": "b", "limit": "100"}
        if from_token:
            params["from"] = from_token
        data = matrix_api(homeserver, token, f"rooms/{encoded}/messages", params)
        chunk = data.get("chunk", [])
        if not chunk:
            break
        hit_boundary = False
        for event in chunk:
            if event.get("origin_server_ts", 0) < since_ts:
                hit_boundary = True
                break
            messages.append(event)
        if hit_boundary:
            break
        next_token = data.get("end")
        if not next_token or next_token == from_token:
            break
        from_token = next_token
    messages.reverse()
    return messages


def format_event(event: dict, redact: bool) -> dict:
    content = event.get("content", {})
    ts = event.get("origin_server_ts", 0)
    record = {
        "event_id": event.get("event_id"),
        "type": event.get("type"),
        "sender": event.get("sender"),
        "timestamp": ts,
        "time": datetime.fromtimestamp(ts / 1000, tz=timezone.utc).isoformat(),
    }
    if event.get("type") == "m.room.message":
        record["msgtype"] = content.get("msgtype")
        body = content.get("body")
        record["body"] = redact_pii(body) if redact else body
        if content.get("format"):
            record["format"] = content["format"]
        if content.get("url"):
            record["url"] = content["url"]
        if content.get("m.relates_to"):
            record["relates_to"] = content["m.relates_to"]
    else:
        record["content"] = content
    return record


def export_matrix_messages(out_dir: Path, since_epoch: float, redact: bool,
                           room_filter: str | None, env_file: str | None,
                           homeserver: str, token: str,
                           messages_only: bool) -> tuple[int, int]:
    """Export Matrix messages. Returns (rooms_exported, message_count)."""
    env_path = env_file or os.path.expanduser("~/hiclaw-manager.env")
    hiclaw_env = load_env_file(env_path)

    if not homeserver:
        if not hiclaw_env:
            print(f"  [matrix] Cannot find {env_path}, skipping Matrix export")
            return 0, 0
        port = hiclaw_env.get("HICLAW_PORT_GATEWAY", "18080")
        homeserver = f"http://127.0.0.1:{port}"

    if not token:
        # Use Manager token — Manager is in every room (DM, Worker, Project)
        manager_password = hiclaw_env.get("HICLAW_MANAGER_PASSWORD", "")
        if manager_password:
            try:
                token = matrix_login(homeserver, "manager", manager_password)
            except Exception:
                pass

        # Fallback to admin token if Manager login failed
        if not token:
            admin_user = hiclaw_env.get("HICLAW_ADMIN_USER", "admin")
            admin_password = hiclaw_env.get("HICLAW_ADMIN_PASSWORD", "")
            if not admin_password:
                print(f"  [matrix] No usable credentials found, skipping Matrix export")
                return 0, 0
            try:
                token = matrix_login(homeserver, admin_user, admin_password)
            except Exception as e:
                print(f"  [matrix] Login failed: {e}, skipping Matrix export")
                return 0, 0

    since_ts = int(since_epoch * 1000)
    out_dir.mkdir(parents=True, exist_ok=True)

    try:
        rooms = matrix_api(homeserver, token, "joined_rooms").get("joined_rooms", [])
    except Exception as e:
        print(f"  [matrix] Failed to list rooms: {e}")
        return 0, 0

    total_messages = 0
    total_rooms = 0

    for room_id in rooms:
        encoded = urllib.parse.quote(room_id)
        try:
            room_name = matrix_api(homeserver, token, f"rooms/{encoded}/state/m.room.name").get("name", "")
        except Exception:
            room_name = ""

        if room_filter:
            if room_filter not in room_id and room_filter not in (room_name or ""):
                continue

        display = f"{room_name} ({room_id})" if room_name else room_id
        print(f"  {display} ... ", end="", flush=True)

        messages = fetch_room_messages(homeserver, token, room_id, since_ts)
        if messages_only:
            messages = [e for e in messages if e.get("type") == "m.room.message"]

        if not messages:
            print("0 messages, skipped")
            continue

        name_part = sanitize_filename(room_name) if room_name else ""
        id_part = sanitize_filename(room_id)
        filename = f"{name_part}_{id_part}.jsonl" if name_part else f"{id_part}.jsonl"

        with open(out_dir / filename, "w", encoding="utf-8") as f:
            for event in messages:
                record = format_event(event, redact=redact)
                f.write(json.dumps(record, ensure_ascii=False) + "\n")

        print(f"{len(messages)} messages -> {filename}")
        total_messages += len(messages)
        total_rooms += 1

    return total_rooms, total_messages


# ---------------------------------------------------------------------------
# Agent sessions export (OpenClaw + CoPaw)
# ---------------------------------------------------------------------------

def detect_runtime(container: str) -> tuple[str, str]:
    std = ".openclaw/agents/main/sessions"
    if docker_exec(container, f"test -d {std} && echo yes || echo no").strip() == "yes":
        return "openclaw", std

    worker_name = docker_exec(container, "echo $HICLAW_WORKER_NAME").strip()
    if worker_name:
        copaw_dir = f"{worker_name}/.copaw/sessions"
        if docker_exec(container, f"test -d '{copaw_dir}' && echo yes || echo no").strip() == "yes":
            return "copaw", copaw_dir

    found = docker_exec(container, "find . -maxdepth 3 -path '*/.copaw/sessions' -type d 2>/dev/null | head -1").strip()
    if found:
        return "copaw", found.lstrip("./")

    return "", ""


def export_openclaw_sessions(container: str, sessions_dir: str, since_epoch: float,
                             out_dir: Path, redact: bool) -> tuple[int, int]:
    ls_output = docker_exec(container, f"ls '{sessions_dir}'/*.jsonl 2>/dev/null").strip()
    if not ls_output:
        return 0, 0

    total_sessions = 0
    total_events = 0

    for session_path in [f.strip() for f in ls_output.splitlines() if f.strip()]:
        filename = os.path.basename(session_path)

        first_line = docker_exec(container, f"head -1 '{session_path}'").strip()
        if not first_line:
            continue
        try:
            header = json.loads(first_line)
        except json.JSONDecodeError:
            continue

        last_line = docker_exec(container, f"tail -1 '{session_path}'").strip()
        try:
            last_ts = parse_ts(json.loads(last_line).get("timestamp", ""))
        except Exception:
            last_ts = parse_ts(header.get("timestamp", ""))

        if last_ts < since_epoch and last_ts > 0:
            continue

        raw = docker_exec(container, f"cat '{session_path}'")
        if not raw.strip():
            continue

        output_lines = []
        for line in raw.splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            event_ts = parse_ts(event.get("timestamp", ""))
            if event.get("type") != "session" and event_ts < since_epoch and event_ts > 0:
                continue
            if redact:
                event = redact_json_strings(event)
            output_lines.append(json.dumps(event, ensure_ascii=False))

        if len(output_lines) <= 1:
            continue

        with open(out_dir / filename, "w", encoding="utf-8") as f:
            f.write("\n".join(output_lines) + "\n")

        event_count = len(output_lines) - 1
        print(f"  {container}/{filename} (openclaw): {event_count} events")
        total_sessions += 1
        total_events += event_count

    # sessions.json index
    try:
        raw = docker_exec(container, f"cat '{sessions_dir}/sessions.json'")
        if raw.strip():
            index = json.loads(raw)
            if redact:
                index = redact_json_strings(index)
            with open(out_dir / "sessions.json", "w", encoding="utf-8") as f:
                json.dump(index, f, ensure_ascii=False, indent=2)
    except Exception:
        pass

    return total_sessions, total_events


def export_copaw_sessions(container: str, sessions_dir: str, since_epoch: float,
                          out_dir: Path, redact: bool) -> tuple[int, int]:
    ls_output = docker_exec(container, f"find '{sessions_dir}' -name '*.json' -type f 2>/dev/null").strip()
    if not ls_output:
        return 0, 0

    total_sessions = 0
    total_events = 0

    for session_path in [f.strip() for f in ls_output.splitlines() if f.strip()]:
        raw = docker_exec(container, f"cat '{session_path}'")
        if not raw.strip():
            continue
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            continue

        agent = data.get("agent", {})
        memory = agent.get("memory", {})
        content = memory.get("content", [])
        if not content:
            continue

        basename = os.path.basename(session_path).replace(".json", "")
        header = {
            "type": "session",
            "runtime": "copaw",
            "agent_name": agent.get("name", ""),
            "session_key": basename,
            "compressed_summary": memory.get("_compressed_summary", ""),
        }

        messages_in_range = []
        for turn_idx, turn in enumerate(content):
            if not isinstance(turn, list):
                continue
            for msg in turn:
                if not isinstance(msg, dict):
                    continue
                msg_ts = parse_ts(msg.get("timestamp", ""))
                if msg_ts >= since_epoch or msg_ts == 0:
                    event = {
                        "type": "message",
                        "turn": turn_idx,
                        "id": msg.get("id", ""),
                        "role": msg.get("role", ""),
                        "name": msg.get("name", ""),
                        "timestamp": msg.get("timestamp", ""),
                        "content": msg.get("content", []),
                    }
                    if msg.get("metadata"):
                        event["metadata"] = msg["metadata"]
                    messages_in_range.append(event)

        if not messages_in_range:
            continue

        output_lines = []
        if redact:
            header = redact_json_strings(header)
        output_lines.append(json.dumps(header, ensure_ascii=False))
        for event in messages_in_range:
            if redact:
                event = redact_json_strings(event)
            output_lines.append(json.dumps(event, ensure_ascii=False))

        out_filename = basename + ".jsonl"
        with open(out_dir / out_filename, "w", encoding="utf-8") as f:
            f.write("\n".join(output_lines) + "\n")

        event_count = len(output_lines) - 1
        print(f"  {container}/{out_filename} (copaw): {event_count} events")
        total_sessions += 1
        total_events += event_count

    return total_sessions, total_events


def export_agent_sessions(out_dir: Path, since_epoch: float, redact: bool,
                          container_filter: str | None) -> tuple[int, int]:
    """Export agent sessions from all containers. Returns (session_count, event_count)."""
    containers = list_hiclaw_containers()
    if container_filter:
        containers = [c for c in containers if container_filter in c]

    if not containers:
        print("  [sessions] No matching hiclaw containers found")
        return 0, 0

    total_sessions = 0
    total_events = 0

    for container in containers:
        runtime, sessions_dir = detect_runtime(container)
        if not runtime:
            print(f"  {container}: no sessions directory, skipped")
            continue

        container_dir = out_dir / container
        container_dir.mkdir(parents=True, exist_ok=True)

        if runtime == "openclaw":
            s, e = export_openclaw_sessions(container, sessions_dir, since_epoch,
                                            container_dir, redact)
        else:
            s, e = export_copaw_sessions(container, sessions_dir, since_epoch,
                                         container_dir, redact)

        if s == 0:
            container_dir.rmdir()
            print(f"  {container} ({runtime}): no sessions in range, skipped")
        else:
            total_sessions += s
            total_events += e

    return total_sessions, total_events


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(
        description="Export HiClaw debug logs (Matrix messages + agent sessions)"
    )
    parser.add_argument("--range", "-r", required=True, dest="time_range",
                        help="Time range to export, e.g. 10m, 1h, 1d")
    parser.add_argument("--container", "-c", default=None,
                        help="Only export sessions from containers matching this substring")
    parser.add_argument("--room", default=None,
                        help="Only export Matrix rooms matching this substring")
    parser.add_argument("--homeserver", "-s", default="",
                        help="Matrix homeserver URL (auto-detected from env file)")
    parser.add_argument("--token", "-t", default="",
                        help="Matrix access token (auto-detected from env file)")
    parser.add_argument("--env-file", default=None,
                        help="Path to hiclaw-manager.env (default: ~/hiclaw-manager.env)")
    parser.add_argument("--messages-only", action="store_true",
                        help="Only export m.room.message events (skip state events)")
    parser.add_argument("--no-redact", action="store_true",
                        help="Disable PII redaction")
    args = parser.parse_args()

    range_seconds = parse_range(args.time_range)
    since_epoch = time.time() - range_seconds
    since_human = datetime.fromtimestamp(since_epoch, tz=timezone.utc).isoformat()
    now_str = datetime.now().strftime("%Y%m%d-%H%M%S")

    run_dir = Path("debug-log") / now_str
    run_dir.mkdir(parents=True, exist_ok=True)
    redact = not args.no_redact

    print(f"HiClaw Debug Log Export")
    print(f"  Range: last {args.time_range} (since {since_human})")
    print(f"  Output: {run_dir.resolve()}")
    print(f"  PII redaction: {'on' if redact else 'off'}")
    print()

    # --- Matrix messages ---
    print("=== Matrix Messages ===")
    matrix_dir = run_dir / "matrix-messages"
    rooms, messages = export_matrix_messages(
        matrix_dir, since_epoch, redact,
        room_filter=args.room, env_file=args.env_file,
        homeserver=args.homeserver, token=args.token,
        messages_only=args.messages_only,
    )
    if rooms == 0 and matrix_dir.exists() and not any(matrix_dir.iterdir()):
        matrix_dir.rmdir()
    print()

    # --- Agent sessions ---
    print("=== Agent Sessions ===")
    sessions_dir = run_dir / "agent-sessions"
    sessions, events = export_agent_sessions(
        sessions_dir, since_epoch, redact,
        container_filter=args.container,
    )
    if sessions == 0 and sessions_dir.exists() and not any(sessions_dir.iterdir()):
        sessions_dir.rmdir()
    print()

    # --- Summary ---
    summary = (
        f"HiClaw Debug Log\n"
        f"Exported at: {now_str}\n"
        f"Range: last {args.time_range} (since {since_human})\n"
        f"PII redaction: {'on' if redact else 'off'}\n"
        f"\n"
        f"Matrix messages: {messages} messages from {rooms} rooms\n"
        f"Agent sessions: {events} events from {sessions} sessions\n"
    )
    (run_dir / "summary.txt").write_text(summary)

    print(f"Done. {messages} messages from {rooms} rooms, {events} events from {sessions} sessions")
    print(f"Output: {run_dir.resolve()}")


if __name__ == "__main__":
    main()
