"""TeamHarness MCP filesync tool — delegates to agentteams_sync.FileSync."""

from __future__ import annotations

import json
import os
import subprocess
import urllib.parse
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

import mcp_common as common

from agentteams_sync import mc as mc_ops
from agentteams_sync.filesync import FileSync
from agentteams_sync.mc import bind_active_mc

MC_ALIAS = "agentteams"


@contextmanager
def _filesync_runtime_env(mc_env: dict[str, str]) -> Iterator[None]:
    previous = {key: os.environ.get(key) for key in mc_env}
    os.environ.update(mc_env)
    try:
        yield
    finally:
        for key, value in previous.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def _filesync(arguments: dict[str, Any]) -> dict[str, Any]:
    try:
        action, normalized, local, remote, is_directory = _resolve_filesync(arguments)
        exclude = _normalize_exclude(arguments.get("exclude"))
    except (ValueError, json.JSONDecodeError) as exc:
        return {"ok": False, "tool": "filesync", "error": str(exc)}

    kind = normalized.split("/", 1)[0]
    command = _filesync_command(action, local, remote, is_directory, exclude)
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

    mc_env, env_error = _filesync_mc_env(remote)
    if env_error:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": env_error,
        }

    try:
        sync = _create_mcp_sync(arguments, mc_env=mc_env)
        with _filesync_runtime_env(mc_env), _mcp_mc_output_guard():
            if action == "pull":
                sync.pull_shared_path(normalized)
            elif action == "push":
                sync.push_shared_path(normalized, exclude=exclude or None)
            elif action == "stat":
                sync.stat_shared_path(normalized)
            else:
                _, entries = sync.list_shared_path(normalized)
                base["entries"] = entries
                return base
    except subprocess.CalledProcessError as exc:
        output = "\n".join(
            part.strip() for part in (exc.stderr, exc.stdout) if part and str(part).strip()
        )
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": output or "filesync command failed",
            "returncode": exc.returncode,
        }
    except FileNotFoundError as exc:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": str(exc),
        }
    except ValueError as exc:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": str(exc),
        }
    except RuntimeError as exc:
        return {
            "ok": False,
            "tool": "filesync",
            "action": action,
            "path": normalized,
            "error": str(exc),
        }

    if action == "stat":
        base["exists"] = True
    return base


def _create_mcp_sync(arguments: dict[str, Any], *, mc_env: dict[str, str] | None = None) -> FileSync:
    workspace = common._workspace_dir(arguments)
    storage = arguments.get("storage") if isinstance(arguments.get("storage"), dict) else {}
    shared_prefix = str(storage.get("sharedPrefix") or "").strip() or common._default_shared_prefix()
    global_shared_prefix = (
        str(storage.get("globalSharedPrefix") or "").strip() or common._default_global_shared_prefix()
    )

    env = mc_env if mc_env is not None else dict(os.environ)
    endpoint = env.get("AGENTTEAMS_FS_ENDPOINT", "").strip()
    access_key = env.get("AGENTTEAMS_FS_ACCESS_KEY", "").strip()
    secret_key = env.get("AGENTTEAMS_FS_SECRET_KEY", "").strip()
    bucket = env.get("AGENTTEAMS_FS_BUCKET", "agentteams-storage").strip() or "agentteams-storage"
    worker_name = (
        env.get("AGENTTEAMS_WORKER_NAME", "").strip()
        or env.get("AGENTTEAMS_AGENT_NAME", "").strip()
        or "teamharness-mcp"
    )

    if not endpoint or not access_key or not secret_key:
        if _remote_uses_mc_alias(common._remote_root(shared_prefix)) or _remote_uses_mc_alias(
            common._remote_root(global_shared_prefix)
        ):
            raise ValueError(
                f"storage alias {MC_ALIAS} is not configured; missing "
                "AGENTTEAMS_FS_ENDPOINT/AGENTTEAMS_FS_ACCESS_KEY/AGENTTEAMS_FS_SECRET_KEY"
            )

    return FileSync(
        endpoint=endpoint or "http://127.0.0.1",
        access_key=access_key or "unused",
        secret_key=secret_key or "unused",
        bucket=bucket,
        worker_name=worker_name,
        secure=str(endpoint).startswith("https://"),
        local_dir=workspace,
        shared_dir=workspace / "shared",
        global_shared_dir=workspace / "global-shared",
        team_resolver="agents_md",
        shared_remote_root=common._remote_root(shared_prefix),
        global_shared_remote_root=common._remote_root(global_shared_prefix),
    )


@contextmanager
def _mcp_mc_output_guard() -> Iterator[None]:
    original_mc = mc_ops.mc

    def guarded_mc(*args: str, **kwargs: Any) -> subprocess.CompletedProcess[str]:
        result = original_mc(*args, **kwargs)
        command_error = _filesync_command_error(result)
        if command_error:
            raise subprocess.CalledProcessError(
                result.returncode,
                [original_mc.__name__, *args],
                output=result.stdout,
                stderr=result.stderr,
            )
        return result

    bind_active_mc(guarded_mc)
    try:
        yield
    finally:
        bind_active_mc(original_mc)


def _filesync_command(
    action: str,
    local: Path,
    remote: str,
    is_directory: bool,
    exclude: list[str],
) -> list[str]:
    if action == "list":
        return ["mc", "ls", "--recursive", remote]
    if action == "stat":
        return ["mc", "stat", remote]
    if action == "pull":
        if is_directory:
            return ["mc", "mirror", remote, str(local), "--overwrite"]
        return ["mc", "cp", remote, str(local)]
    if is_directory:
        source = str(local) + ("/" if not str(local).endswith("/") else "")
        command = ["mc", "mirror", source, remote, "--overwrite"]
        for pattern in exclude:
            command.extend(["--exclude", pattern])
        return command
    return ["mc", "cp", str(local), remote]


def _filesync_command_error(completed: subprocess.CompletedProcess[str]) -> str:
    output = "\n".join(part.strip() for part in (completed.stderr, completed.stdout) if part.strip())
    if completed.returncode != 0:
        return output or "filesync command failed"
    if "<ERROR>" in output or "Access Denied" in output:
        return output
    return ""


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


def _remote_uses_mc_alias(remote: str) -> bool:
    return remote.strip().startswith(f"{MC_ALIAS}/")


def _resolve_filesync(arguments: dict[str, Any]) -> tuple[str, str, Path, str, bool]:
    action = str(arguments.get("action") or "").strip()
    if action not in {"pull", "push", "list", "stat"}:
        raise ValueError("action is required; use pull, push, stat, or list")
    normalized, is_directory = _normalize_shared_path(str(arguments.get("path") or ""), action)
    parts = normalized.strip("/").split("/")
    kind = parts[0]
    if kind == "global-shared" and action == "push":
        raise ValueError("global-shared is read-only for TeamHarness filesync")

    storage = arguments.get("storage") if isinstance(arguments.get("storage"), dict) else {}
    shared_prefix = str(storage.get("sharedPrefix") or "").strip() or common._default_shared_prefix()
    shared_root = common._remote_root(shared_prefix)
    global_root = ""
    if kind == "global-shared":
        global_shared_prefix = (
            str(storage.get("globalSharedPrefix") or "").strip() or common._default_global_shared_prefix()
        )
        global_root = common._remote_root(global_shared_prefix)
    workspace = common._workspace_dir(arguments)
    local = workspace / Path(*parts)
    remote_root = shared_root if kind == "shared" else global_root
    remote = remote_root + "/".join(parts[1:])
    if is_directory:
        remote = remote.rstrip("/") + "/"
    return action, normalized, local, remote, is_directory


def filesync(arguments: dict[str, Any]) -> dict[str, Any]:
    """MCP filesync entry; shared FileSync with TeamHarness storage prefix semantics."""
    return _filesync(arguments)
