"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import json
import logging
import os
import re
import time
from pathlib import Path
from typing import Any, Dict, Iterable, List
from urllib.parse import urlparse

from qwenpaw_worker.update.constants import REGION_ID_ENV_NAMES

logger = logging.getLogger(__name__)

def _section(data: Dict[str, Any], name: str) -> Dict[str, Any]:
    value = data.get(name) or {}
    return value if isinstance(value, dict) else {}


def _string(value: Any) -> str:
    return str(value).strip() if value is not None else ""


_ENV_NAME_PATTERN = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def _instance_id_from_controller_url(value: str) -> str:
    host = urlparse(value.strip()).hostname or ""
    parts = host.split(".")
    if len(parts) >= 2 and parts[0] == "controller":
        return parts[1]
    return ""


def _worker_instance_id() -> str:
    explicit = _string(os.getenv("AGENTTEAMS_INSTANCE_ID"))
    if explicit:
        return explicit
    return _instance_id_from_controller_url(_string(os.getenv("AGENTTEAMS_CONTROLLER_URL")))


def _worker_region_id() -> str:
    for name in REGION_ID_ENV_NAMES:
        value = _string(os.getenv(name))
        if value:
            return value
    return ""


def credential_provider_env_name(provider_name: str, instance_id: str = "") -> str:
    text = _string(provider_name)
    if _ENV_NAME_PATTERN.fullmatch(text):
        return text
    prefix = f"{_string(instance_id)}-"
    if prefix != "-" and text.startswith(prefix):
        suffix = text[len(prefix):]
        if _ENV_NAME_PATTERN.fullmatch(suffix):
            return suffix
    return ""


def _credential_provider_env_name(provider_name: str) -> str:
    return credential_provider_env_name(provider_name, _worker_instance_id())


def _download_path_part(value: str, fallback: str) -> str:
    text = value.strip() or fallback
    return re.sub(r"[^A-Za-z0-9._=-]+", "_", text).strip("._") or fallback


def _string_list(value: Any) -> List[str]:
    if not isinstance(value, list):
        return []
    result = []
    for item in value:
        text = _string(item)
        if text:
            result.append(text)
    return result


def _stable_json(value: Any) -> str:
    return json.dumps(value if value is not None else {}, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


def _count_collection(value: Any) -> int:
    if isinstance(value, dict):
        return len(value)
    if isinstance(value, list):
        return len(value)
    return 0


def _named_keys(value: Any) -> str:
    if not isinstance(value, dict):
        return "-"
    names = sorted(str(name).strip() for name in value.keys() if str(name).strip())
    return ",".join(names) if names else "-"


def _duration_ms(started_at: float) -> int:
    return max(0, int((time.monotonic() - started_at) * 1000))


def _strip_json_line_comments(text: str) -> str:
    result: List[str] = []
    in_string = False
    escaped = False
    index = 0
    while index < len(text):
        char = text[index]
        if in_string:
            result.append(char)
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                in_string = False
            index += 1
            continue

        if char == '"':
            in_string = True
            result.append(char)
            index += 1
            continue
        if char == "/" and index + 1 < len(text) and text[index + 1] == "/":
            index += 2
            while index < len(text) and text[index] not in "\r\n":
                index += 1
            continue
        result.append(char)
        index += 1
    return "".join(result)


def _string_fields(value: Any, keys: Iterable[str]) -> Dict[str, str]:
    if not isinstance(value, dict):
        return {}
    result: Dict[str, str] = {}
    for key in keys:
        text = _string(value.get(key))
        if text:
            result[key] = text
    return result


def _env_bool(name: str) -> bool:
    value = _string(os.getenv(name)).lower()
    return value in {"1", "true", "yes", "on"}


def _bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return bool(value)
    return _string(value).lower() in {"1", "true", "yes", "on"}


