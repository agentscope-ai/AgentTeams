# artifact publishing, matrix attachments and sensitive-content scan (W1-a2: real module)
from __future__ import annotations

import hashlib

import json

import mimetypes

import os

import re

import sys

import time

import uuid

from pathlib import Path

from typing import Any

import urllib.error

import urllib.parse

import urllib.request

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.identity import _canonical_room_id
from mcp.common.io import _read_json
from mcp.common.paths import _task_state_path, _workspace_dir
from mcp.common.payload import _string_list
from mcp.room.matrix import _matrix_env, _team_leader_matrix_id
from mcp.room.mention import MATRIX_MENTION_MXID_RE, MATRIX_ROOM_RE, _matrix_content, _matrix_target
from mcp.transport.route import _reply_route
from .filesync import _normalize_shared_path

SENSITIVE_ARTIFACT_NAME_RE = re.compile(
    r"(secret|token|cookie|authorization|private[_-]?key|credential|client[_-]?secret)",
    re.IGNORECASE,
)


# Each pattern's group 1 (when present) captures the credential value so the
# scanner can whitelist placeholder templates. Patterns without a group
# (the private-key header) never need it: group(0) is used instead.
SENSITIVE_ARTIFACT_TEXT_RE = [
    re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----", re.IGNORECASE),
    re.compile(r"\bAuthorization\s*:\s*(?:Bearer|Basic)\s+(\S+)", re.IGNORECASE),
    re.compile(
        r"\b(?:access[_-]?key[_-]?secret|client[_-]?secret|secret[_-]?key|api[_-]?key|token)\b"
        r"\s*[:=]\s*['\"]?([A-Za-z0-9_./+=:-]{16,})",
        re.IGNORECASE,
    ),
]


# A value that is an *explicit* template is documentation, not a
# credential. Three wrapper families are recognized — angle brackets
# (``<YOUR_API_KEY>``), a single backtick code span
# (`` `YOUR_API_KEY` ``), and markdown bold (``**YOUR_API_KEY**``) —
# each optionally surrounded by quotes, because the Authorization
# header pattern captures raw non-space runs and documentation
# commonly wraps placeholders in any of the three. The exemption
# requires more than shape: the enclosed text must carry an explicit
# placeholder marker (case-insensitive, whole component) — your, my,
# our, paste, insert, replace, placeholder, example, sample, dummy,
# fake, redacted, masked, change[-_]?me, todo, tbd, here, xxx, yyy,
# zzz. Neither the kebab shape alone
# (``<sk-abcdefghijklmnopqrstuvwxyz>``) nor arbitrary label:value
# content (``<token:abc123def4567890>``) is exempt, in any wrapper
# family. Content nouns (token/key/secret/password/api) are NOT
# markers: they name what the value is, not that it is a placeholder.
# Additionally the content may not contain a run of 16+ consecutive
# alphanumerics (mirrors the scanner's minimum credential length).
PLACEHOLDER_MARKER_WORDS_RE = (
    r"(?<![A-Za-z0-9])(?:your|my|our|paste|insert|replace|"
    r"placeholder|example|sample|dummy|fake|redacted|masked|change[-_]?me|"
    r"todo|tbd|here|xxx|yyy|zzz)(?![A-Za-z0-9])"
)


def _placeholder_family(inner: str) -> str:
    """Content pattern for one wrapper family.

    ``inner`` is the family's excluded-character class (the wrapper's
    own delimiters plus whitespace); the marker-word requirement and
    the no-16+-alphanumeric-run rule are shared by every family.
    """
    return (
        rf"(?={inner}*{PLACEHOLDER_MARKER_WORDS_RE})"
        rf"(?!{inner}*[A-Za-z0-9]{{16,}})"
        rf"{inner}+"
    )


PLACEHOLDER_CREDENTIAL_RE = re.compile(
    r"^(?:['\"]*<" + _placeholder_family(r"[^<>\s]") + r">['\"]*"
    r"|['\"]*`" + _placeholder_family(r"[^`\s]") + r"`['\"]*"
    r"|['\"]*\*\*" + _placeholder_family(r"[^*\s]") + r"\*\*['\"]*)$",
    re.IGNORECASE,
)


MATRIX_ATTACHMENT_REL_TYPE = "com.agentteams.attachment"


MATRIX_ATTACHMENT_CONTEXT_FILE = "teamharness-matrix-context.json"


MATRIX_ATTACHMENT_CONTEXT_TTL_SECONDS = 30 * 60


ATTACHMENT_PARENT_EVENT_KEYS = (
    "parentEventId",
    "parent_event_id",
    "attachmentParentEventId",
    "attachment_parent_event_id",
    "matrixAttachmentParentEventId",
    "matrix_attachment_parent_event_id",
)


TEXT_ARTIFACT_SAMPLE_BYTES = 256 * 1024


def _attachment_parent_event_id(*sources: dict[str, Any]) -> str:
    for source in sources:
        if not isinstance(source, dict):
            continue
        for key in ATTACHMENT_PARENT_EVENT_KEYS:
            value = str(source.get(key) or "").strip()
            if value:
                return value
    return ""


def _matrix_attachment_context_path() -> Path | None:
    raw = os.getenv("TEAMHARNESS_MATRIX_CONTEXT_FILE", "").strip()
    if raw:
        return Path(raw)
    qwenpaw_dir = os.getenv("QWENPAW_WORKING_DIR", "").strip()
    if qwenpaw_dir:
        return Path(qwenpaw_dir) / MATRIX_ATTACHMENT_CONTEXT_FILE
    return None


def _matrix_attachment_context_parent_event_id(room_id: str) -> str:
    path = _matrix_attachment_context_path()
    if not path or not path.is_file():
        return ""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return ""
    if not isinstance(data, dict):
        return ""
    rooms = data.get("rooms")
    if not isinstance(rooms, dict):
        return ""
    record = rooms.get(_canonical_room_id(room_id)) or rooms.get(room_id)
    if not isinstance(record, dict):
        return ""
    try:
        updated_at = float(record.get("updatedAt") or record.get("updated_at") or 0)
    except (TypeError, ValueError):
        updated_at = 0
    if updated_at and time.time() - updated_at > MATRIX_ATTACHMENT_CONTEXT_TTL_SECONDS:
        return ""
    for key in ("attachmentParentEventId", "parentEventId", "eventId", "event_id"):
        value = str(record.get(key) or "").strip()
        if value:
            return value
    return ""


def _write_matrix_attachment_context_parent_event_id(room_id: str, event_id: str | None) -> bool:
    parent_event_id = str(event_id or "").strip()
    if not parent_event_id:
        return False
    path = _matrix_attachment_context_path()
    if not path:
        return False
    data: dict[str, Any] = {}
    try:
        if path.is_file():
            loaded = json.loads(path.read_text(encoding="utf-8"))
            if isinstance(loaded, dict):
                data = loaded
    except (OSError, json.JSONDecodeError):
        data = {}
    rooms = data.get("rooms")
    if not isinstance(rooms, dict):
        rooms = {}
        data["rooms"] = rooms
    rooms[_canonical_room_id(room_id)] = {
        "attachmentParentEventId": parent_event_id,
        "eventId": parent_event_id,
        "updatedAt": time.time(),
    }
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_name(f"{path.name}.{os.getpid()}.tmp")
        tmp.write_text(json.dumps(data, ensure_ascii=False, sort_keys=True), encoding="utf-8")
        tmp.replace(path)
    except OSError:
        return False
    return True


def _artifact_publish_result(source_path: str, filename: str = "", parent_event_id: str = "") -> dict[str, Any]:
    return {
        "sourcePath": source_path,
        "filename": filename,
        "size": None,
        "mimetype": "",
        "mxcUri": "",
        "eventId": "",
        "parentEventId": parent_event_id,
        "status": "skipped",
        "error": "",
    }


def _path_is_under(normalized: str, prefix: str) -> bool:
    return normalized == prefix or normalized.startswith(f"{prefix}/")


def _shared_dir_candidates() -> list[Path]:
    candidates: list[Path] = []
    for env_key in ("TEAMHARNESS_SHARED_DIR", "AGENTTEAMS_SHARED_DIR"):
        raw = os.getenv(env_key, "").strip()
        if raw:
            candidates.append(Path(raw).expanduser())
    return candidates


def _artifact_is_under_runtime_shared(workspace: Path, local_path: Path, normalized: str) -> bool:
    if not _path_is_under(normalized, "shared"):
        return False
    candidates = _shared_dir_candidates()
    if not candidates:
        return False
    try:
        workspace_shared = (workspace / "shared").resolve()
        local_resolved = local_path.resolve()
    except (OSError, RuntimeError):
        return False
    for shared_dir in candidates:
        try:
            shared_resolved = shared_dir.resolve()
            local_resolved.relative_to(shared_resolved)
        except (OSError, RuntimeError, ValueError):
            continue
        if workspace_shared == shared_resolved:
            return True
    return False


def _normalize_workspace_artifact_path(raw_path: str) -> tuple[str, bool]:
    raw = (raw_path or "").strip()
    if not raw or raw.startswith("/") or "\\" in raw:
        raise ValueError("artifact path must be a relative workspace path")
    parts = raw.strip("/").split("/")
    if any(part in {"", ".", ".."} for part in parts):
        raise ValueError("artifact path must be a relative workspace path without '.', '..', or empty segments")
    is_directory = raw.endswith("/")
    normalized = "/".join(parts)
    if is_directory:
        normalized += "/"
    return normalized, is_directory


def _resolve_workspace_artifact_path(arguments: dict[str, Any], source_path: str, expected_prefix: str) -> tuple[str, Path]:
    normalized, is_directory = _normalize_workspace_artifact_path(source_path)
    if is_directory:
        raise ValueError("artifact path must be a file")
    if expected_prefix and not _path_is_under(normalized, expected_prefix):
        raise ValueError(f"artifact path must be under {expected_prefix}/")
    workspace = _workspace_dir(arguments)
    parts = normalized.split("/")
    local_path = workspace / Path(*parts)
    try:
        local_path.resolve().relative_to(workspace.resolve())
    except ValueError as exc:
        if _artifact_is_under_runtime_shared(workspace, local_path, normalized):
            return normalized, local_path
        raise ValueError("artifact path must stay under workspace shared/") from exc
    return normalized, local_path


def _artifact_mimetype(path: Path) -> str:
    return mimetypes.guess_type(str(path))[0] or "application/octet-stream"


def _artifact_is_text(path: Path, mimetype: str) -> bool:
    if mimetype.startswith("text/"):
        return True
    if mimetype in {
        "application/json",
        "application/javascript",
        "application/xml",
        "application/x-yaml",
        "application/yaml",
        "application/toml",
    }:
        return True
    return path.suffix.lower() in {
        ".cfg",
        ".conf",
        ".css",
        ".csv",
        ".html",
        ".ini",
        ".js",
        ".json",
        ".jsx",
        ".log",
        ".md",
        ".py",
        ".rb",
        ".sh",
        ".sql",
        ".toml",
        ".ts",
        ".tsx",
        ".txt",
        ".xml",
        ".yaml",
        ".yml",
    }


def _artifact_text_has_sensitive_content(path: Path, mimetype: str) -> bool:
    if not _artifact_is_text(path, mimetype):
        return False
    sample = path.read_bytes()[:TEXT_ARTIFACT_SAMPLE_BYTES]
    if b"\x00" in sample:
        return False
    text = sample.decode("utf-8", errors="replace")
    for pattern in SENSITIVE_ARTIFACT_TEXT_RE:
        for match in pattern.finditer(text):
            value = match.group(1) if match.lastindex else match.group(0)
            if PLACEHOLDER_CREDENTIAL_RE.match(value):
                continue
            return True
    return False


def _matrix_upload_artifact(homeserver: str, token: str, path: Path, filename: str, mimetype: str) -> str:
    url = f"{homeserver}/_matrix/media/v3/upload?filename={urllib.parse.quote(filename)}"
    request = urllib.request.Request(
        url,
        data=path.read_bytes(),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": mimetype,
        },
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    mxc_uri = str(data.get("content_uri") or "").strip()
    if not mxc_uri:
        raise ValueError("Matrix media upload response missing content_uri")
    return mxc_uri


def _valid_mention_mxids(values: Any) -> list[str]:
    """Validate candidate mention values as full Matrix user IDs (MXIDs).

    A Matrix mention requires the full ``@localpart:domain`` form; short
    names or malformed values are dropped (with a warning) instead of
    being concatenated as plain text, which would not mention anyone.
    """
    mentions: list[str] = []
    for raw in _string_list(values):
        mxid = raw.strip()
        if not mxid:
            continue
        if not MATRIX_MENTION_MXID_RE.fullmatch(mxid):
            print(f"teamharness: dropping invalid Matrix mention {mxid!r}", file=sys.stderr)
            continue
        if mxid not in mentions:
            mentions.append(mxid)
    return mentions


def _matrix_send_file_summary(
    homeserver: str,
    token: str,
    encoded_room_id: str,
    file_txn_id: str,
    summary: str,
    mentions: list[str] | None,
) -> None:
    """Best-effort ``m.text`` companion event for a published file.

    The ``m.file`` event alone notifies nobody; the summary carries the
    ``m.mentions`` + matrix.to pill notification built by
    ``_matrix_content``. Its transaction id is the file event's id plus
    ``-summary`` so the pair is idempotent on retry. Notification-level
    failures never fail the publish: the file already reached the room.

    Mentions are prepended to the text when the text does not already
    contain the MXID, mirroring the completion/delegate notifications:
    ``_formatted_body`` only renders a pill for an MXID present in the
    text, and the plain ``body`` keeps the readable fallback.
    """
    mxids = _valid_mention_mxids(mentions)
    text = summary
    missing = [mxid for mxid in mxids if mxid not in text]
    if missing:
        text = " ".join(missing) + " " + text
    content = _matrix_content(text, mxids)
    summary_txn_id = f"{file_txn_id}-summary"
    url = (
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room_id}/send/"
        f"m.room.message/{urllib.parse.quote(summary_txn_id, safe='')}"
    )
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            response.read()
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        print(f"teamharness: artifact summary event failed: {exc}", file=sys.stderr)


def _artifact_publish_txn(canonical_room_id: str, filename: str, content_sha: str, publish_id: str) -> str:
    """Deterministic artifact publish transaction id, derived before upload.

    The seed is (room, filename, content hash, publish id) and
    deliberately excludes the uploaded MXC URI: a real homeserver may
    mint a different media URI for every upload of the same bytes, and a
    txn that depends on the response cannot deduplicate a retry. Same
    room + filename + content with no publish id retries to the same
    txn (the homeserver keeps one event pair even when the client lost
    the first response); changed content hashes to a new txn; a
    deliberate re-publish of unchanged content passes a fresh
    ``publish_id``.
    """
    seed = f"{canonical_room_id}|{filename}|{content_sha}|{publish_id}".encode("utf-8")
    return f"teamharness-file-{hashlib.sha256(seed).hexdigest()[:24]}"


def _matrix_send_file_event(
    homeserver: str,
    token: str,
    room_id: str,
    filename: str,
    mxc_uri: str,
    size: int,
    mimetype: str,
    parent_event_id: str = "",
    summary: str = "",
    mentions: list[str] | None = None,
    txn_id: str = "",
) -> str:
    """Send an ``m.file`` event, optionally followed by a summary event.

    When ``summary`` is non-empty, a second ``m.text`` event goes to the
    same room so the room members are actually notified about the file.
    The summary derives ``<file-txn>-summary`` from the file txn, so a
    retried publish re-PUTs the same two txns and the homeserver
    deduplicates instead of emitting a second summary.

    ``txn_id`` (when given) is the file transaction id the caller
    computed before the media upload via ``_artifact_publish_txn`` — it
    takes precedence over the local fallbacks below, which is what keeps
    a retry idempotent when the homeserver mints a fresh MXC URI per
    upload. Without it the legacy behavior is preserved: a
    deterministic (room + media + filename) id with a summary, otherwise
    a random single-use id.
    """
    content = {
        "msgtype": "m.file",
        "body": filename,
        "url": mxc_uri,
        "info": {
            "size": size,
            "mimetype": mimetype,
        },
    }
    if parent_event_id:
        content["m.relates_to"] = {
            "rel_type": MATRIX_ATTACHMENT_REL_TYPE,
            "event_id": parent_event_id,
        }
    canonical_room_id = _canonical_room_id(room_id)
    if not txn_id:
        if summary:
            stable_seed = f"{canonical_room_id}|{mxc_uri}|{filename}".encode("utf-8")
            txn_id = f"teamharness-file-{hashlib.sha256(stable_seed).hexdigest()[:24]}"
        else:
            txn_id = f"teamharness-file-{os.getpid()}-{int(time.time() * 1000)}-{uuid.uuid4().hex}"
    encoded_room_id = urllib.parse.quote(canonical_room_id, safe="")
    url = f"{homeserver}/_matrix/client/v3/rooms/{encoded_room_id}/send/m.room.message/{txn_id}"
    request = urllib.request.Request(
        url,
        data=json.dumps(content).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="PUT",
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        data = json.loads(response.read().decode("utf-8") or "{}")
    event_id = str(data.get("event_id") or "").strip()
    if not event_id:
        raise ValueError("Matrix file event response missing event_id")
    if summary:
        _matrix_send_file_summary(homeserver, token, encoded_room_id, txn_id, summary, mentions)
    return event_id


def _matrix_event_sender(homeserver: str, token: str, room_id: str, event_id: str) -> str:
    """Best-effort sender lookup for a room event (fail-open).

    Attributes an artifact summary to the person whose message the file is
    attached to. Any lookup failure returns "" so the summary is still
    sent without a mention instead of blocking the publish.
    """
    if not event_id:
        return ""
    encoded_room_id = urllib.parse.quote(_canonical_room_id(room_id), safe="")
    encoded_event_id = urllib.parse.quote(event_id, safe="")
    url = f"{homeserver}/_matrix/client/v3/rooms/{encoded_room_id}/event/{encoded_event_id}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            data = json.loads(response.read().decode("utf-8") or "{}")
    except (urllib.error.URLError, TimeoutError, OSError, ValueError):
        return ""
    sender = str(data.get("sender") or "").strip()
    return sender if MATRIX_MENTION_MXID_RE.fullmatch(sender) else ""


def _default_artifact_summary(filename: str, size: int, source_path: str) -> str:
    return f"📎 已发布 {filename}（{size / 1024:.1f} KB）— {source_path}；结论/下一步见文件"


def _resolve_artifact_mentions(
    arguments: dict[str, Any],
    *,
    room_id: str,
    parent_event_id: str,
    homeserver: str,
    token: str,
    task_id: str = "",
) -> list[str]:
    """Resolve who an artifact summary should mention (fail-open).

    Order: explicit ``mentions`` argument → sender of the parent event
    the file attaches to → the task's requester/leader. Every step may
    come up empty; a publish with no resolvable mention still sends the
    summary text (without a mention) instead of failing.
    """
    explicit = _valid_mention_mxids(
        arguments.get("mentions") or arguments.get("mentionIds") or arguments.get("mention_ids")
    )
    if explicit:
        return explicit
    if parent_event_id:
        sender = _matrix_event_sender(homeserver, token, room_id, parent_event_id)
        if sender:
            return [sender]
    task_id = str(task_id or "").strip()
    if task_id:
        # Lazy import: mcp.lifecycle.taskflow imports this module at top
        # level, so a top-level import back would be a cycle.
        from mcp.lifecycle.taskflow import _load_task

        try:
            task = _load_task(arguments, task_id)
        except ValueError:
            task = {}
        requester = str(task.get("requester") or "").strip()
        if requester and MATRIX_MENTION_MXID_RE.fullmatch(requester):
            return [requester]
        leader = _team_leader_matrix_id()
        if leader:
            return [leader]
    return []


def _publish_workspace_artifact(
    arguments: dict[str, Any],
    *,
    room_id: str,
    source_path: str,
    filename: str,
    expected_prefix: str,
    parent_event_id: str = "",
    summary: str | None = None,
    mentions: list[str] | None = None,
    task_id: str = "",
) -> dict[str, Any]:
    parent_event_id = str(parent_event_id or "").strip()
    result = _artifact_publish_result(str(source_path), filename, parent_event_id)
    try:
        normalized, path = _resolve_workspace_artifact_path(arguments, str(source_path), expected_prefix)
        result["sourcePath"] = normalized
    except ValueError as exc:
        result["status"] = "failed"
        result["error"] = str(exc)
        return result

    if not path.exists():
        result["status"] = "failed"
        result["error"] = "artifact file not found"
        return result
    if path.is_dir():
        result["status"] = "failed"
        result["error"] = "artifact path must be a file"
        return result

    if SENSITIVE_ARTIFACT_NAME_RE.search(result["sourcePath"]) or SENSITIVE_ARTIFACT_NAME_RE.search(filename):
        result["status"] = "failed"
        result["error"] = "artifact appears sensitive and was not published"
        return result

    size = path.stat().st_size
    mimetype = _artifact_mimetype(path)
    result["size"] = size
    result["mimetype"] = mimetype

    try:
        if _artifact_text_has_sensitive_content(path, mimetype):
            result["status"] = "failed"
            result["error"] = "artifact appears sensitive and was not published"
            return result
    except OSError:
        result["status"] = "failed"
        result["error"] = "artifact file could not be read"
        return result

    canonical_room_id = _canonical_room_id(room_id)
    if not canonical_room_id:
        result["status"] = "skipped"
        result["error"] = "Matrix room is unavailable"
        return result
    if not parent_event_id:
        parent_event_id = _matrix_attachment_context_parent_event_id(canonical_room_id)
        result["parentEventId"] = parent_event_id

    try:
        homeserver, token = _matrix_env("artifact publish")
    except ValueError as exc:
        result["status"] = "skipped"
        result["error"] = str(exc)
        return result

    # summary=None → the default publish summary; summary="" → no summary
    # event (the caller's other notification already covers this file).
    summary_text = (
        str(summary).strip()
        if summary is not None
        else _default_artifact_summary(filename, size, normalized)
    )
    if summary_text and mentions is None:
        summary_mentions = _resolve_artifact_mentions(
            arguments,
            room_id=canonical_room_id,
            parent_event_id=parent_event_id,
            homeserver=homeserver,
            token=token,
            task_id=task_id,
        )
    else:
        summary_mentions = mentions

    # Publish identity is fixed before the upload: the txn seed
    # (room + filename + content hash + publish id) excludes the upload
    # response, because real homeservers may mint a fresh MXC URI for
    # every upload of the same bytes. A retry — including one after a
    # lost response — re-PUTs the same txns, so the homeserver keeps a
    # single event pair; a deliberate re-publish of unchanged content
    # passes a fresh publishId to mint a new identity. Without a summary
    # and without a publishId the legacy random txn behavior is kept.
    publish_id = str(arguments.get("publishId") or arguments.get("publish_id") or "").strip()
    stable_txn = ""
    if summary_text or publish_id:
        content_sha = hashlib.sha256(path.read_bytes()).hexdigest()
        stable_txn = _artifact_publish_txn(canonical_room_id, filename, content_sha, publish_id)

    try:
        mxc_uri = _matrix_upload_artifact(homeserver, token, path, filename, mimetype)
        event_id = _matrix_send_file_event(
            homeserver,
            token,
            canonical_room_id,
            filename,
            mxc_uri,
            size,
            mimetype,
            parent_event_id,
            summary=summary_text,
            mentions=summary_mentions,
            txn_id=stable_txn,
        )
    except urllib.error.HTTPError as exc:
        result["status"] = "failed"
        result["error"] = f"Matrix artifact publish failed: HTTP {exc.code}"
        return result
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
        result["status"] = "failed"
        result["error"] = f"Matrix artifact publish failed: {exc}"
        return result

    result["mxcUri"] = mxc_uri
    result["eventId"] = event_id
    result["status"] = "published"
    return result


def _artifact_room_id(arguments: dict[str, Any]) -> str:
    route = _reply_route(arguments)
    target = (
        arguments.get("target")
        or arguments.get("roomId")
        or arguments.get("room_id")
        or route.get("target")
        or route.get("roomId")
        or route.get("room_id")
        or route.get("targetRoom")
        or route.get("target_room")
        or route.get("targetSession")
        or route.get("target_session")
        or ""
    )
    target_kind, target_id = _matrix_target(str(target))
    if target_kind != "room":
        raise ValueError("artifact target must be a Matrix room")
    return target_id


def _artifact(arguments: dict[str, Any]) -> dict[str, Any]:
    action = str(arguments.get("action") or "publish_file").strip()
    if action != "publish_file":
        return {"ok": False, "tool": "artifact", "action": action, "error": f"unsupported action: {action}"}
    source_path = str(
        arguments.get("path")
        or arguments.get("sourcePath")
        or arguments.get("source_path")
        or arguments.get("file")
        or ""
    ).strip()
    if not source_path:
        return {"ok": False, "tool": "artifact", "action": action, "error": "path is required"}
    filename = str(arguments.get("filename") or arguments.get("displayName") or arguments.get("display_name") or "").strip()
    if not filename:
        filename = Path(source_path.rstrip("/")).name or "artifact"
    try:
        room_id = _artifact_room_id(arguments)
    except ValueError as exc:
        return {"ok": False, "tool": "artifact", "action": action, "error": str(exc)}
    parent_event_id = (
        _attachment_parent_event_id(arguments)
        or _matrix_attachment_context_parent_event_id(room_id)
    )
    artifact = _publish_workspace_artifact(
        arguments,
        room_id=room_id,
        source_path=source_path,
        filename=filename,
        expected_prefix="",
        parent_event_id=parent_event_id,
        task_id=str(arguments.get("taskId") or arguments.get("task_id") or "").strip(),
    )
    return {
        "ok": artifact.get("status") == "published",
        "tool": "artifact",
        "action": action,
        "artifact": artifact,
        "error": artifact.get("error") or "",
    }


def _task_artifact_filename(task_id: str, source_path: str, result_artifact: bool = False) -> str:
    if result_artifact:
        suffix = Path(source_path).suffix or ".md"
        return f"{task_id}-result{suffix}"
    name = Path(str(source_path).rstrip("/")).name or "artifact"
    return f"{task_id}-{name}"


def _publish_task_artifacts(
    arguments: dict[str, Any],
    task: dict[str, Any],
    task_id: str,
    deliverables: list[Any],
    parent_event_id: str = "",
) -> list[dict[str, Any]]:
    room_id = str(task.get("room_id") or "")
    expected_prefix = f"shared/tasks/{task_id}"
    result_source = f"{expected_prefix}/result.md"
    # (source, filename, is_result) — the result.md summary is withheld
    # because the submit_task action sends the completion notification for
    # it in the same action (its Result line names the file and mentions
    # the leader); a second @-summary would double-notify the room.
    artifacts: list[tuple[str, str, bool]] = []
    try:
        _normalized_result, result_path = _resolve_workspace_artifact_path(arguments, result_source, expected_prefix)
        if result_path.is_file():
            artifacts.append((result_source, _task_artifact_filename(task_id, "result.md", result_artifact=True), True))
    except ValueError:
        pass
    seen = {source for source, _filename, _is_result in artifacts}
    for item in deliverables:
        source = str(item or "").strip()
        if not source:
            continue
        try:
            normalized, is_directory = _normalize_shared_path(source, "stat")
            seen_key = normalized.rstrip("/") + ("/" if is_directory else "")
        except ValueError:
            seen_key = source
        if seen_key in seen:
            continue
        seen.add(seen_key)
        artifacts.append((source, _task_artifact_filename(task_id, source), False))
    return [
        _publish_workspace_artifact(
            arguments,
            room_id=room_id,
            source_path=source,
            filename=filename,
            expected_prefix=expected_prefix,
            parent_event_id=parent_event_id,
            summary="" if is_result else None,
            task_id=task_id,
        )
        for source, filename, is_result in artifacts
    ]


refs.define("_publish_task_artifacts", _publish_task_artifacts)


def _project_artifact_room(project: dict[str, Any], task: dict[str, Any]) -> str:
    reply_route = project.get("reply_route") if isinstance(project.get("reply_route"), dict) else {}
    if str(reply_route.get("channel") or "").strip().lower() == "matrix":
        target_session = str(reply_route.get("target_session") or "").strip()
        if target_session:
            return target_session
    room_id = str(task.get("room_id") or "").strip()
    if room_id:
        return room_id
    source_room_id = str(project.get("source_room_id") or "").strip()
    if MATRIX_ROOM_RE.fullmatch(_canonical_room_id(source_room_id)):
        return source_room_id
    return ""


def _publish_project_artifacts(
    arguments: dict[str, Any],
    project: dict[str, Any],
    project_id: str,
    task_id: str,
    parent_event_id: str = "",
    completion_event_id: str = "",
) -> list[dict[str, Any]]:
    task = _read_json(_task_state_path(arguments, task_id)) if task_id else {}
    source_path = f"shared/projects/{project_id}/result.md"
    return [
        _publish_workspace_artifact(
            arguments,
            room_id=_project_artifact_room(project, task),
            source_path=source_path,
            filename=f"{project_id}-project-result.md",
            expected_prefix=f"shared/projects/{project_id}",
            parent_event_id=parent_event_id,
            # A completion notification already emitted for this action
            # (its event id is recorded) names the result file and
            # mentions the leader; a second @-summary would double-notify.
            summary="" if completion_event_id else None,
            task_id=task_id,
        )
    ]


refs.define("_publish_project_artifacts", _publish_project_artifacts)


