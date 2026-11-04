# filesync storage sync (W1-a2: real module)
from __future__ import annotations

import datetime

import json

import os

import re

import subprocess

from pathlib import Path

from typing import Any

import urllib.parse

# Cross-group imports are top-level absolutes (see lifecycle/transitions.py
# note): mcp/ on sys.path keeps one module identity in both load modes.
from mcp.common import refs
from mcp.common.paths import (
    _default_global_shared_prefix,
    _default_shared_prefix,
    _workspace_dir,
)

MC_ALIAS = "agentteams"


def _remote_root(value: str) -> str:
    text = (value or "").strip()
    if not text:
        raise ValueError("storage sharedPrefix is required")
    return text.rstrip("/") + "/"


def _mc_host_url(endpoint: str, access_key: str, secret_key: str) -> str:
    url = endpoint.strip().rstrip("/")
    if not url.startswith(("http://", "https://")):
        url = f"http://{url}"
    parsed = urllib.parse.urlsplit(url)
    if "@" in parsed.netloc:
        return url
    user = urllib.parse.quote(access_key, safe="")
    password = urllib.parse.quote(secret_key, safe="")
    netloc = f"{user}:{password}@{parsed.netloc}"
    return urllib.parse.urlunsplit(
        (parsed.scheme, netloc, parsed.path, parsed.query, parsed.fragment)
    )


def _remote_uses_mc_alias(remote: str) -> bool:
    return remote.strip().startswith(f"{MC_ALIAS}/")


def _mc_alias_configured(env: dict[str, str]) -> bool:
    try:
        completed = subprocess.run(
            ["mc", "alias", "list", MC_ALIAS],
            check=False,
            capture_output=True,
            text=True,
            timeout=20,
            env=env,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    output = f"{completed.stdout}\n{completed.stderr}"
    return completed.returncode == 0 and f"{MC_ALIAS}" in output


def _filesync_mc_env(remote: str) -> tuple[dict[str, str], str | None]:
    env = dict(os.environ)
    if not _remote_uses_mc_alias(remote):
        return env, None

    alias_env = f"MC_HOST_{MC_ALIAS}"
    if env.get(alias_env):
        return env, None

    endpoint = env.get("AGENTTEAMS_FS_ENDPOINT", "").strip()
    access_key = env.get("AGENTTEAMS_FS_ACCESS_KEY", "").strip()
    secret_key = env.get("AGENTTEAMS_FS_SECRET_KEY", "").strip()
    if endpoint and access_key and secret_key:
        env[alias_env] = _mc_host_url(endpoint, access_key, secret_key)
        return env, None

    if _mc_alias_configured(env):
        return env, None
    return env, (
        f"storage alias {MC_ALIAS} is not configured; missing "
        "AGENTTEAMS_FS_ENDPOINT/AGENTTEAMS_FS_ACCESS_KEY/AGENTTEAMS_FS_SECRET_KEY"
    )


def _normalize_exclude(value: Any) -> list[str]:
    if not value:
        return []
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return []
        if text.startswith("["):
            parsed = json.loads(text)
            if not isinstance(parsed, list):
                raise ValueError("exclude must be a list")
            return [str(item) for item in parsed if str(item).strip()]
        return [text]
    if isinstance(value, list):
        return [str(item) for item in value if str(item).strip()]
    raise ValueError("exclude must be a list")


def _normalize_shared_path(raw_path: str, action: str) -> tuple[str, bool]:
    raw = (raw_path or "").strip()
    if not raw or raw.startswith("/") or "\\" in raw:
        raise ValueError("path must be a relative shared path")
    parts = raw.strip("/").split("/")
    if any(part in {"", ".", ".."} for part in parts):
        raise ValueError("path must be a relative shared path without '.', '..', or empty segments")
    if parts[0] not in {"shared", "global-shared"}:
        raise ValueError("path must start with shared/ or global-shared/")
    if parts[0] == "shared" and action in {"push", "pull"} and len(parts) < 3:
        raise ValueError("shared push/pull requires a subpath under shared/")
    if parts[0] == "global-shared" and len(parts) < 2:
        raise ValueError("global-shared path must include a subpath")
    is_directory = raw.endswith("/") or (
        action in {"pull", "push", "list"}
        and len(parts) <= 3
        and parts[0] in {"shared", "global-shared"}
    )
    normalized = "/".join(parts)
    if is_directory:
        normalized += "/"
    return normalized, is_directory


def _resolve_filesync(arguments: dict[str, Any]) -> tuple[str, str, Path, str, bool]:
    action = str(arguments.get("action") or "").strip()
    if action not in {"pull", "push", "list", "stat", "cat", "etag"}:
        raise ValueError("action is required; use pull, push, stat, cat, list, or etag")
    normalized, is_directory = _normalize_shared_path(str(arguments.get("path") or ""), action)
    parts = normalized.strip("/").split("/")
    kind = parts[0]
    if kind == "global-shared" and action == "push":
        raise ValueError("global-shared is read-only for TeamHarness filesync")

    storage = arguments.get("storage") if isinstance(arguments.get("storage"), dict) else {}
    shared_prefix = str(storage.get("sharedPrefix") or "").strip() or _default_shared_prefix()
    shared_root = _remote_root(shared_prefix)
    global_root = ""
    if kind == "global-shared":
        global_shared_prefix = str(storage.get("globalSharedPrefix") or "").strip() or _default_global_shared_prefix()
        global_root = _remote_root(global_shared_prefix)
    workspace = _workspace_dir(arguments)
    local = workspace / Path(*parts)
    remote_root = shared_root if kind == "shared" else global_root
    remote = remote_root + "/".join(parts[1:])
    if is_directory:
        remote = remote.rstrip("/") + "/"
    return action, normalized, local, remote, is_directory


_PUSH_CLOCK_TOLERANCE = datetime.timedelta(seconds=5)


def _parse_remote_mtime(value: Any) -> datetime.datetime | None:
    """Parse a remote last-modified timestamp into an aware UTC datetime.

    Accepts RFC3339 (``Z`` or numeric offset, fractional seconds of any
    precision) and the ``mc stat`` text form ``YYYY-MM-DD HH:MM:SS UTC``.
    Returns None when unparseable.
    """
    text = str(value or "").strip()
    if not text:
        return None
    if text.endswith(("UTC", "GMT")):
        text = text[:-3].strip()
    if text.endswith(("Z", "z")):
        text = text[:-1] + "+00:00"
    # Cap fractional seconds at microseconds for pre-3.11 fromisoformat.
    text = re.sub(r"(\.\d{6})\d+", r"\1", text)
    try:
        parsed = datetime.datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=datetime.timezone.utc)
    return parsed.astimezone(datetime.timezone.utc)


def _filesync_remote_mtime(
    remote: str, mc_env: dict[str, str]
) -> tuple[datetime.datetime | None, str | None, str | None]:
    """Probe the remote object before a single-file push.

    Best-effort freshness guard (2026-09-21 incident: a stale local view
    clobbered newer MinIO state).  The comparison is against the local
    file's *last edit time*, not the remote version that file was derived
    from, so this is scoped as a check for unedited (synced) local copies.

    Known limitations (documented, not enforced here):

    - **Stale-read/edit/push**: if a local copy is pulled (v1), a newer
      remote version is published (v2), and the local copy is then edited
      *after* that remote write, the local mtime is newer and the guard
      permits the overwrite.  A version-baseline or conditional-put check
      is required to close that case; tracked as a follow-up.
    - **Probe failure is fail-open by design**: the guard is a freshness
      hint, not a write lock.  When the remote cannot be stat'ed the push
      proceeds and the reason is surfaced via ``warning``; blocking on an
      unavailable probe would make filesync unusable during transient
      MinIO outages, while the guarded incident class (known-stale local
      view) is unaffected by probe failures.
    - **Not atomic**: stat and cp are separate steps (TOCTOU); this is not
      a concurrent-write lock.

    Returns (mtime, raw, warning):

    - remote absent          -> (None, None, None): a normal first push
    - probe/parse failed     -> (None, None, reason): push through, surface reason
    - remote timestamp known -> (mtime, raw, None): caller compares vs local
    """
    try:
        stat = subprocess.run(
            ["mc", "stat", "--json", remote],
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
            env=mc_env,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return None, None, f"remote stat probe failed: {exc}"

    if stat.returncode != 0:
        detail = "\n".join(part.strip() for part in (stat.stderr, stat.stdout) if part.strip())
        if "does not exist" in detail.lower():
            return None, None, None
        return None, None, f"remote stat probe failed: {detail or 'unknown error'}"

    raw: str | None = None
    mtime: datetime.datetime | None = None
    try:
        payload = json.loads(stat.stdout)
    except ValueError:
        payload = None
    if isinstance(payload, dict):
        candidate = str(payload.get("lastModified") or "").strip()
        if candidate:
            raw = candidate
            mtime = _parse_remote_mtime(candidate)
    if mtime is None:
        # --json output lacked a usable lastModified; degrade to the text
        # `mc stat` Date: line before giving up on the guard.
        try:
            text_stat = subprocess.run(
                ["mc", "stat", remote],
                check=False,
                capture_output=True,
                text=True,
                timeout=30,
                env=mc_env,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            return None, None, f"remote stat probe failed: {exc}"
        if text_stat.returncode != 0:
            detail = "\n".join(part.strip() for part in (text_stat.stderr, text_stat.stdout) if part.strip())
            if "does not exist" in detail.lower():
                return None, None, None
            return None, None, f"remote stat probe failed: {detail or 'unknown error'}"
        for line in text_stat.stdout.splitlines():
            date_match = re.match(r"^date\s*:\s*(.+)$", line.strip(), re.IGNORECASE)
            if date_match:
                raw = date_match.group(1).strip()
                mtime = _parse_remote_mtime(raw)
                break
        if mtime is None:
            return None, None, (
                "could not determine remote lastModified; push passed through "
                "without the freshness guard"
            )
    return mtime, raw, None


def _filesync(arguments: dict[str, Any]) -> dict[str, Any]:
    try:
        action, normalized, local, remote, is_directory = _resolve_filesync(arguments)
        exclude = _normalize_exclude(arguments.get("exclude"))
    except (ValueError, json.JSONDecodeError) as exc:
        return {"ok": False, "tool": "filesync", "error": str(exc)}

    kind = normalized.split("/", 1)[0]
    command: list[str]
    if action == "list":
        command = ["mc", "ls", "--recursive", remote]
    elif action == "stat":
        command = ["mc", "stat", remote]
    elif action == "cat":
        # Read one remote object without touching local state (#1402:
        # _sync_project snapshots the project meta this way before its
        # guarded single-file commit).  Read-only: no freshness guard.
        command = ["mc", "cat", remote]
    elif action == "etag":
        # The object's ETag for conditional commits (If-Match); same
        # probe the push guard uses, read-only.
        command = ["mc", "stat", "--json", remote]
    elif action == "pull":
        if is_directory:
            command = ["mc", "mirror", remote, str(local), "--overwrite"]
        else:
            command = ["mc", "cp", remote, str(local)]
    else:
        if is_directory:
            # TODO(B13): consider a per-file guard for directory pushes
            source = str(local) + ("/" if not str(local).endswith("/") else "")
            command = ["mc", "mirror", source, remote, "--overwrite"]
            for pattern in exclude:
                command.extend(["--exclude", pattern])
        else:
            command = ["mc", "cp", str(local), remote]

    base: dict[str, Any] = {
        "ok": True,
        "tool": "filesync",
        "action": action,
        "kind": kind,
        "path": normalized,
        "localPath": str(local),
        "remotePath": remote,
        "command": command,
        "exclude": exclude,
    }
    if arguments.get("dryRun"):
        base["dryRun"] = True
        return base

    if action == "pull" and not is_directory:
        local.parent.mkdir(parents=True, exist_ok=True)
    mc_env, env_error = _filesync_mc_env(remote)
    if env_error:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": env_error,
        }
    guard_warning: str | None = None
    if action == "push" and not is_directory:
        remote_mtime, remote_raw, guard_warning = _filesync_remote_mtime(remote, mc_env)
        if remote_mtime is not None:
            try:
                local_mtime = datetime.datetime.fromtimestamp(
                    local.stat().st_mtime, tz=datetime.timezone.utc
                )
            except OSError:
                local_mtime = None
            if (
                local_mtime is not None
                and remote_mtime > local_mtime + _PUSH_CLOCK_TOLERANCE
            ):
                return {
                    "ok": False,
                    "conflict": True,
                    "tool": "filesync",
                    "action": "push",
                    "path": normalized,
                    "remotePath": remote,
                    "localMtime": local_mtime.isoformat(),
                    "remoteLastModified": remote_raw,
                    "error": "remote copy is newer than the local file; pull before pushing again",
                }
    try:
        completed = subprocess.run(
            command,
            check=False,
            capture_output=True,
            text=True,
            timeout=120,
            env=mc_env,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        body: dict[str, Any] = {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": f"filesync process failed: {exc}",
            "retryable": True,
        }
        if guard_warning is not None:
            body["warning"] = guard_warning
        return body
    command_error = _filesync_command_error(completed)
    if command_error:
        body = {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": command_error,
            "returncode": completed.returncode,
        }
        if guard_warning is not None:
            body["warning"] = guard_warning
        return body
    if action == "etag":
        etag = _filesync_stat_etag(completed.stdout)
        if not etag:
            return {
                "ok": False,
                "tool": "filesync",
                "action": action,
                "path": normalized,
                "remotePath": remote,
                "error": "the remote stat response carried no etag",
            }
        base["etag"] = etag
        return base
    if action == "list":
        base["entries"] = [line for line in completed.stdout.splitlines() if line.strip()]
    if action == "stat":
        base["exists"] = True
    if action == "cat":
        base["content"] = completed.stdout
    if guard_warning is not None:
        base["warning"] = guard_warning
    return base


refs.define("_filesync", _filesync)


def _filesync_stat_etag(stdout: str) -> str | None:
    """The etag field of an ``mc stat --json`` stdout, or None when the
    response is not a JSON object carrying a usable etag."""
    try:
        payload = json.loads(stdout)
    except (TypeError, ValueError):
        return None
    etag = payload.get("etag") if isinstance(payload, dict) else None
    if isinstance(etag, str) and etag.strip():
        return etag.strip()
    return None


def _filesync_command_error(completed: subprocess.CompletedProcess[str]) -> str:
    output = "\n".join(part.strip() for part in (completed.stderr, completed.stdout) if part.strip())
    if completed.returncode != 0:
        return output or "filesync command failed"
    if "<ERROR>" in output or "Access Denied" in output:
        return output
    return ""


