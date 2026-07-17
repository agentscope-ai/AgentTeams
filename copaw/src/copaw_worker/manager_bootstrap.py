"""CoPaw Manager runtime bootstrap (bridge, layout, Matrix DM rooms, CMS)."""

from __future__ import annotations

import json
import logging
import os
import shutil
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

from copaw_worker.bridge import _is_in_container, _port_remap
from copaw_worker.workspace_layout import WorkspaceLayout

logger = logging.getLogger(__name__)

_DEFAULT_MATRIX_API = "http://127.0.0.1:6167"
_WATCH_INTERVAL_SEC = 60
_DM_REFRESH_DELAYS_SEC = (15.0, 30.0, 60.0)


def resolve_matrix_api_url(
    agent_data: dict[str, Any] | None = None,
    *,
    copaw_working_dir: Path | None = None,
) -> str:
    """Resolve Matrix Client-Server API base URL for bootstrap HTTP calls."""
    url = os.environ.get("AGENTTEAMS_MATRIX_URL", "").strip()
    if not url and agent_data:
        matrix = agent_data.get("channels", {}).get("matrix", {})
        url = str(matrix.get("homeserver") or "").strip()
    if not url and copaw_working_dir is not None:
        config_path = copaw_working_dir / "config.json"
        if config_path.is_file():
            try:
                cfg = json.loads(config_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                cfg = {}
            url = str(
                cfg.get("channels", {}).get("matrix", {}).get("homeserver") or ""
            ).strip()
    if not url:
        url = _DEFAULT_MATRIX_API
    return _port_remap(url, _is_in_container())


def _resolve_matrix_access_token(agent_data: dict[str, Any]) -> str:
    matrix = agent_data.get("channels", {}).get("matrix", {})
    token = matrix.get("access_token") or matrix.get("accessToken") or ""
    if token and token != "null":
        return str(token)
    env_token = os.environ.get("AGENTTEAMS_MANAGER_MATRIX_TOKEN", "").strip()
    if env_token and env_token != "null":
        return env_token
    return ""


def maybe_archive_legacy_manager_config(copaw_working_dir: Path) -> None:
    """Archive legacy config.json when tool_guard was enabled (one-shot migration)."""
    config_path = copaw_working_dir / "config.json"
    marker = copaw_working_dir / ".config-migrated-v2"
    if not config_path.is_file() or marker.is_file():
        return

    try:
        data = json.loads(config_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return

    tool_guard = (
        data.get("security", {})
        .get("tool_guard", {})
        .get("enabled", False)
    )
    if not tool_guard:
        return

    stamp = time.strftime("%Y%m%d-%H%M%S")
    archive = copaw_working_dir / f"config.json.legacy-{stamp}"
    config_path.rename(archive)
    marker.touch()
    logger.info(
        "Archived legacy config.json (tool_guard enabled) -> %s",
        archive.name,
    )


def load_openclaw_json(path: Path) -> dict[str, Any]:
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def propagate_manager_extras(standard_dir: Path, copaw_working_dir: Path) -> None:
    """Sync manager-only prompt paths (USER/MEMORY/memory) into workspaces/default."""
    workspace_dir = copaw_working_dir / "workspaces" / "default"
    workspace_dir.mkdir(parents=True, exist_ok=True)

    user_md = standard_dir / "USER.md"
    if user_md.is_file():
        _copy_if_source_newer(user_md, workspace_dir / "PROFILE.md")

    memory_md = standard_dir / "MEMORY.md"
    if memory_md.is_file():
        _copy_if_source_newer(memory_md, workspace_dir / "MEMORY.md")

    memory_src = standard_dir / "memory"
    if memory_src.is_dir():
        memory_dst = workspace_dir / "memory"
        memory_dst.mkdir(parents=True, exist_ok=True)
        _copy_tree_recursive_update(memory_src, memory_dst)


def _copy_if_source_newer(src: Path, dst: Path) -> None:
    if not src.is_file():
        return
    if dst.exists() and dst.stat().st_mtime >= src.stat().st_mtime:
        return
    shutil.copy2(src, dst)
    logger.debug("Synced %s -> %s", src.name, dst)


def _copy_tree_recursive_update(src_dir: Path, dst_dir: Path) -> None:
    for item in src_dir.rglob("*"):
        rel = item.relative_to(src_dir)
        target = dst_dir / rel
        if item.is_dir():
            target.mkdir(parents=True, exist_ok=True)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        if target.exists() and target.stat().st_mtime >= item.stat().st_mtime:
            continue
        shutil.copy2(item, target)


def materialize_manager_workspace(
    standard_dir: Path,
    copaw_working_dir: Path,
    openclaw_cfg: dict[str, Any],
) -> WorkspaceLayout:
    """Bridge + propagate using WorkspaceLayout (skills symlink, not active_skills)."""
    layout = WorkspaceLayout(standard_dir, copaw_working_dir, profile="manager")
    layout.materialize(openclaw_cfg, bootstrap=True)
    propagate_manager_extras(standard_dir, copaw_working_dir)
    return layout


def configure_manager_dm_rooms(
    copaw_working_dir: Path,
    *,
    matrix_api: str | None = None,
    max_retries: int = 5,
    retry_delay_sec: float = 3.0,
) -> None:
    """Mark 2-member Matrix rooms as DM auto-reply in agent.json."""
    agent_json_path = copaw_working_dir / "workspaces" / "default" / "agent.json"
    if not agent_json_path.is_file():
        raise FileNotFoundError(f"agent.json not found at {agent_json_path}")

    agent_data = json.loads(agent_json_path.read_text(encoding="utf-8"))
    access_token = _resolve_matrix_access_token(agent_data)
    if not access_token:
        logger.warning(
            "No Matrix access token in agent.json or env; skipping DM room detection"
        )
        return

    resolved_api = matrix_api or resolve_matrix_api_url(
        agent_data, copaw_working_dir=copaw_working_dir
    )
    logger.info("Configuring Manager DM rooms via Matrix API at %s", resolved_api)

    dm_rooms: dict[str, dict[str, bool]] = {}
    joined_rooms: list[str] = []
    for attempt in range(max_retries):
        joined_rooms = _fetch_joined_rooms(resolved_api, access_token)
        if joined_rooms:
            break
        if attempt + 1 < max_retries:
            logger.info(
                "Retrying DM room detection (%s/%s)...",
                attempt + 1,
                max_retries,
            )
            time.sleep(retry_delay_sec)

    if not joined_rooms:
        logger.warning(
            "Could not fetch joined rooms after %s retries (Tuwunel may not be ready)",
            max_retries,
        )
    else:
        for room_id in joined_rooms:
            member_count = _fetch_member_count(resolved_api, access_token, room_id)
            if member_count == 2:
                dm_rooms[room_id] = {"requireMention": False, "autoReply": True}
                logger.info(
                    "DM room: %s (%s members, autoReply)",
                    room_id,
                    member_count,
                )

    groups = agent_data.setdefault("channels", {}).setdefault("matrix", {}).setdefault(
        "groups", {}
    )
    groups.update(dm_rooms)
    agent_json_path.write_text(
        json.dumps(agent_data, indent=2, ensure_ascii=False),
        encoding="utf-8",
    )


def _fetch_joined_rooms(matrix_api: str, access_token: str) -> list[str]:
    url = f"{matrix_api.rstrip('/')}/_matrix/client/v3/joined_rooms"
    try:
        req = urllib.request.Request(
            url,
            headers={"Authorization": f"Bearer {access_token}"},
            method="GET",
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read())
        return list(payload.get("joined_rooms") or [])
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as exc:
        logger.debug("joined_rooms fetch failed: %s", exc)
        return []


def _fetch_member_count(matrix_api: str, access_token: str, room_id: str) -> int:
    encoded = urllib.parse.quote(room_id, safe="")
    url = (
        f"{matrix_api.rstrip('/')}/_matrix/client/v3/rooms/{encoded}/members"
        "?membership=join"
    )
    try:
        req = urllib.request.Request(
            url,
            headers={"Authorization": f"Bearer {access_token}"},
            method="GET",
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read())
        return sum(
            1
            for event in payload.get("chunk") or []
            if (event.get("content") or {}).get("membership") == "join"
        )
    except (urllib.error.URLError, OSError, json.JSONDecodeError):
        return 0


def start_dm_room_refresh_watcher(
    copaw_working_dir: Path,
    *,
    delays_sec: tuple[float, ...] = _DM_REFRESH_DELAYS_SEC,
) -> threading.Thread:
    """Re-run DM room detection after Matrix auto-join completes."""

    def _refresh() -> None:
        for delay in delays_sec:
            time.sleep(delay)
            try:
                configure_manager_dm_rooms(copaw_working_dir)
            except Exception as exc:
                logger.debug("Deferred DM room config failed: %s", exc)

    thread = threading.Thread(
        target=_refresh,
        name="dm-room-refresh",
        daemon=True,
    )
    thread.start()
    logger.info(
        "DM room refresh watcher started (delays: %s)",
        ", ".join(str(int(d)) for d in delays_sec),
    )
    return thread


def configure_cms_plugin(home_dir: Path) -> None:
    """Write LoongSuite bootstrap config when CMS tracing is enabled."""
    enabled = os.environ.get("AGENTTEAMS_CMS_TRACES_ENABLED", "false").lower()
    if enabled != "true":
        return

    bootstrap_dir = home_dir / ".loongsuite"
    bootstrap_dir.mkdir(parents=True, exist_ok=True)
    cfg_path = bootstrap_dir / "bootstrap-config.json"
    endpoint = os.environ.get("AGENTTEAMS_CMS_ENDPOINT", "")
    license_key = os.environ.get("AGENTTEAMS_CMS_LICENSE_KEY", "")
    arms_project = os.environ.get("AGENTTEAMS_CMS_PROJECT", "")
    cms_workspace = os.environ.get("AGENTTEAMS_CMS_WORKSPACE", "")
    service_name = os.environ.get("AGENTTEAMS_CMS_SERVICE_NAME", "agentteams-manager")

    config = {
        "OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
        "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
        "OTEL_EXPORTER_OTLP_HEADERS": (
            f"x-arms-license-key={license_key},"
            f"x-arms-project={arms_project},"
            f"x-cms-workspace={cms_workspace}"
        ),
        "OTEL_SERVICE_NAME": service_name,
        "OTEL_SEMCONV_STABILITY_OPT_IN": "http,gen_ai_latest_experimental",
        "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "SPAN_AND_EVENT",
        "LOONGSUITE_PYTHON_SITE_BOOTSTRAP": "true",
    }
    cfg_path.write_text(json.dumps(config, indent=2), encoding="utf-8")
    logger.info("CoPaw CMS plugin configured at %s", cfg_path)


def start_openclaw_json_watcher(
    openclaw_json: Path,
    copaw_working_dir: Path,
    standard_dir: Path,
) -> threading.Thread:
    """Background thread: re-bridge when openclaw.json changes."""

    def _watch() -> None:
        prev_hash = _file_md5(openclaw_json)
        layout = WorkspaceLayout(standard_dir, copaw_working_dir, profile="manager")
        while True:
            time.sleep(_WATCH_INTERVAL_SEC)
            curr_hash = _file_md5(openclaw_json)
            if not curr_hash or curr_hash == prev_hash:
                continue
            try:
                openclaw_cfg = load_openclaw_json(openclaw_json)
                layout.rebridge(openclaw_cfg)
                propagate_manager_extras(standard_dir, copaw_working_dir)
                prev_hash = curr_hash
                logger.info("Re-bridge complete (openclaw.json changed)")
            except Exception as exc:
                logger.warning(
                    "Re-bridge failed, will retry on next cycle: %s",
                    exc,
                )

    thread = threading.Thread(target=_watch, name="openclaw-json-watcher", daemon=True)
    thread.start()
    logger.info("openclaw.json watcher started")
    return thread


def _file_md5(path: Path) -> str:
    import hashlib

    if not path.is_file():
        return ""
    digest = hashlib.md5()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()
