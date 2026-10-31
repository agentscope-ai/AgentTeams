# -*- coding: utf-8 -*-
"""Worker-side dispatch surface: gateless headless CLI runs (design §4.2).

Semantics ported (read-only reference) from the workspace dispatcher:

- ``tools/dispatch.sh`` — spawn sequence, state file layout, claim flag;
- ``tools/dispatch_watch.py`` — watchdog loop (20s pid poll, terminal
  state written to file, 3-line terminal summary);
- ``tools/dispatch_notify.py`` — terminal-state wake: session inject
  ``POST /api/console/chat`` with retry on 409/429/503.

Dropped on purpose (10/22 gate-removal ruling): the parallel-limit
gate (``max_parallel``), the ``files_to_touch`` conflict gate, the
budget fuse, and the external claim check (``qc-claim.sh``). The claim
flag is kept: a dispatch with the same task name already ``running``
→ 409. ``files_to_touch`` is accepted by the API schema as pure
metadata only: never persisted, never consumed.

State is file-backed under ``<state_dir>/dispatch/<task_id>.json``
(atomic write, same pattern as install tasks) plus ``<task_id>.log``
(full captured stdout). A stale ``running`` task file (hard crash or
server exit mid-run) blocks re-dispatch of the same task name until
the file is removed manually — same policy as stale install task
files.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import os
import re
import shutil
import signal
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path
from typing import Any, Callable

from . import (
    approval,
    chats,
    interaction,
    millforge,
    notify,
    progress,
    qwen_status,
    runs,
    wake_queue,
)
from .endpoints import app_base
from .registry import CLIS
from .state import (
    APPROVAL_ROOTS_ENV,
    cli_home,
    prepare_cli_home,
    qwen_cli,
    state_dir,
)

#: Task ids are ``uuid4().hex``; the check also keeps task file names
#: safe against traversal via ``get_task`` / ``get_log`` input.
_TASK_ID_RE = re.compile(r"^[0-9a-f]{32}$")

#: Maximum characters of child stdout kept in the task file ``log_tail``.
_LOG_TAIL_LIMIT = 65536

#: Watchdog poll interval (s); same default as dispatch_watch.py.
_DEFAULT_WATCH_INTERVAL = 20.0

#: Env override for the poll interval (tests / ops; dispatch.sh uses
#: DISPATCH_WATCH_INTERVAL for the same purpose).
WATCH_INTERVAL_ENV = "QWENPAW_DISPATCH_WATCH_INTERVAL"

#: Default per-task wall-clock timeout (s). Bumped 3600 -> 7200 in v0.2.0
#: (B1): unattended coding runs routinely need 1-2 h. The watchdog is now
#: always bounded — a missing/zero/``None`` timeout falls back to this
#: default instead of running unbounded.
DEFAULT_DISPATCH_TIMEOUT = 7200.0

#: Cancel/stop defaults (v0.2.0 A): SIGTERM first, SIGKILL after grace.
DEFAULT_CANCEL_GRACE_S = 10.0
MAX_CANCEL_GRACE_S = 60.0
#: ``wait=true`` polls for the terminal state at most this long (s).
WAIT_TIMEOUT_S = 5.0

#: Terminal task states (cancel idempotency). ``stale`` is included: a
#: reaper-closed task is as good as terminal for cancellation purposes.
TERMINAL_TASK_STATES = ("done", "failed", "timeout", "cancelled", "stale")

#: Log tail truncation (v0.2.0 B2): default 200 lines, cap 2000, 0 = full.
LOG_TAIL_DEFAULT = 200
LOG_TAIL_MAX = 2000

#: Progress event truncation (v0.2.0 B3): last 200 stream-json events,
#: cap 2000, 0 = all events.
PROGRESS_EVENTS_DEFAULT = 200
PROGRESS_EVENTS_MAX = 2000

_DEFAULT_WAKE_URL = ""  # resolved dynamically via endpoints.app_base
_WAKE_ENDPOINT = "/api/console/chat"
_WAKE_ATTEMPTS = 3

#: Approval tiers accepted by the qwen CLI. Resolution order is 1:1 with
#: ``run-coding-cli.sh`` (#1379): per-run request > env > default "yolo".
APPROVAL_MODES: tuple[str, ...] = (
    "plan",
    "default",
    "auto-edit",
    "auto",
    "yolo",
)

#: Default tier for unattended dispatch (10/22 ruling).
DEFAULT_APPROVAL_MODE = "yolo"

#: Env override mirroring the runner's config fallback (#1379).
APPROVAL_MODE_ENV = "QWEN_CODE_BRIDGE_APPROVAL_MODE"
LEGACY_APPROVAL_MODE_ENV = "AGENTTEAMS_CODING_CLI_APPROVAL_MODE"

#: QwenPaw tool-execution level -> Qwen Code approval tier.
#: STRICT = everything needs approval -> plan (analyze only, never acts);
#: SMART  = low-risk auto -> auto-edit (edits auto, shell still gated);
#: AUTO   = only guarded tools -> auto (classifier blocks risky actions);
#: OFF    = guard disabled -> yolo (everything approved).
APPROVAL_LEVEL_MAP: dict[str, str] = {
    "STRICT": "plan",
    "SMART": "auto-edit",
    "AUTO": "auto",
    "OFF": "yolo",
}


def map_level_to_mode(level: str | None) -> str:
    """Map a QwenPaw approval level to a Qwen Code approval tier."""
    return APPROVAL_LEVEL_MAP.get(str(level or "").strip().upper(), "")

#: Headless spawn args, 1:1 with the production adapter.
#: Authoritative source: ``tools/dispatch_adapters.json`` (entry "qwen",
#: key "args"); ``{approval_mode}`` / ``{sid}`` / ``{prompt}`` are
#: substituted in ``start()``.
SPAWN_ARG_TEMPLATE: tuple[str, ...] = (
    "--approval-mode",
    "{approval_mode}",
    "--channel",
    "CI",
    "-o",
    "stream-json",
    "--chat-recording",
    "--session-id",
    "{sid}",
    "-p",
    "{prompt}",
)

#: Tiers that need a live approver: the CLI is spawned with
#: ``--input-format stream-json`` so the host can answer ``can_use_tool``
#: control requests (worker 代审批). ``yolo``/``plan`` need no approver.
STREAM_INPUT_MODES: tuple[str, ...] = ("default", "auto-edit", "auto")


#: One reaper per dispatch dir (module-level so it survives per-request
#: service instances; the task itself survives a plugin/process restart).
_REAPERS: dict[str, asyncio.Task] = {}

#: One cancel lock per (dispatch dir, task) — module-level like _REAPERS
#: because the service is constructed per request. Serializes the cancel
#: endpoint against the spawn path (the pending-cancel race, v0.2.0 A).
_CANCEL_LOCKS: dict[str, asyncio.Lock] = {}


def build_spawn_plan(
    mode: str,
    session_id: str,
    prompt: str,
    max_wall_time: str | None = None,
    max_session_turns: int | None = None,
    allowed_tools: list[str] | None = None,
    resume: bool = False,
) -> tuple[list[str], bool]:
    """Return ``(argv, prompt_via_stdin)`` for one dispatch.

    ``default``/``auto-edit``/``auto`` run in bidirectional stream-json
    mode: no ``-p`` prompt, the first user turn is written to stdin and
    the channel stays open for ``can_use_tool`` control responses.
    ``yolo``/``plan`` keep the byte-compatible one-shot argv.
    """
    extra: list[str] = []
    if max_wall_time:
        extra += ["--max-wall-time", str(max_wall_time)]
    if max_session_turns:
        extra += ["--max-session-turns", str(int(max_session_turns))]
    if allowed_tools:
        extra += ["--allowed-tools", ",".join(str(t) for t in allowed_tools if str(t))]
    session_args = (
        ["--resume", session_id] if resume else ["--session-id", session_id]
    )
    if mode in STREAM_INPUT_MODES:
        return (
            [
                "--approval-mode",
                mode,
                "--channel",
                "CI",
                "-o",
                "stream-json",
                "--chat-recording",
            ]
            + session_args
            + [
                "--input-format",
                "stream-json",
            ]
            + extra,
            True,
        )
    args: list[str] = []
    for item in SPAWN_ARG_TEMPLATE:
        if item == "--session-id":
            # Replaced by session_args (--session-id or --resume).
            continue
        if item == "{sid}":
            args.extend(session_args)
        elif item == "{prompt}":
            args.append(prompt)
        elif item == "{approval_mode}":
            args.append(mode)
        else:
            args.append(item)
    return args + extra, False


def initialize_message(can_use_tool_timeout: int = 600) -> str:
    """SDK-mode handshake: the CLI enables its control system only after
    the host sends this first ``initialize`` control request."""
    return (
        json.dumps(
            {
                "type": "control_request",
                "request_id": uuid.uuid4().hex,
                "request": {
                    "subtype": "initialize",
                    "hooks": None,
                    "timeout": {"canUseTool": can_use_tool_timeout},
                },
            },
            ensure_ascii=False,
        )
        + "\n"
    )


def user_message(session_id: str, prompt: str) -> str:
    """The initial stream-json user turn (one line, newline-terminated)."""
    return (
        json.dumps(
            {
                "type": "user",
                "session_id": session_id,
                "message": {"role": "user", "content": prompt},
                "parent_tool_use_id": None,
            },
            ensure_ascii=False,
        )
        + "\n"
    )


#: Liveness decisions for a dispatch child (see ``decide_liveness``).
LIVENESS_KEEP = "keep"
LIVENESS_KILL = "kill"
LIVENESS_FINALIZE = "finalize"

#: A stale task whose pid is gone but whose watchdog may still be
#: draining is not marked stale until the task file has been quiet this
#: long (the stale mark races the watchdog finalize otherwise).
STALE_FRESH_SECONDS = 120.0


def decide_liveness(
    *,
    proc_alive: bool,
    progress_age: float,
    busy: bool | None,
    stall_after: float,
    silent_extensions: int = 0,
    hard_cap_left: bool = True,
) -> str:
    """Decide what to do with a dispatch child that reached its deadline.

    ``busy`` is three-state: ``True`` / ``False`` / ``None`` (probe
    unreachable — *unknown*, which must never be treated as idle).

    - child already exited -> finalize (never kill);
    - hard cap exhausted -> kill regardless of progress;
    - progress within the stall window -> keep (busy is irrelevant: the
      local event stream is the ground truth);
    - silent past the stall window -> one grace window when the probe is
      busy/unknown (long silent tool, transient probe failure), then kill;
      an explicit ``busy is False`` kills immediately.
    """
    if not proc_alive:
        return LIVENESS_FINALIZE
    if not hard_cap_left:
        return LIVENESS_KILL
    if float(progress_age) <= float(stall_after):
        return LIVENESS_KEEP
    if busy is False:
        return LIVENESS_KILL
    # busy is True or None (unknown): allow exactly one silent window.
    if silent_extensions < 1:
        return LIVENESS_KEEP
    return LIVENESS_KILL


def keep_running_after_timeout(
    busy: bool | None, progress_age: float, stall_after: float
) -> bool:
    """Backward-compatible wrapper over :func:`decide_liveness`."""
    return (
        decide_liveness(
            proc_alive=True,
            progress_age=progress_age,
            busy=busy,
            stall_after=stall_after,
        )
        == LIVENESS_KEEP
    )


def git_dirty_summary(cwd: str | None, limit: int = 3) -> str:
    """``N 个未提交改动 (a, b, c)`` for the wake; "" when clean/not a repo."""
    if not cwd or not os.path.isdir(cwd):
        return ""
    try:
        proc = subprocess.run(
            ["git", "-C", cwd, "status", "--porcelain"],
            capture_output=True,
            text=True,
            timeout=3,
        )
    except Exception:
        return ""
    if proc.returncode != 0:
        return ""
    lines = [line for line in (proc.stdout or "").splitlines() if line.strip()]
    if not lines:
        return ""
    paths = [line[3:].strip() for line in lines[:limit]]
    suffix = ", ".join(paths)
    more = "" if len(lines) <= limit else " …"
    return f"{len(lines)} 个未提交改动（{suffix}{more}）"


def ensure_dispatch_goals_disabled() -> bool:
    """Idempotently set ``goals.modelProposed=disabled`` for dispatch runs.

    The QwenCode setting is consent-affecting, so only the User / System /
    SystemDefaults scopes are honoured — workspace values are ignored.
    The dispatch home is the plugin's own ``cli_home``, so this writes the
    User scope there and preserves every other key.
    """
    try:
        import json as _json

        qdir = Path(prepare_cli_home(cli_home()))
        path = qdir / "settings.json"
        data: dict[str, Any] = {}
        if path.exists():
            try:
                loaded = _json.loads(path.read_text(encoding="utf-8"))
                if isinstance(loaded, dict):
                    data = loaded
            except (OSError, ValueError):
                data = {}
        goals = data.get("goals")
        if not isinstance(goals, dict):
            goals = {}
        if goals.get("modelProposed") == "disabled":
            return True
        goals["modelProposed"] = "disabled"
        data["goals"] = goals
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(
            _json.dumps(data, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        os.replace(tmp, path)
        return True
    except Exception:
        return False


def chat_sync_enabled() -> bool:
    """Sidebar chat sync switch: env > state/config.json > default true."""
    if os.environ.get("QWEN_CODE_BRIDGE_CHAT_SYNC", "1") == "0":
        return False
    try:
        cfg = state_dir() / "config.json"
        if cfg.exists():
            data = json.loads(cfg.read_text(encoding="utf-8"))
            if isinstance(data, dict) and "chat_sync" in data:
                return bool(data.get("chat_sync"))
    except (OSError, ValueError):
        pass
    return True


def resolve_approval_mode(
    requested: str | None = None,
    level: str | None = None,
) -> str:
    """Resolve the per-run approval tier (flag > env > default "yolo").

    Mirrors ``run-coding-cli.sh`` (#1379). Raises ``ValueError`` on an
    unknown tier so the API answers 400 instead of spawning a CLI with
    an invalid flag.
    """
    for raw in (
        requested,
        os.environ.get(APPROVAL_MODE_ENV, ""),
        os.environ.get(LEGACY_APPROVAL_MODE_ENV, ""),
    ):
        value = str(raw or "").strip()
        if not value:
            continue
        if value not in APPROVAL_MODES:
            raise ValueError(
                f"invalid approval_mode {value!r}; expected one of "
                + ", ".join(APPROVAL_MODES),
            )
        return value
    mapped = map_level_to_mode(level)
    if mapped:
        return mapped
    return DEFAULT_APPROVAL_MODE


class DispatchBusyError(RuntimeError):
    """Raised when a dispatch with the same task name is already running."""


class CliNotFoundError(RuntimeError):
    """Raised when the requested CLI binary is not on PATH."""


class SpawnFailedError(RuntimeError):
    """Raised when the child process cannot be started."""


class TaskNotFoundError(RuntimeError):
    """Raised when a cancel targets an unknown task id."""


class PidMismatchError(RuntimeError):
    """Raised when the task's pid fails the identity check (no signal sent)."""


class NotCancellableError(RuntimeError):
    """Raised when a task cannot be cancelled (protected/bad pid, EPERM)."""


#: ``wake_sender(session_id, agent_id, base_url, text) -> None``;
#: returns on success, raises on final failure. Senders accept an
#: ``approval_level`` keyword argument (empty string = fall back to the
#: agent profile's level).
WakeSender = Callable[..., None]


def load_task(task_id: str) -> dict[str, Any] | None:
    """Read one dispatch task file (read-only; used by the run-chat view)."""
    if not _TASK_ID_RE.match(task_id or ""):
        return None
    try:
        path = state_dir() / "dispatch" / f"{task_id}.json"
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return None


def mark_wake_delivered(task_id: str) -> None:
    """Queue callback: flip a task's ``wake_status`` to ``sent``.

    Called from the durable wake queue after a delayed delivery; the task
    file is written with the same tmp+rename atomicity as the service.
    """
    task = load_task(task_id)
    if not task:
        return
    if str(task.get("wake_status") or "") == "sent":
        return
    task["wake_status"] = "sent"
    task["wake_delivered_at"] = _utcnow()
    try:
        path = state_dir() / "dispatch" / f"{task_id}.json"
        tmp = path.with_name(path.name + ".tmp")
        tmp.write_text(
            json.dumps(task, ensure_ascii=False, indent=1),
            encoding="utf-8",
        )
        os.replace(tmp, path)
    except Exception:
        pass


def wake_is_delivered(task_id: str) -> bool:
    """Queue callback: has this task's wake already been delivered?

    Read from the task file (the durable state), so a record that survived a
    crash after delivery is dropped instead of re-injecting the text."""
    task = load_task(task_id) or {}
    return str(task.get("wake_status") or "") == "sent"


def _queue_wake(
    task: dict[str, Any],
    wake: dict[str, Any],
    origin: dict[str, Any],
    text: str,
    session_id: str,
    error: str,
) -> None:
    """Persist one wake into the durable queue (shared by both call sites)."""
    # A task whose wake was already delivered must never be queued again: the
    # durable retry would re-inject the same text (the duplicate-wake reports).
    if str(task.get("wake_status") or "") == "sent":
        return
    wake_queue.enqueue(
        {
            "task_id": str(task.get("task_id") or ""),
            "text": text,
            "session_id": session_id,
            "chat_id": str(task.get("chat_id") or ""),
            "agent_id": str(wake.get("agent_id") or ""),
            "base_url": str(wake.get("base_url") or ""),
            "channel": str(
                origin.get("channel") or wake.get("channel") or "console"
            ),
            "user_id": str(
                origin.get("user_id") or wake.get("user_id") or "default"
            ),
            "room_bell": bool(wake.get("room_bell")),
            "approval_level": str(task.get("approval_level") or ""),
            "last_error": str(error)[:120],
        }
    )


def _effective_approval_level(agent_id: str) -> str:
    """Read the effective QwenPaw tool-execution level for the wake turn.

    Returns ``""`` when the host config is unavailable; the wake then
    omits ``request_context`` and the session default applies (never
    fail the wake over this).
    """
    try:
        from qwenpaw.config.config import load_agent_config
        from qwenpaw.config.utils import load_config

        aid = str(agent_id or "").strip()
        if not aid:
            aid = str(load_config().agents.active_agent or "default")
        profile = load_agent_config(aid)
        return str(getattr(profile, "approval_level", "") or "").strip()
    except Exception:  # pragma: no cover - host config always present
        return ""


def _pending_approval_line(base_url: str, session_id: str) -> str:
    """One compact line describing the newest pending approval ("" if none).

    Reads the host approval list; ``exact_target`` is the literal operation
    (e.g. the shell command) and ``reasoning`` is why the guard flagged it.
    Resolved approvals are not queryable — the service keeps no history.
    """
    root = app_base(base_url)
    url = (
        f"{root}/api/approval/list?session_id="
        + urllib.parse.quote(session_id, safe="")
    )
    try:
        with urllib.request.urlopen(url, timeout=3) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return ""
    items = [
        item
        for item in ((data or {}).get("pending_approvals") or [])
        if isinstance(item, dict)
    ]
    if not items:
        return ""
    items.sort(key=lambda x: float(x.get("created_at") or 0.0), reverse=True)
    newest = items[0]
    tool = str(
        newest.get("tool_display_name") or newest.get("tool_name") or "?"
    )
    target = str(newest.get("exact_target") or "")[:80]
    try:
        age = int(max(0.0, time.time() - float(newest.get("created_at") or 0.0)))
    except (TypeError, ValueError):
        age = -1
    sev = str(newest.get("severity") or "")
    line = f"待审批 {len(items)} 条 | 最近: {tool}"
    if target:
        line += f" → {target}"
    line += f" (age {age}s"
    if sev:
        line += f", {sev}"
    line += ")"
    reason = str(newest.get("reasoning") or "")[:60]
    if reason:
        line += f" reason={reason}"
    if len(items) > 1:
        others = ",".join(
            str(i.get("tool_name") or "?") for i in items[1:4]
        )
        line += f"; 其余 {len(items) - 1} 条: {others}"
    return line


def _wake_body(
    session_id: str,
    text: str,
    approval_level: str,
    channel: str = "console",
    user_id: str = "default",
) -> dict:
    """Chat payload; ``channel`` picks console vs matrix, ``request_context``
    pins the wake turn's approval level."""
    body: dict[str, Any] = {
        "channel": str(channel or "console"),
        "user_id": str(user_id or "default"),
        "session_id": session_id,
        "input": [
            {"content": [{"type": "text", "text": text}], "metadata": {}},
        ],
    }
    if approval_level:
        body["request_context"] = {"approval_level": approval_level}
    return body


def _send_room_bell(
    root: str,
    channel: str,
    session_id: str,
    user_id: str,
    text: str,
    agent_id: str,
) -> None:
    """Post the terminal summary to the room via POST /api/messages/send."""
    payload = {
        "channel": channel,
        "target_user": user_id,
        "target_session": session_id,
        "text": text,
    }
    req = urllib.request.Request(
        root + "/api/messages/send",
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "X-Agent-Id": agent_id or "AgentTeamGuide",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        resp.read(64)


def default_wake_sender(
    session_id: str,
    agent_id: str,
    base_url: str,
    text: str,
    channel: str = "console",
    user_id: str = "default",
    room_bell: bool = False,
    approval_level: str = "",
) -> None:
    """Real-HTTP session inject (dispatch_notify.py default backend).

    POSTs the terminal summary to ``<base>/api/console/chat`` with the
    ``X-Agent-Id`` header; retries on 409/429/503 and transient errors
    (3 attempts). Raises ``RuntimeError`` on final failure.

    ``approval_level`` is the dispatch-time capture and is only a last
    resort: the chat-level switch is client state, so the wake first uses
    the level last observed on that session (middleware recording), then
    the live agent profile, and only then the stale capture.
    """
    root = app_base(base_url)
    observed = ""
    try:
        from .session_levels import last_level

        observed = last_level(session_id)
    except Exception:
        observed = ""
    effective_level = (
        observed or _effective_approval_level(agent_id) or approval_level
    )
    body = _wake_body(
        session_id,
        text,
        effective_level,
        channel,
        user_id,
    )
    pending_line = _pending_approval_line(base_url, session_id)
    if pending_line:
        body["input"][0]["content"][0]["text"] += (
            "\n" + pending_line + "（唤醒回合请先处理）"
        )
    last = ""
    for attempt in range(_WAKE_ATTEMPTS):
        try:
            req = urllib.request.Request(
                root + _WAKE_ENDPOINT,
                data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
                headers={
                    "Content-Type": "application/json",
                    "X-Agent-Id": agent_id or "AgentTeamGuide",
                },
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=15) as resp:
                resp.read(64)
            if room_bell and str(channel or "console") != "console":
                _send_room_bell(
                    root,
                    str(channel),
                    session_id,
                    user_id,
                    text,
                    agent_id,
                )
            return
        except urllib.error.HTTPError as exc:
            last = f"http {exc.code}"
            if exc.code not in (409, 429, 503):
                break
        except Exception as exc:  # wake is best-effort: report, don't crash
            last = str(exc)[:120]
        if attempt + 1 < _WAKE_ATTEMPTS:
            time.sleep(2)
    raise RuntimeError(f"wake inject failed: {last}")


def _env_float(name: str, default: float) -> float:
    """Read a float env var, falling back to ``default`` on any error."""
    try:
        raw = os.environ.get(name, "")
        return float(raw) if str(raw).strip() else float(default)
    except (TypeError, ValueError):
        return float(default)


def _utcnow() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _parse_epoch(value: Any) -> float:
    """Parse an RFC3339 ``Z`` timestamp (task ``updated_at``) to epoch."""
    text = str(value or "").strip()
    if not text:
        return 0.0
    try:
        import calendar

        return float(calendar.timegm(time.strptime(text, "%Y-%m-%dT%H:%M:%SZ")))
    except (ValueError, TypeError):
        return 0.0


def _install_hint(cli: str) -> str:
    """409 hint text pointing at the install route for managed CLIs."""
    for spec in CLIS.values():
        if spec.bin_name == cli:
            return (
                f"CLI {cli!r} not found on PATH; install it via "
                f"POST /api/qwen-code-bridge/{spec.id}/install"
            )
    return (
        f"CLI {cli!r} not found on PATH; it is not a managed coding "
        "CLI, so no install route exists (install it manually or fix PATH)"
    )


def _timeout_evidence(task: dict[str, Any]) -> bool:
    """True when the wall budget is provably spent.

    The task carries a finite ``timeout`` and ``now - started >= timeout``.
    The recovery paths (reaper / read / cleanup / load-time orphan sweep)
    use this to write the decidable ``timeout`` terminal state instead of
    ``stale`` when the watchdog that owned the kill never wrote the
    terminal state (v0.2.0 fix: watchdog-lost incident left
    ``stale`` / ``timeout_killed_watchdog_lost``).
    """
    try:
        timeout = float(task.get("timeout") or 0)
    except (TypeError, ValueError):
        return False
    if timeout <= 0:
        return False
    raw = task.get("started_epoch")
    if isinstance(raw, (int, float)) and float(raw) > 0:
        started = float(raw)
    else:
        started = _parse_epoch(task.get("started_at"))
    if started <= 0:
        return False
    return time.time() - started >= timeout


class DispatchService:
    """File-backed dispatch task manager (one instance per request).

    All state lives under ``<state_dir>/dispatch/``; the service holds
    no mutable state beyond immutable configuration, so constructing a
    fresh instance per request is safe (same pattern as
    ``CodingCliService``).
    """

    def __init__(
        self,
        dispatch_dir: Path | None = None,
        watch_interval: float | None = None,
        wake_sender: WakeSender | None = None,
        pre_spawn_hook: Callable[[str], Any] | None = None,
    ) -> None:
        if dispatch_dir is None:
            dispatch_dir = state_dir() / "dispatch"
        self._dispatch_dir = Path(dispatch_dir)
        self._dispatch_dir.mkdir(parents=True, exist_ok=True)
        if watch_interval is None:
            raw = os.environ.get(WATCH_INTERVAL_ENV, "").strip()
            try:
                watch_interval = float(raw) if raw else _DEFAULT_WATCH_INTERVAL
            except ValueError:
                watch_interval = _DEFAULT_WATCH_INTERVAL
        self._watch_interval = max(watch_interval, 0.05)
        self._wake_sender = wake_sender or default_wake_sender
        # Test seam (v0.2.0 A): invoked (sync or async) after the task file
        # is written and before spawn — lets a test race a cancel into the
        # spawn window to prove the abort path. The default must be a
        # no-op, not None: start() calls the hook unconditionally, so a
        # bare None default would crash every real (hook-less) dispatch.
        self._pre_spawn_hook = pre_spawn_hook or (lambda _task_id: None)

    # ---- state IO (atomic, same pattern as install tasks) ----

    def _task_path(self, task_id: str) -> Path:
        return self._dispatch_dir / f"{task_id}.json"

    def _log_path(self, task_id: str) -> Path:
        return self._dispatch_dir / f"{task_id}.log"

    def _write_task(self, task: dict[str, Any]) -> None:
        path = self._task_path(task["task_id"])
        tmp = path.with_name(path.name + ".tmp")
        tmp.write_text(
            json.dumps(task, ensure_ascii=False, indent=1),
            encoding="utf-8",
        )
        os.replace(tmp, path)

    def _read_task(self, task_id: str) -> dict[str, Any] | None:
        if not _TASK_ID_RE.match(task_id or ""):
            return None
        try:
            return json.loads(
                self._task_path(task_id).read_text(encoding="utf-8")
            )
        except (OSError, ValueError):
            return None

    def _update_task(self, task_id: str, fields: dict[str, Any]) -> None:
        task = self._read_task(task_id)
        if task is not None:
            task.update(fields)
            self._write_task(task)

    # ---- queries ----

    @staticmethod
    def _pid_alive(pid: Any) -> bool:
        """Alive = /proc and ``ps`` both say the pid exists and is not dead.

        ``os.kill(pid, 0)`` alone is not enough: a **zombie** (process
        exited, parent has not reaped it) keeps the pid and still answers
        0, so a dead dispatch looked alive forever and neither the reaper
        nor ``cleanup`` could close it (#914). Read the ``/proc`` state
        and cross-check ``ps``; Z (zombie) / X (dead) mean gone.
        """
        try:
            value = int(pid)
        except (TypeError, ValueError):
            return False
        if value <= 0:
            return False
        proc_alive = False
        try:
            stat = Path(f"/proc/{value}/stat").read_text(encoding="utf-8")
            proc_state = stat.rsplit(")", 1)[1].split()[0]
            proc_alive = proc_state not in {"Z", "z", "X", "x"}
        except (OSError, IndexError, ValueError):
            proc_alive = False
        try:
            result = subprocess.run(
                ["ps", "-o", "stat=", "-p", str(value)],
                capture_output=True,
                text=True,
                timeout=3,
            )
        except Exception:
            # ps unavailable: trust /proc alone rather than guessing alive.
            return proc_alive
        ps_state = (result.stdout or "").strip()
        ps_alive = (
            result.returncode == 0
            and bool(ps_state)
            and not ps_state.startswith("Z")
        )
        return proc_alive and ps_alive

    def _mark_timeout(
        self,
        task: dict[str, Any],
        reason: str = "timeout_killed_watchdog_lost",
    ) -> dict[str, Any]:
        """Decidable terminal state for a task whose wall budget is spent.

        The watchdog that owned the kill (or the timeout itself) never
        wrote the terminal state — it was lost with its process. The
        budget evidence is deterministic, so the task gets ``timeout`` /
        ``timeout_killed`` instead of ``stale`` (v0.2.0 fix): callers can
        then branch on a decidable terminal state.
        """
        now = _utcnow()
        try:
            timeout = float(task.get("timeout") or 0)
        except (TypeError, ValueError):
            timeout = 0.0
        age = time.time() - float(task.get("started_epoch") or 0.0)
        task.update(
            {
                "state": "timeout",
                "exit_code": task.get("exit_code"),
                "death_reason": "timeout_killed",
                "finished_at": task.get("finished_at") or now,
                "updated_at": now,
                "terminal_summary": [
                    (
                        f"[{task.get('task')}] timeout · 预算 {timeout:.0f}s "
                        f"已用尽（已运行 {age:.0f}s），看门狗丢失，恢复路径写终态"
                    ),
                    f"evidence: {reason}",
                    "下一步：查看日志（/dispatch/{task_id}/log）或重新派单",
                ],
            }
        )
        self._write_task(task)
        return task

    def _mark_stale(
        self,
        task: dict[str, Any],
        reason: str = "crash_or_restart",
    ) -> dict[str, Any]:
        """A ``running``/``cancelling`` task whose child is gone can never
        finish by itself; give it a decidable terminal state.

        Guards against the watchdog-finalize race (10/23 incident):

        - a ``terminating`` marker means the watchdog killed the child and
          is writing the authoritative terminal state — never call that a
          crash;
        - a task file touched within ``STALE_FRESH_SECONDS`` may still be
          mid-finalize;
        - the age guard keeps the spawn window (pid not written yet) and
          short-lived test doubles from being reaped.
        """
        state = task.get("state")
        if state == "cancelling":
            # Cancel in flight and the child is already gone (the watcher
            # that should have written the terminal state is lost): close
            # it as cancelled so reads never dangle on a non-terminal
            # state (v0.2.0 fix; the live-watcher case still ends
            # cancelled via the watchdog's own terminal write).
            if (
                task.get("pid") is not None
                and not self._pid_alive(task.get("pid"))
            ):
                now = _utcnow()
                task.update(
                    {
                        "state": "cancelled",
                        "death_reason": "cancelled_by_user",
                        "cancelled_at": task.get("cancelled_at") or now,
                        "finished_at": task.get("finished_at") or now,
                        "updated_at": now,
                    }
                )
                self._write_task(task)
            return task
        if state != "running":
            return task
        if _timeout_evidence(task) and not self._pid_alive(task.get("pid")):
            # Budget spent and no live child: the watchdog that should
            # have written `timeout` is gone. Decidable terminal state
            # instead of `stale` (v0.2.0 fix: watchdog-lost incident).
            # Covers pid=None (lost spawn window) as well as dead pid.
            return self._mark_timeout(task, reason)
        age = time.time() - float(
            task.get("started_epoch") or 0.0
        )
        stale_after = float(
            os.environ.get("QWENPAW_DISPATCH_STALE_AFTER", "60") or 60
        )
        updated_epoch = _parse_epoch(task.get("updated_at")) or 0.0
        fresh = (time.time() - updated_epoch) < STALE_FRESH_SECONDS
        if (
            task.get("state") == "running"
            and task.get("pid") is not None
            and age > stale_after
            and not fresh
            and not self._pid_alive(task.get("pid"))
        ):
            reason_text = {
                "plugin_restart": "插件重启（原 watchdog 已退出，无终态记录）",
                "crash_or_restart": "进程已消失（崩溃或重启，无终态记录）",
                "timeout_killed_watchdog_lost": (
                    "超时已被看门狗终止，但终态未写入（看门狗丢失）"
                ),
            }.get(reason, "进程已消失（无终态记录）")
            task.update(
                {
                    "state": "stale",
                    "exit_code": None,
                    "death_reason": reason,
                    "finished_at": task.get("finished_at") or _utcnow(),
                    "updated_at": _utcnow(),
                    "terminal_summary": [
                        f"[{task.get('task')}] stale · {reason_text}",
                        "none: 无终态记录（原 watchdog 已随进程退出）",
                        "下一步：重新派单，或 POST /dispatch/cleanup 清理",
                    ],
                }
            )
            self._write_task(task)
        return task

    def _ensure_reaper(self) -> None:
        """Start the restart-recovery reaper once per dispatch dir.

        The child watchdog dies with the plugin process; after a restart a
        live pid has no watcher and a dead pid has no terminal writer. The
        reaper scans on read and closes both cases (S6).
        """
        key = str(self._dispatch_dir)
        existing = _REAPERS.get(key)
        if existing is not None and not existing.done():
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return
        _REAPERS[key] = loop.create_task(
            self._reaper_loop(), name=f"coding-cli-reaper-{key}"
        )

    async def _reaper_loop(self) -> None:
        while True:
            await asyncio.sleep(self._watch_interval)
            try:
                for task in self._scan_once():
                    await self._notify_external(task)
            except (asyncio.CancelledError, Exception):
                pass

    def _scan_once(self) -> list[dict[str, Any]]:
        """Recover tasks orphaned by a process restart (sync, testable).

        Never races a live watchdog and never kills a child whose event
        stream is still moving:

        - ``terminating`` / a fresh ``updated_at`` / a live ``watchdog_pid``
          all mean "someone else owns the terminal state";
        - a dead pid is stale only after the freshness guard;
        - a live pid is killed only when the progress file stopped moving
          past the stall window *and* the wall budget is spent.
        """
        changed: list[dict[str, Any]] = []
        stall_after = float(
            os.environ.get("QWEN_CODE_BRIDGE_STALL_TIMEOUT", "1800") or 1800
        )
        for path in sorted(self._dispatch_dir.glob("*.json")):
            try:
                task = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if task.get("state") != "running":
                continue
            pid = task.get("pid")
            if pid is None:
                # Lost spawn window (plugin died before the pid was
                # written). A spent budget is still a decidable terminal
                # state — never leave `running` without a pid past its
                # budget (v0.2.0 fix).
                if _timeout_evidence(task):
                    changed.append(self._mark_timeout(task, "pid_never_written"))
                continue
            age = time.time() - float(task.get("started_epoch") or 0)
            updated_epoch = _parse_epoch(task.get("updated_at")) or 0.0
            fresh = (time.time() - updated_epoch) < STALE_FRESH_SECONDS
            if not self._pid_alive(pid):
                if age > 60 and not fresh:
                    # `terminating` means the watchdog killed the child but
                    # never wrote the terminal state (it died or the plugin
                    # restarted). Recover it instead of skipping it forever
                    # (#914: four dispatches stayed `running` for hours).
                    reason = (
                        "timeout_killed_watchdog_lost"
                        if task.get("terminating")
                        else "crash_or_restart"
                    )
                    changed.append(self._mark_stale(task, reason))
                continue
            # A live child is owned by its watchdog only while the task file
            # is fresh. `watchdog_pid` is NOT a liveness signal by itself:
            # after a plugin/container restart the recorded pid is routinely
            # reused by the new app process, which made the reaper skip a
            # dead dispatch forever (#914).
            watchdog_pid = task.get("watchdog_pid")
            if watchdog_pid and fresh and self._pid_alive(watchdog_pid):
                continue
            timeout = float(task.get("timeout") or 0)
            if not timeout or age <= timeout + 60:
                continue
            # Wall budget spent: kill only when the event stream is also
            # silent for the stall window (progress file mtime).
            progress_path = self._dispatch_dir / f"{task['task_id']}.progress.json"
            try:
                progress_age = time.time() - progress_path.stat().st_mtime
            except OSError:
                progress_age = float("inf")
            if progress_age <= stall_after:
                continue
            try:
                os.kill(int(pid), 9)
            except OSError:
                pass
            # No progress file means "no observable progress" (inf); it must
            # not crash the display formatting — an OverflowError here used
            # to be swallowed by the reaper loop, leaving the task running
            # forever (#914 second root cause).
            progress_age_text = (
                f"{int(progress_age)}s"
                if progress_age != float("inf")
                else "unknown"
            )
            task.update(
                {
                    "state": "timeout",
                    "exit_code": None,
                    "death_reason": "timeout_killed_by_reaper",
                    "finished_at": _utcnow(),
                    "updated_at": _utcnow(),
                    "terminal_summary": [
                        f"[{task.get('task')}] timeout · 进程超预算且无进展，由重启恢复器终止",
                        f"无事件 {progress_age_text} · 超过 stall 窗口 {int(stall_after)}s",
                        "下一步：重新派单或查看日志",
                    ],
                }
            )
            self._write_task(task)
            changed.append(task)
        return changed

    async def _notify_external(self, task: dict[str, Any]) -> None:
        try:
            if not task.get("run_record"):
                log_text = await asyncio.to_thread(self.get_log, task["task_id"]) or ""
                rec = await asyncio.to_thread(
                    runs.write_run_record, task, log_text, task.get("runs_dir")
                )
                if rec:
                    self._update_task(
                        task["task_id"],
                        {"run_record": rec["record"], "run_log": rec["run"]},
                    )
        except Exception:
            pass
        wake = task.get("wake") or {}
        origin = task.get("origin") or {}
        sid = str(origin.get("session_id") or wake.get("session_id") or "").strip()
        if not sid:
            return
        # Wake is single-writer: the watchdog owns the terminal write and
        # sets "pending" before sending. The reaper must never send a
        # second wake for a task that already has one (10/23 incident:
        # stale wake + timeout wake for the same task).
        current = self._read_task(str(task.get("task_id") or "")) or {}
        current_wake = str(current.get("wake_status") or "")
        # "queued: ..." = already failed once and sits in the durable queue;
        # treat it as "has a wake" so the reaper never sends a duplicate.
        if (
            current_wake == "sent"
            or current_wake.startswith("pending")
            or current_wake.startswith("queued")
        ):
            return
        self._update_task(
            task["task_id"],
            {"wake_status": "pending", "updated_at": _utcnow()},
        )
        text = self._wake_text(
            task,
            str(task.get("state") or ""),
            task.get("exit_code"),
            float(task.get("wall_s") or 0),
        )
        # Queue semantics: never POST while the chat is running
        # (``/api/chats/{id}/status`` is exactly what the console frontend
        # polls before sending).  The POST's 409 stays as the last resort.
        busy = await asyncio.to_thread(
            wake_queue.chat_is_busy,
            str(wake.get("base_url") or ""),
            str(wake.get("agent_id") or ""),
            str(task.get("chat_id") or ""),
        )
        if busy:
            _queue_wake(task, wake, origin, text, sid, "chat busy")
            self._update_task(
                task["task_id"],
                {"wake_status": "queued: chat busy"},
            )
            return
        try:
            await asyncio.to_thread(
                self._wake_sender,
                sid,
                str(wake.get("agent_id") or ""),
                str(wake.get("base_url") or ""),
                text,
                str(origin.get("channel") or wake.get("channel") or "console"),
                str(origin.get("user_id") or wake.get("user_id") or "default"),
                bool(wake.get("room_bell")),
                approval_level=str(task.get("approval_level") or ""),
            )
            wake_queue.discard(str(task["task_id"]))
            self._update_task(task["task_id"], {"wake_status": "sent"})
        except Exception as exc:
            _queue_wake(task, wake, origin, text, sid, str(exc))
            self._update_task(
                task["task_id"],
                {"wake_status": f"queued: {str(exc)[:100]}"},
            )

    def running_task_named(self, task_name: str) -> dict[str, Any] | None:
        """Claim flag: a live ``running`` task file with the same task name."""
        for path in sorted(self._dispatch_dir.glob("*.json")):
            try:
                task = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if task.get("task") == task_name and task.get("state") == "running":
                return task
        return None

    def cleanup(self, kill: bool = False) -> dict[str, Any]:
        """Mark dead ``running`` tasks as stale (report-first).

        ``kill=True`` only removes nothing live: it is accepted for API
        symmetry; stale task files are kept for audit. Tasks whose wall
        budget is provably spent are closed as ``timeout`` (decidable),
        even on a fresh task file (v0.2.0 fix).
        """
        self._ensure_reaper()
        stale: list[str] = []
        for path in sorted(self._dispatch_dir.glob("*.json")):
            try:
                task = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if task.get("state") != "running":
                continue
            updated_epoch = _parse_epoch(task.get("updated_at")) or 0.0
            if (
                (time.time() - updated_epoch) < STALE_FRESH_SECONDS
                and not _timeout_evidence(task)
            ):
                continue
            if not self._pid_alive(task.get("pid")):
                # Same reason mapping as the reaper: a terminating task was
                # killed by its watchdog, which never wrote the terminal
                # state — that is not a crash (#914).
                reason = (
                    "timeout_killed_watchdog_lost"
                    if task.get("terminating")
                    else "crash_or_restart"
                )
                task = self._mark_stale(task, reason)
                if task.get("state") in ("stale", "timeout"):
                    stale.append(str(task.get("task_id")))
        return {"stale": stale, "count": len(stale), "kill": bool(kill)}

    def answer_question(
        self,
        task_id: str,
        request_id: str,
        answers: dict[str, Any],
        by: str = "human",
    ) -> bool:
        """Resolve one pending interaction request (answer endpoint)."""
        return interaction.store_for(str(self._dispatch_dir)).answer(
            task_id, request_id, answers, by
        )

    def pending_questions(self, task_id: str) -> list[dict[str, Any]]:
        """Open interaction requests for one task (poll endpoint)."""
        return interaction.store_for(str(self._dispatch_dir)).pending(task_id)

    def get_task(self, task_id: str) -> dict[str, Any] | None:
        self._ensure_reaper()
        task = self._read_task(task_id)
        if task is None:
            return None
        return self._mark_stale(task)

    def list_tasks(self) -> list[dict[str, Any]]:
        """All dispatch tasks, newest first (summary fields only)."""
        raw: list[dict[str, Any]] = []
        for path in sorted(self._dispatch_dir.glob("*.json")):
            try:
                raw.append(json.loads(path.read_text(encoding="utf-8")))
            except (OSError, ValueError):
                continue
        self._ensure_reaper()
        raw = [self._mark_stale(t) for t in raw]
        raw.sort(key=lambda t: t.get("started_epoch") or 0.0, reverse=True)
        return [
            {
                key: task.get(key)
                for key in (
                    "task_id",
                    "task",
                    "cli",
                    "state",
                    "pid",
                    "exit_code",
                    "started_at",
                    "finished_at",
                    "updated_at",
                    "progress",
                    "approvals",
                    "escalations",
                    "terminal_summary",
                )
            }
            for task in raw
        ]

    def get_log(self, task_id: str) -> str | None:
        """Full captured stdout, or ``None`` if the task does not exist."""
        if self._read_task(task_id) is None:
            return None
        try:
            return self._log_path(task_id).read_text(
                encoding="utf-8", errors="replace"
            )
        except OSError:
            return ""

    # ---- truncated views (v0.2.0 B2/B3) ----

    @staticmethod
    def _normalize_count(value: Any, default: int, cap: int) -> int:
        """Coerce a tail/limit query param: int, clamped to [0, cap]."""
        try:
            count = int(value)
        except (TypeError, ValueError):
            return default
        return max(0, min(count, cap))

    def get_log_tail(
        self, task_id: str, tail: int = LOG_TAIL_DEFAULT
    ) -> tuple[str, bool, int, int]:
        """Tail-truncated log (B2): ``(text, truncated, total_lines, tail)``.

        ``tail=0`` is the explicit full-log opt-in; otherwise the last
        ``tail`` lines are kept (cap :data:`LOG_TAIL_MAX`).
        """
        tail_eff = self._normalize_count(tail, LOG_TAIL_DEFAULT, LOG_TAIL_MAX)
        full = self.get_log(task_id) or ""
        lines = full.splitlines()
        if tail_eff == 0 or len(lines) <= tail_eff:
            return full, False, len(lines), tail_eff
        text = "\n".join(lines[-tail_eff:])
        if full.endswith("\n"):
            text += "\n"
        return text, True, len(lines), tail_eff

    def get_progress_events(
        self, task_id: str, limit: int = PROGRESS_EVENTS_DEFAULT
    ) -> tuple[list[dict[str, Any]], bool, int, int]:
        """Last stream-json events of the log (B3).

        Returns ``(events, truncated, total_events, limit)``; ``limit=0``
        is the explicit all-events opt-in (cap :data:`PROGRESS_EVENTS_MAX`).
        Non-JSON log lines are skipped.
        """
        limit_eff = self._normalize_count(
            limit, PROGRESS_EVENTS_DEFAULT, PROGRESS_EVENTS_MAX
        )
        full = self.get_log(task_id) or ""
        events: list[dict[str, Any]] = []
        for line in full.splitlines():
            stripped = line.strip()
            if not stripped.startswith("{"):
                continue
            try:
                event = json.loads(stripped)
            except ValueError:
                continue
            if isinstance(event, dict):
                events.append(event)
        total = len(events)
        if limit_eff == 0 or total <= limit_eff:
            return events, False, total, limit_eff
        return events[-limit_eff:], True, total, limit_eff

    # ---- cancel / stop (v0.2.0 A) ----

    def _cancel_lock(self, task_id: str) -> asyncio.Lock:
        key = f"{self._dispatch_dir}:{task_id}"
        lock = _CANCEL_LOCKS.get(key)
        if lock is None:
            lock = asyncio.Lock()
            _CANCEL_LOCKS[key] = lock
        return lock

    @staticmethod
    def _clamp_grace(value: Any) -> float:
        """Grace window in [0, MAX_CANCEL_GRACE_S]; bad input -> default."""
        try:
            grace = float(value)
        except (TypeError, ValueError):
            return float(DEFAULT_CANCEL_GRACE_S)
        if grace != grace:  # NaN
            return float(DEFAULT_CANCEL_GRACE_S)
        return max(0.0, min(grace, MAX_CANCEL_GRACE_S))

    def _verify_kill_target(self, task: dict[str, Any]) -> int:
        """Return the verified child pid, or raise (no signal is sent).

        Hard safety rules (v0.2.0 A §1.3):
        - pid must be an int and not protected (pid <= 1, self, parent);
        - ``/proc/<pid>/cmdline`` must name *this* dispatch: argv[0]'s
          basename equals the task's CLI executable name AND the task's
          ``session_id`` appears in the cmdline. Both required, else
          :class:`PidMismatchError`.
        """
        raw_pid = task.get("pid")
        try:
            pid = int(raw_pid)
        except (TypeError, ValueError):
            raise NotCancellableError(
                f"task pid is not an integer: {raw_pid!r}"
            ) from None
        if pid <= 1 or pid == os.getpid() or pid == os.getppid():
            raise NotCancellableError(f"refusing to signal protected pid {pid}")
        try:
            cmdline = Path(f"/proc/{pid}/cmdline").read_bytes().decode(
                "utf-8", "replace"
            )
        except OSError:
            return pid  # already gone: let the kill attempt surface it
        parts = [part for part in cmdline.split("\0") if part]
        cli_name = str(task.get("cli") or "qwen").split("/")[-1]
        session_id = str(task.get("session_id") or "")
        # argv[0] is the interpreter for a Node/Python entrypoint
        # (``node /…/qwen …``), so match the CLI name against *any*
        # argv element rather than argv[0] alone.
        cli_ok = any(part.split("/")[-1] == cli_name for part in parts)
        sid_ok = bool(session_id) and session_id in cmdline
        if not (cli_ok and sid_ok):
            raise PidMismatchError(
                f"pid {pid} cmdline does not match this dispatch "
                f"(cli={cli_name!r}, session_id={session_id[:8] or '?'}); "
                "no signal sent"
            )
        return pid

    async def _cancel_escalate(
        self, task_id: str, pid: int, grace: float
    ) -> None:
        """SIGKILL a still-alive cancelled child after the grace window.

        Background follow-up to ``request_cancel`` (the request returns
        immediately; the escalation must not block it). Both signals are
        recorded in the task's ``signals[]``.
        """
        await asyncio.sleep(grace)
        try:
            if not self._pid_alive(pid):
                return
            os.kill(pid, signal.SIGKILL)
        except (OSError, ProcessLookupError):
            return
        task = self._read_task(task_id)
        if task is None:
            return
        signals = list(task.get("signals") or [])
        signals.append({"ts": _utcnow(), "signal": "SIGKILL", "pid": pid})
        self._update_task(task_id, {"signals": signals, "updated_at": _utcnow()})

    async def request_cancel(
        self,
        task_id: str,
        reason: str = "",
        grace_s: float = DEFAULT_CANCEL_GRACE_S,
        wait: bool = False,
    ) -> dict[str, Any]:
        """Cancel/stop one task (idempotent; never signals an unverified pid).

        State machine (v0.2.0 A §1.2):

            pending / running(pid=None)  -> cancelled   (before spawn)
            running(pid)                 -> cancelling  -> cancelled
                                              (watcher writes the terminal)
            done/failed/timeout/cancelled/stale -> no-op (already_terminal)

        This method only writes ``cancelling`` + ``cancel_requested_at``;
        the terminal state is written by the watchdog, which avoids racing
        the watchdog on the terminal write. With ``wait=True`` the call
        polls for the terminal state for at most :data:`WAIT_TIMEOUT_S`.
        """
        task = self._read_task(task_id)
        if task is None:
            raise TaskNotFoundError(task_id)
        previous_state = str(task.get("state") or "")
        if previous_state in TERMINAL_TASK_STATES:
            return {
                "ok": True,
                "task_id": task_id,
                "previous_state": previous_state,
                "state": previous_state,
                "pid": task.get("pid"),
                "signalled": None,
                "already_terminal": True,
            }
        grace = self._clamp_grace(grace_s)
        signalled = None
        async with self._cancel_lock(task_id):
            task = self._read_task(task_id)
            if task is None:
                raise TaskNotFoundError(task_id)
            state = str(task.get("state") or "")
            if state in TERMINAL_TASK_STATES:
                return {
                    "ok": True,
                    "task_id": task_id,
                    "previous_state": state,
                    "state": state,
                    "pid": task.get("pid"),
                    "signalled": None,
                    "already_terminal": True,
                }
            pid = task.get("pid")
            now = _utcnow()
            if state == "pending" or (state == "running" and pid is None):
                # Pending: never spawned. The spawn path re-checks under
                # the same lock and will not start a cancelled task.
                self._update_task(
                    task_id,
                    {
                        "state": "cancelled",
                        "death_reason": "cancelled_before_spawn",
                        "cancel_requested_at": now,
                        "cancel_reason": str(reason or "") or None,
                        "cancelled_at": now,
                        "finished_at": now,
                        "updated_at": now,
                    },
                )
                return {
                    "ok": True,
                    "task_id": task_id,
                    "previous_state": state,
                    "state": "cancelled",
                    "pid": None,
                    "signalled": None,
                    "already_terminal": False,
                }
            if state == "cancelling":
                # In progress: a repeated cancel never re-signals.
                return {
                    "ok": True,
                    "task_id": task_id,
                    "previous_state": "cancelling",
                    "state": "cancelling",
                    "pid": task.get("pid"),
                    "signalled": None,
                    "already_terminal": False,
                }
            kill_pid = self._verify_kill_target(task)
            try:
                os.kill(kill_pid, signal.SIGTERM)
            except ProcessLookupError:
                signalled = None  # exited in the gap; the watcher finalizes
            except OSError as exc:
                raise NotCancellableError(
                    f"cannot signal pid {kill_pid}: {exc}"
                ) from exc
            else:
                signalled = "SIGTERM"
            signals = list(task.get("signals") or [])
            if signalled:
                signals.append(
                    {"ts": now, "signal": signalled, "pid": kill_pid}
                )
            self._update_task(
                task_id,
                {
                    "state": "cancelling",
                    "cancel_requested_at": now,
                    "cancel_reason": str(reason or "") or None,
                    "signals": signals,
                    "updated_at": now,
                },
            )
            if signalled and grace > 0:
                asyncio.get_running_loop().create_task(
                    self._cancel_escalate(task_id, kill_pid, grace),
                    name=f"cancel-escalate-{task_id}",
                )
        if wait:
            deadline = time.monotonic() + WAIT_TIMEOUT_S
            while time.monotonic() < deadline:
                await asyncio.sleep(0.1)
                current = self._read_task(task_id)
                if (
                    current is not None
                    and str(current.get("state") or "") in TERMINAL_TASK_STATES
                ):
                    break
        final = self._read_task(task_id) or {}
        return {
            "ok": True,
            "task_id": task_id,
            "previous_state": previous_state,
            "state": str(final.get("state") or ""),
            "pid": final.get("pid"),
            "signalled": signalled,
            "already_terminal": False,
        }

    # ---- lifecycle ----

    async def start(
        self,
        task_name: str,
        prompt: str,
        cwd: str | None,
        cli: str,
        timeout: float,
        wake: dict[str, Any] | None,
        origin: dict[str, Any] | None = None,
        approval_mode: str | None = None,
        approval_level: str | None = None,
        evidence: str | None = None,
        max_wall_time: str | None = None,
        max_session_turns: int | None = None,
        allowed_tools: list[str] | None = None,
        resume_session_id: str | None = None,
        runs_dir: str | None = None,
        worker: str | None = None,
        runner: str | None = None,
    ) -> str:
        """Spawn a headless CLI run; returns the new task id.

        The child is ``<cli> --approval-mode <tier> --channel CI
        -o stream-json --session-id <uuid> -p <prompt>`` (args 1:1 with
        ``tools/dispatch_adapters.json``, design §4.2) with
        ``QWENPAW_SESSION_ID`` set in its env — the spawn path is
        plugin-controlled, so no middleware is involved. ``approval_mode``
        resolves flag > env > default "yolo" (#1379 parity).

        Raises:
            DispatchBusyError: same task name already running (claim flag).
            CliNotFoundError: ``cli`` binary not on PATH (hint in message).
            ValueError: ``cwd`` given but not a directory, or an invalid
                ``approval_mode``.
            SpawnFailedError: the child process could not be started.
        """
        # B1: the budget must always be finite — bad/missing input falls
        # back to DEFAULT_DISPATCH_TIMEOUT instead of "None = unbounded".
        try:
            timeout = float(timeout)
        except (TypeError, ValueError):
            timeout = float(DEFAULT_DISPATCH_TIMEOUT)
        if not timeout or timeout <= 0:
            timeout = float(DEFAULT_DISPATCH_TIMEOUT)
        _agent_hint = str((wake or {}).get("agent_id") or "")
        # Consent-affecting QwenCode setting: dispatch runs have no human
        # sitting in front of the propose_goal dialog, so the tool is
        # removed at the User scope of the dispatch CLI home (workspace
        # values are ignored by the CLI). Idempotent, best-effort.
        ensure_dispatch_goals_disabled()
        _level = (
            str(approval_level or "").strip().upper()
            or _effective_approval_level(_agent_hint)
        )
        mode = resolve_approval_mode(approval_mode, _level)
        running = self.running_task_named(task_name)
        if running is not None:
            raise DispatchBusyError(
                f"task {task_name!r} is already running "
                f"(task_id={running.get('task_id')}, "
                f"pid={running.get('pid')}); wait for it to finish or "
                "remove its task file"
            )
        bin_path = None
        if cli in ("qwen", ""):
            try:
                configured = qwen_cli()
                if configured and os.path.exists(configured):
                    bin_path = configured
            except Exception:
                bin_path = None
        bin_path = bin_path or shutil.which(cli)
        if bin_path is None:
            raise CliNotFoundError(_install_hint(cli))
        cli_version = None
        try:
            probe = await asyncio.to_thread(
                subprocess.run,
                [bin_path, "--version"],
                capture_output=True,
                text=True,
                timeout=5,
            )
            cli_version = runs.parse_cli_version(probe.stdout or probe.stderr)
        except Exception:
            cli_version = None
        # Versions we have exercised end to end (smoke dispatch + approval
        # hook). A new upstream release is reported as unsupported until it
        # is added here, which is a warning rather than a hard block.
        tested_cli_versions = {"0.25.0", "0.26.0", "0.26.3"}
        cli_version_supported = (
            cli_version in tested_cli_versions if cli_version else None
        )
        if cwd:
            cwd = str(Path(cwd).expanduser())
            if not os.path.isdir(cwd):
                raise ValueError(f"cwd is not a directory: {cwd}")
        else:
            cwd = None

        task_id = uuid.uuid4().hex
        session_id = (
            str(resume_session_id).strip()
            if resume_session_id and str(resume_session_id).strip()
            else str(uuid.uuid4())
        )
        resumed_from = str(resume_session_id).strip() if resume_session_id else None
        task: dict[str, Any] = {
            "task_id": task_id,
            "task": task_name,
            "cli": cli,
            "cli_path": bin_path,
            "prompt": prompt,
            "cwd": cwd,
            "session_id": session_id,
            "state": "running",
            "pid": None,
            "exit_code": None,
            "timeout": timeout,
            "timeout_s": timeout,
            "started_at": _utcnow(),
            "started_epoch": time.time(),
            "updated_at": _utcnow(),
            "finished_at": None,
            "wall_s": None,
            "log_tail": "",
            "approval_mode": mode,
            "approval_level": _level or None,
            "approval_mode_source": (
                "request"
                if str(approval_mode or "").strip()
                else (
                    "level-request"
                    if str(approval_level or "").strip()
                    else (
                        "env"
                        if (
                            os.environ.get(APPROVAL_MODE_ENV, "").strip()
                            or os.environ.get(
                                LEGACY_APPROVAL_MODE_ENV, ""
                            ).strip()
                        )
                        else ("level-map" if _level else "default")
                    )
                )
            ),
            "origin": dict(origin) if origin else None,
            "wake": dict(wake) if wake else None,
            "wake_status": None,
            "input_format": "text",
            "approvals": {"allowed": 0, "denied": 0, "escalated": 0},
            "progress": None,
            "interaction_pending": 0,
            "death_reason": None,
            "cancel_requested_at": None,
            "cancel_reason": None,
            "signals": [],
            "cancelled_at": None,
            "evidence": str(evidence).upper() if evidence else None,
            "resumed_from": resumed_from,
            "runs_dir": runs_dir or None,
            "worker": worker or None,
            "runner": runner or None,
            "cli_version": cli_version,
            "cli_version_supported": cli_version_supported,
            "max_wall_time": max_wall_time,
            "max_session_turns": max_session_turns,
            "allowed_tools": list(allowed_tools or []),
        }
        self._write_task(task)

        env = dict(os.environ)
        env["QWENPAW_SESSION_ID"] = session_id
        try:
            # Run roots for the PreToolUse policy hook (hook_policy.py):
            # a per-task value, so it belongs in this spawn's environment
            # (the shared settings.json cannot carry per-task roots).
            # Same list the can_use_tool path passes to approval.decide.
            env[APPROVAL_ROOTS_ENV] = json.dumps(
                [cwd] if cwd else [os.getcwd()]
            )
            _home = cli_home()
            if _home:
                env["HOME"] = _home
                _prepared = prepare_cli_home(_home)
                if _prepared:
                    env["QWEN_HOME"] = _prepared
            _gitconfig = os.path.expanduser("~/.gitconfig")
            if os.path.exists(_gitconfig):
                env.setdefault("GIT_CONFIG_GLOBAL", _gitconfig)
        except Exception:
            pass
        spawn_args, prompt_via_stdin = build_spawn_plan(
            mode,
            session_id,
            prompt,
            max_wall_time=max_wall_time,
            max_session_turns=max_session_turns,
            allowed_tools=allowed_tools,
            resume=bool(resumed_from),
        )
        self._update_task(
            task_id,
            {"input_format": "stream-json" if prompt_via_stdin else "text"},
        )
        _hook_result = self._pre_spawn_hook(task_id)
        if inspect.isawaitable(_hook_result):
            await _hook_result
        async with self._cancel_lock(task_id):
            # A cancel can slip in between task creation and spawn (it
            # holds the same lock); a cancelled task must never start.
            fresh = self._read_task(task_id) or {}
            if str(fresh.get("state") or "") in TERMINAL_TASK_STATES:
                return task_id
            try:
                proc = await asyncio.create_subprocess_exec(
                    bin_path,
                    *spawn_args,
                    cwd=cwd,
                    env=env,
                    stdin=(
                        asyncio.subprocess.PIPE
                        if prompt_via_stdin
                        else asyncio.subprocess.DEVNULL
                    ),
                    stdout=asyncio.subprocess.PIPE,
                    stderr=asyncio.subprocess.STDOUT,
                )
            except OSError as exc:
                self._update_task(
                    task_id,
                    {
                        "state": "failed",
                        "finished_at": _utcnow(),
                        "updated_at": _utcnow(),
                        "log_tail": f"spawn failed: {exc}",
                    },
                )
                raise SpawnFailedError(
                    f"failed to spawn {cli!r}: {exc}"
                ) from exc

            self._update_task(task_id, {"pid": proc.pid})
        self._ensure_reaper()
        if prompt_via_stdin and proc.stdin is not None:
            try:
                proc.stdin.write(initialize_message().encode("utf-8"))
                proc.stdin.write(user_message(session_id, prompt).encode("utf-8"))
                await proc.stdin.drain()
            except Exception:  # the pump reports the run failure
                pass
        asyncio.get_running_loop().create_task(
            self._watch(task_id, proc, timeout),
            name=f"coding-cli-dispatch-{task_id}",
        )
        # 侧栏同步（P1）：在 console 左栏 "QwenCode" 分组里建一条会话记录；失败不影响派单
        _sync = chat_sync_enabled()
        if _sync:
          try:
              _wake = task.get("wake") or {}
              _origin = task.get("origin") or {}
              _root = chats.resolve_root(_wake.get("base_url"))
              _agent = chats.resolve_agent(_wake.get("agent_id"))
              _gid = await asyncio.to_thread(chats.ensure_group, _root, _agent)
              _cid = await asyncio.to_thread(
                  chats.create_run_chat,
                  _root,
                  _agent,
                  name=chats.run_chat_name(task_name, cli, "running"),
                  session_id=f"qwen-code-bridge:{task_id}",
                  user_id=str(_origin.get("user_id") or "default"),
                  group_id=_gid,
                  meta={"task_id": task_id, "cli": cli, "task": task_name},
              )
              self._update_task(
                  task_id,
                  {"chat_id": _cid, "chat_sync": "created" if _cid else "empty"},
              )
          except Exception as exc:  # best-effort
              self._update_task(
                  task_id,
                  {"chat_sync": f"failed: {str(exc)[:120]}"},
              )
        return task_id

    @staticmethod
    def _wake_text(
        task: dict[str, Any], status: str, code: Any, wall: float
    ) -> str:
        """3-line terminal summary (dispatch_watch.py inbox semantics)."""
        summary = task.get("terminal_summary") or []
        head = (
            str(summary[0])
            if summary
            else f"[dispatch] {task.get('task')} 终态={status} "
            f"exit={code} wall={wall}s"
        )
        tail_line = (
            str(summary[2])
            if len(summary) > 2
            else "下一步：按任务书验收（复跑门/抽查 diff/给推送命令）"
        )
        return "\n".join(
            [
                head,
                f"日志: GET /api/qwen-code-bridge/dispatch/"
                f"{task.get('task_id')}/log",
                tail_line,
            ]
        )

    async def _watch(
        self,
        task_id: str,
        proc: asyncio.subprocess.Process,
        timeout: float,
    ) -> None:
        """Watchdog: poll the child (default 20s), enforce the timeout,
        write the terminal state, then send the wake (best-effort).
        """
        started = time.time()
        # B1: the watchdog budget must always be bounded (never None).
        try:
            timeout = float(timeout)
        except (TypeError, ValueError):
            timeout = float(DEFAULT_DISPATCH_TIMEOUT)
        if not timeout or timeout <= 0:
            timeout = float(DEFAULT_DISPATCH_TIMEOUT)
        tail: list[str] = [""]
        killed = False
        killed_reason = "timeout_killed"
        meta = self._read_task(task_id) or {}
        stdin_mode = str(meta.get("input_format") or "") == "stream-json"
        roots = [str(meta.get("cwd") or os.getcwd())]
        stall_after = float(
            os.environ.get("QWEN_CODE_BRIDGE_STALL_TIMEOUT", "1800") or 1800
        )
        silent_extensions = 0
        # Safety net only: "let a live session finish" must still have an
        # upper bound so a hung-but-chatty session cannot run forever.
        hard_cap = max(3.0 * float(timeout or 0.0), 7200.0)
        try:
            wrapup_seconds = float(
                os.environ.get("QWEN_CODE_BRIDGE_WRAPUP", "0") or 0
            )
        except ValueError:
            wrapup_seconds = 0.0
        wrapup_injected = False
        # The reaper must not touch a task that still has a live watchdog.
        self._update_task(task_id, {"watchdog_pid": os.getpid()})
        tracker = progress.ProgressTracker(roots=roots, stall_after=stall_after)
        progress_path = self._dispatch_dir / f"{task_id}.progress.json"
        receipts_path = self._dispatch_dir / f"{task_id}.receipts.jsonl"
        approvals = {"allowed": 0, "denied": 0, "escalated": 0}
        escalations: list[dict[str, Any]] = []
        flags = {"stdin_closed": False}
        last_write = [0.0]
        stdin_lock = asyncio.Lock()

        async def _resolve_interaction(
            question: "interaction.PendingQuestion", request_id: str
        ) -> None:
            """Human -> agent -> default answer ladder for one question."""
            hold = _env_float(
                "QWEN_CODE_BRIDGE_HOLD_SECONDS",
                interaction.DEFAULT_HOLD_SECONDS,
            )
            grace = _env_float(
                "QWEN_CODE_BRIDGE_AGENT_GRACE",
                interaction.DEFAULT_AGENT_GRACE_SECONDS,
            )
            answers = await question.wait(hold)
            answered_by = "human"
            if answers is None:
                wake = meta.get("wake") or {}
                origin = meta.get("origin") or {}
                sid = str(
                    origin.get("session_id") or wake.get("session_id") or ""
                ).strip()
                await notify.notify_human(
                    task=meta,
                    title=f"[{meta.get('task')}] 人工未答，转为 agent 代答",
                    body=question.question_text,
                    payload={
                        "task_id": task_id,
                        "request_id": request_id,
                        "tool_name": question.tool_name,
                    },
                    event_type="needs_agent_answer",
                )
                if sid:
                    try:
                        await asyncio.to_thread(
                            self._wake_sender,
                            sid,
                            str(wake.get("agent_id") or ""),
                            str(wake.get("base_url") or ""),
                            (
                                f"[{meta.get('task')}] 人工未回答，请代答：\n"
                                f"{question.question_text}\n"
                                f"回填: POST /api/qwen-code-bridge/dispatch/"
                                f"{task_id}/answer "
                                f'{{"request_id":"{request_id}","choice":"..."}}'
                            ),
                            str(
                                origin.get("channel")
                                or wake.get("channel")
                                or "console"
                            ),
                            str(
                                origin.get("user_id")
                                or wake.get("user_id")
                                or "default"
                            ),
                            False,
                        )
                    except Exception:
                        pass
                answers = await question.wait(grace)
                answered_by = "agent"
            if answers:
                decision: dict[str, Any] = {
                    "behavior": "allow",
                    "message": "",
                    "updated_input": {
                        **(
                            question.tool_input
                            if isinstance(question.tool_input, dict)
                            else {}
                        ),
                        "answers": answers,
                    },
                }
                approvals["allowed"] += 1
            else:
                decision = {
                    "behavior": "deny",
                    "message": (
                        "headless run: no human answer within the window. "
                        "Make the most reasonable assumption, continue the "
                        "task, and record the assumption in the final report."
                    ),
                    "requires_human": True,
                }
                approvals["denied"] += 1
                escalations.append(
                    {
                        "tool": question.tool_name,
                        "rule": "interaction-timeout",
                        "reason": "no human or agent answer within the window",
                        "request_id": request_id,
                    }
                )
            await _write_stdin(
                json.dumps(
                    approval.control_response(request_id, decision),
                    ensure_ascii=False,
                )
                + "\n"
            )
            try:
                approval.append_receipt(
                    receipts_path,
                    approval.receipt_entry(
                        task_id,
                        question.tool_name,
                        question.tool_input,
                        decision,
                        request_id,
                    ),
                )
            except Exception:
                pass
            self._update_task(
                task_id,
                {
                    "approvals": dict(approvals),
                    "escalations": list(escalations),
                    "interaction_pending": len(
                        interaction.store_for(
                            str(self._dispatch_dir)
                        ).pending(task_id)
                    ),
                },
            )

        async def _write_stdin(payload: str) -> None:
            if not stdin_mode or flags["stdin_closed"] or proc.stdin is None:
                return
            async with stdin_lock:
                try:
                    proc.stdin.write(payload.encode("utf-8"))
                    await proc.stdin.drain()
                except Exception:
                    pass

        async def _close_stdin() -> None:
            if (
                stdin_mode
                and not flags["stdin_closed"]
                and proc.stdin is not None
            ):
                flags["stdin_closed"] = True
                try:
                    proc.stdin.close()
                except Exception:
                    pass

        def _persist_progress(state: str) -> None:
            snap = tracker.snapshot(state)
            try:
                progress.write_progress(progress_path, snap)
            except Exception:
                pass
            self._update_task(
                task_id,
                {"progress": snap, "updated_at": _utcnow()},
            )

        async def _handle_event(event: dict[str, Any]) -> None:
            kind = str(event.get("type") or "")
            if kind == "control_request":
                request = event.get("request") or {}
                request_id = str(event.get("request_id") or "")
                if str(request.get("subtype") or "") == "can_use_tool":
                    tool_name = str(request.get("tool_name") or "")
                    tool_input = request.get("input")
                    if tool_name in interaction.INTERACTION_TOOLS:
                        # Human-first interaction: notify the host surface,
                        # hold for the human, then hand the question to the
                        # dispatch owner (the agent), then default. Runs in
                        # the background so the event pump keeps flowing
                        # while the CLI waits for the control response.
                        question = interaction.store_for(
                            str(self._dispatch_dir)
                        ).create(
                            task_id=task_id,
                            request_id=request_id,
                            tool_name=tool_name,
                            tool_input=tool_input,
                        )
                        try:
                            surface = await notify.notify_human(
                                task=meta,
                                title=(
                                    f"[{meta.get('task')}] 需要你回答"
                                    f"（{tool_name}）"
                                ),
                                body=(
                                    question.question_text
                                    + "\n（3 分钟内无人回答将由 agent 代答）"
                                ),
                                payload={
                                    "task_id": task_id,
                                    "request_id": request_id,
                                    "tool_name": tool_name,
                                    "questions": question.questions,
                                },
                            )
                            question.notifications["human"] = surface
                            question.notified_at = time.time()
                        except Exception:
                            pass
                        asyncio.ensure_future(
                            _resolve_interaction(question, request_id)
                        )
                        return
                    decision = approval.decide(
                        tool_name,
                        tool_input,
                        roots=roots,
                    )
                    await _write_stdin(
                        json.dumps(
                            approval.control_response(request_id, decision),
                            ensure_ascii=False,
                        )
                        + "\n"
                    )
                    if decision.get("behavior") == "allow":
                        approvals["allowed"] += 1
                    else:
                        approvals["denied"] += 1
                    if decision.get("requires_human"):
                        approvals["escalated"] += 1
                        escalations.append(
                            {
                                "tool": str(request.get("tool_name") or ""),
                                "rule": decision.get("rule"),
                                "reason": decision.get("reason"),
                                "request_id": request_id,
                            }
                        )
                    try:
                        approval.append_receipt(
                            receipts_path,
                            approval.receipt_entry(
                                task_id,
                                str(request.get("tool_name") or ""),
                                request.get("input"),
                                decision,
                                request_id,
                            ),
                        )
                    except Exception:
                        pass
                    self._update_task(
                        task_id,
                        {
                            "approvals": dict(approvals),
                            "escalations": list(escalations),
                        },
                    )
                else:
                    await _write_stdin(
                        json.dumps(
                            {
                                "type": "control_response",
                                "response": {
                                    "subtype": "error",
                                    "request_id": request_id,
                                    "error": {
                                        "message": "unsupported control request"
                                    },
                                },
                            }
                        )
                        + "\n"
                    )
                return
            if kind in ("control_cancel_request", "control_response"):
                return
            tracker.feed(event)
            now = time.time()
            if now - last_write[0] >= 1.0:
                last_write[0] = now
                _persist_progress("running")
            if kind == "result":
                await _close_stdin()

        async def _pump() -> None:
            assert proc.stdout is not None
            with self._log_path(task_id).open("ab") as fh:
                while True:
                    line = await proc.stdout.readline()
                    if not line:
                        break
                    fh.write(line)
                    fh.flush()
                    text = line.decode("utf-8", "replace")
                    tail[0] = (tail[0] + text)[-_LOG_TAIL_LIMIT:]
                    self._update_task(
                        task_id,
                        {"log_tail": tail[0], "updated_at": _utcnow()},
                    )
                    stripped = text.strip()
                    if stripped.startswith("{"):
                        try:
                            event = json.loads(stripped)
                        except ValueError:
                            event = None
                        if isinstance(event, dict):
                            try:
                                await _handle_event(event)
                            except Exception:
                                pass

        pump = asyncio.ensure_future(_pump())
        timeout_extensions = 0
        try:
            while True:
                await asyncio.sleep(self._watch_interval)
                if proc.returncode is not None:
                    break
                if time.time() - started > timeout:
                    # The wall clock is not a liveness signal: the local
                    # event stream decides, with the CLI's live indicator
                    # only as a tie-breaker (three-state: unknown != idle).
                    elapsed = time.time() - started
                    busy: bool | None
                    try:
                        busy = await qwen_status.session_busy(
                            str(meta.get("cwd") or os.getcwd()),
                            str(meta.get("session_id") or ""),
                        )
                    except Exception:
                        busy = None
                    progress_age = time.time() - tracker.last_event
                    decision = decide_liveness(
                        proc_alive=True,
                        progress_age=progress_age,
                        busy=busy,
                        stall_after=stall_after,
                        silent_extensions=silent_extensions,
                        hard_cap_left=elapsed < hard_cap,
                    )
                    if decision == LIVENESS_KEEP:
                        timeout_extensions += 1
                        if progress_age > stall_after:
                            silent_extensions += 1
                        timeout = elapsed + stall_after
                        self._update_task(
                            task_id,
                            {
                                "timeout_extensions": timeout_extensions,
                                "silent_extensions": silent_extensions,
                                "timeout": timeout,
                                "stall_after": stall_after,
                                "busy_probe": busy,
                            },
                        )
                        continue
                    killed = True
                    killed_reason = (
                        "timeout_killed"
                        if elapsed < hard_cap
                        else "hard_cap_killed"
                    )
                    # Tell the reaper this kill is intentional: it must not
                    # classify the dead pid as a crash and send a second,
                    # misleading wake (10/23 incident).
                    self._update_task(
                        task_id,
                        {"terminating": True, "updated_at": _utcnow()},
                    )
                    await _close_stdin()
                    proc.kill()
                    break
                # Optional wrap-up window: ask the model to commit and
                # finish before the deadline (off unless the env is set).
                if (
                    wrapup_seconds > 0
                    and not wrapup_injected
                    and stdin_mode
                    and (timeout - (time.time() - started)) <= wrapup_seconds
                ):
                    wrapup_injected = True
                    self._update_task(
                        task_id,
                        {"wrapup_injected_at": _utcnow()},
                    )
                    await _write_stdin(
                        json.dumps(
                            {
                                "type": "user",
                                "message": {
                                    "role": "user",
                                    "content": (
                                        "收尾提醒：时间预算即将用尽。请立即停止新工作，"
                                        "先把已完成的改动 commit（显式 pathspec），"
                                        "再输出最终报告。"
                                    ),
                                },
                                "parent_tool_use_id": None,
                            },
                            ensure_ascii=False,
                        )
                        + "\n"
                    )
                # A finished turn (result seen) should not keep the process
                # alive waiting on stdin; reap it after a short grace.
                if (
                    tracker.result is not None
                    and time.time() - tracker.last_event > 10
                ):
                    await _close_stdin()
                    proc.kill()
                    break
            code = await proc.wait()
            try:
                await pump
            except (asyncio.CancelledError, Exception):
                pass
        finally:
            await _close_stdin()
            if not pump.done():
                pump.cancel()
                try:
                    await pump
                except (asyncio.CancelledError, Exception):
                    pass

        wall = round(time.time() - started, 1)
        now = _utcnow()

        task = self._read_task(task_id) or {}
        # v0.2.0 A: the watcher owns the terminal write; a cancel request
        # takes precedence over timeout/exit code, so the state is
        # `cancelled` (death_reason `cancelled_by_user`).
        cancelled = bool(task.get("cancel_requested_at"))
        if cancelled:
            status = "cancelled"
        elif killed:
            status = "timeout"
        elif code == 0:
            status = "done"
        else:
            status = "failed"
        snap = tracker.snapshot(status)
        try:
            progress.write_progress(progress_path, snap)
        except Exception:
            pass
        dirty = git_dirty_summary(str(task.get("cwd") or ""))
        tail_line = (
            f"失败: {snap['last_error']}"
            if snap.get("last_error")
            else (
                f"需人工: {len(escalations)} 条"
                f"（首条 {escalations[0]['tool']}: "
                f"{escalations[0]['reason']}；见 receipts）"
                if escalations
                else "下一步：按任务书验收（复跑门/抽查 diff/给推送命令）"
            )
        )
        if dirty:
            tail_line = f"{tail_line} · 工作树：{dirty}"
        terminal_summary = [
            (
                f"[{task.get('task')}] {status} · exit={code} · {wall}s · "
                f"{snap['turns']} turns · {snap['tool_calls']} tools · "
                f"approvals {approvals['allowed']}a/{approvals['denied']}d"
                f"/{approvals['escalated']}e"
            ),
            (
                f"tests {snap['tests']['passed']}p/{snap['tests']['failed']}f"
                f" · last={snap['last_tool']} {snap['last_target']}"
            ).strip(),
            tail_line,
        ]
        wake = task.get("wake") or {}
        origin = task.get("origin") or {}
        wake_session_id = str(
            origin.get("session_id") or wake.get("session_id") or ""
        ).strip()
        # Terminal write carries the wake status: "pending" until the
        # (best-effort) inject lands, "skipped" when there is no target.
        self._update_task(
            task_id,
            {
                "state": status,
                "exit_code": code,
                "wall_s": wall,
                "finished_at": now,
                "updated_at": now,
                "log_tail": tail[0],
                "death_reason": (
                    "cancelled_by_user"
                    if cancelled
                    else (
                        killed_reason
                        if killed
                        else ("completed" if code == 0 else "failed")
                    )
                ),
                "cancelled_at": task.get("cancelled_at")
                or (now if cancelled else None),
                "terminating": False,
                "wake_status": "pending" if wake_session_id else "skipped",
                "notify_tier": millforge.notify_tier(status),
                "progress": snap,
                "approvals": dict(approvals),
                "escalations": list(escalations),
                "terminal_summary": terminal_summary,
            },
        )

        # Q3: AgentTeams coding-runs index (only when runs_dir is configured)
        try:
            log_text = await asyncio.to_thread(self.get_log, task_id) or ""
            # Re-read AFTER the terminal update: the earlier snapshot still
            # says "running" (status would map to failed, exit/finished None).
            run_task = self._read_task(task_id) or task
            rec = await asyncio.to_thread(
                runs.write_run_record,
                run_task,
                log_text,
                run_task.get("runs_dir"),
            )
            if rec:
                self._update_task(
                    task_id,
                    {"run_record": rec["record"], "run_log": rec["run"]},
                )
        except Exception:
            pass

        # 侧栏同步（P1）：终态改名（状态进名字）
        _chat_id = str(task.get("chat_id") or "")
        if _chat_id:
            try:
                await asyncio.to_thread(
                    chats.rename_chat,
                    chats.resolve_root(wake.get("base_url")),
                    chats.resolve_agent(wake.get("agent_id")),
                    _chat_id,
                    chats.run_chat_name(
                        str(task.get("task") or ""),
                        str(task.get("cli") or ""),
                        status,
                    ),
                )
                self._update_task(task_id, {"chat_sync": f"renamed:{status}"})
            except Exception as exc:  # best-effort
                self._update_task(
                    task_id,
                    {"chat_sync": f"rename_failed: {str(exc)[:80]}"},
                )

        if wake_session_id:
            text = self._wake_text(task, status, code, wall)
            busy = await asyncio.to_thread(
                wake_queue.chat_is_busy,
                str(wake.get("base_url") or ""),
                str(wake.get("agent_id") or ""),
                str(task.get("chat_id") or ""),
            )
            if busy:
                wake_status = "queued: chat busy"
                _queue_wake(
                    task, wake, origin, text, wake_session_id, "chat busy"
                )
                self._update_task(
                    task_id,
                    {"wake_status": wake_status, "updated_at": _utcnow()},
                )
                return
            try:
                await asyncio.to_thread(
                    self._wake_sender,
                    wake_session_id,
                    str(wake.get("agent_id") or ""),
                    str(wake.get("base_url") or ""),
                    text,
                    str(origin.get("channel") or wake.get("channel") or "console"),
                    str(origin.get("user_id") or wake.get("user_id") or "default"),
                    bool(wake.get("room_bell")),
                    approval_level=str(task.get("approval_level") or ""),
                )
                wake_status = "sent"
                wake_queue.discard(str(task_id))
            except Exception as exc:  # wake is best-effort; state stands
                wake_status = f"queued: {str(exc)[:100]}"
                _queue_wake(
                    task, wake, origin, text, wake_session_id, str(exc)
                )
            self._update_task(
                task_id, {"wake_status": wake_status, "updated_at": _utcnow()}
            )


# ---- orphan cleanup on load (v0.2.0 B4) ----


def orphan_cleanup(dispatch_dir: str | Path | None = None) -> list[dict[str, Any]]:
    """Sweep stale dispatch tasks on plugin load (idempotent, never kills).

    Scans ``<state_dir>/dispatch/*.json`` and finalizes tasks the previous
    process left ``running``/``cancelling`` whose child pid no longer
    exists:

    - ``running`` + dead pid + wall budget spent -> ``timeout``
      (``timeout_killed``) — the decidable terminal state wins, even with
      the ``terminating`` marker set (v0.2.0 fix: watchdog-lost incident);
    - ``running``    + dead pid -> ``failed``    (``orphaned_on_reload``)
    - ``cancelling`` + dead pid -> ``cancelled`` (``cancelled_by_user``)

    Guards: tasks carrying the ``terminating`` marker belong to the
    in-process reaper and are skipped (except the spent-budget case
    above); a task whose ``updated_at`` is fresher than
    :data:`STALE_FRESH_SECONDS` may still be written by a live watchdog
    and is skipped; a task whose pid is still alive is never touched
    (this function must not kill anything). Returns the list of changed
    task dicts.
    """
    base = Path(dispatch_dir) if dispatch_dir else state_dir() / "dispatch"
    if not base.is_dir():
        return []
    changed: list[dict[str, Any]] = []
    now = _utcnow()
    for path in sorted(base.glob("*.json")):
        try:
            task = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if not isinstance(task, dict):
            continue
        state = str(task.get("state") or "")
        if state not in ("running", "cancelling"):
            continue
        pid = task.get("pid")
        pid_live = pid is not None and DispatchService._pid_alive(pid)
        if state == "running" and _timeout_evidence(task) and not pid_live:
            # Wall budget provably spent and no live child remains: the
            # watchdog that should have written `timeout` is gone. Decidable
            # terminal state instead of `stale` (v0.2.0 fix); this also
            # covers the `terminating` marker, previously skipped wholesale.
            fields: dict[str, Any] = {
                "state": "timeout",
                "death_reason": "timeout_killed",
                "exit_code": task.get("exit_code"),
            }
        elif task.get("terminating"):
            continue  # the reaper owns terminating tasks
        else:
            updated_epoch = _parse_epoch(task.get("updated_at"))
            if (time.time() - updated_epoch) < STALE_FRESH_SECONDS:
                continue  # possibly still written by a live watchdog
            if pid_live:
                continue  # live child: a watchdog may still be finishing it
            if state == "running":
                fields = {
                    "state": "failed",
                    "death_reason": "orphaned_on_reload",
                    "exit_code": task.get("exit_code"),
                }
            else:
                fields = {
                    "state": "cancelled",
                    "death_reason": "cancelled_by_user",
                    "exit_code": task.get("exit_code"),
                    "cancelled_at": task.get("cancelled_at") or now,
                }
        fields["finished_at"] = task.get("finished_at") or now
        fields["updated_at"] = now
        task.update(fields)
        tmp = path.with_name(path.name + ".tmp")
        tmp.write_text(
            json.dumps(task, ensure_ascii=False, indent=1),
            encoding="utf-8",
        )
        os.replace(tmp, path)
        changed.append(task)
    return changed


__all__ = [
    "APPROVAL_MODES",
    "APPROVAL_MODE_ENV",
    "DEFAULT_APPROVAL_MODE",
    "DEFAULT_CANCEL_GRACE_S",
    "DEFAULT_DISPATCH_TIMEOUT",
    "LOG_TAIL_DEFAULT",
    "LOG_TAIL_MAX",
    "MAX_CANCEL_GRACE_S",
    "PROGRESS_EVENTS_DEFAULT",
    "PROGRESS_EVENTS_MAX",
    "SPAWN_ARG_TEMPLATE",
    "TERMINAL_TASK_STATES",
    "WAIT_TIMEOUT_S",
    "WATCH_INTERVAL_ENV",
    "CliNotFoundError",
    "DispatchBusyError",
    "DispatchService",
    "NotCancellableError",
    "PidMismatchError",
    "SpawnFailedError",
    "TaskNotFoundError",
    "WakeSender",
    "default_wake_sender",
    "orphan_cleanup",
    "resolve_approval_mode",
]
