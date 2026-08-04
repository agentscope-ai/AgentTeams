"""Runtime configuration loading without a mandatory YAML dependency."""

from __future__ import annotations

from dataclasses import dataclass
import json
from pathlib import Path
import re
from typing import Any


_SENSITIVE_KEY = re.compile(r"(?:password|secret|token|api[_-]?key|authorization)", re.IGNORECASE)


class ConfigError(ValueError):
    """Raised when the remote-member runtime snapshot is unsafe or incomplete."""


def _scalar(value: str) -> Any:
    text = value.strip()
    if not text:
        return {}
    if text in {"null", "Null", "NULL", "~"}:
        return None
    if text.lower() in {"true", "false"}:
        return text.lower() == "true"
    if (text.startswith('"') and text.endswith('"')) or (
        text.startswith("'") and text.endswith("'")
    ):
        return text[1:-1]
    try:
        return json.loads(text)
    except (json.JSONDecodeError, TypeError):
        return text.split(" #", 1)[0].rstrip()


def _load_simple_yaml(text: str) -> dict[str, Any]:
    """Parse the mapping subset used by MemberRuntimeConfig.

    Full YAML is used when PyYAML is available. This fallback deliberately
    ignores sequence entries because this bridge only consumes identity,
    routing, model, and credential-location mappings.
    """

    root: dict[str, Any] = {}
    stack: list[tuple[int, dict[str, Any]]] = [(-1, root)]
    for number, raw in enumerate(text.splitlines(), start=1):
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        leading = raw[: len(raw) - len(raw.lstrip())]
        if "\t" in leading:
            raise ConfigError(f"tabs are not supported in runtime YAML at line {number}")
        if raw.lstrip().startswith("- "):
            continue
        indent = len(raw) - len(raw.lstrip(" "))
        body = raw.strip()
        if ":" not in body:
            raise ConfigError(f"invalid runtime YAML mapping at line {number}")
        key, value = body.split(":", 1)
        key = key.strip()
        if not key:
            raise ConfigError(f"empty runtime YAML key at line {number}")
        while stack[-1][0] >= indent:
            stack.pop()
        parent = stack[-1][1]
        parsed = _scalar(value)
        parent[key] = parsed
        if parsed == {}:
            stack.append((indent, parsed))
    return root


def load_mapping(path: Path) -> dict[str, Any]:
    text = path.read_text(encoding="utf-8")
    try:
        value = json.loads(text)
    except json.JSONDecodeError:
        try:
            import yaml  # type: ignore[import-not-found]

            value = yaml.safe_load(text)
        except ImportError:
            value = _load_simple_yaml(text)
        except Exception as exc:  # PyYAML exposes multiple parser exception types.
            raise ConfigError(f"invalid runtime YAML: {exc}") from exc
    if not isinstance(value, dict):
        raise ConfigError("runtime config root must be a mapping")
    return value


def _mapping(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _text(mapping: dict[str, Any], *keys: str) -> str:
    for key in keys:
        value = mapping.get(key)
        if value is not None:
            text = str(value).strip()
            if text:
                return text
    return ""


def _assert_no_inline_secrets(value: Any, path: tuple[str, ...] = ()) -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            key_text = str(key)
            lowered = key_text.lower()
            location_only = lowered.endswith(("env", "path", "ref", "name"))
            if _SENSITIVE_KEY.search(key_text) and child not in (None, "", {}) and not location_only:
                dotted = ".".join((*path, key_text))
                raise ConfigError(f"runtime config must not contain secret value at {dotted}")
            _assert_no_inline_secrets(child, (*path, key_text))
    elif isinstance(value, list):
        for index, child in enumerate(value):
            _assert_no_inline_secrets(child, (*path, str(index)))


@dataclass(frozen=True)
class RuntimeConfig:
    team_name: str
    member_name: str
    matrix_user_id: str
    personal_room_id: str
    team_room_id: str
    leader_matrix_user_id: str
    model: str
    matrix_token_env: str

    @classmethod
    def from_path(cls, path: Path) -> "RuntimeConfig":
        raw = load_mapping(path)
        _assert_no_inline_secrets(raw)
        team = _mapping(raw.get("team"))
        member = _mapping(raw.get("member"))
        desired = _mapping(raw.get("desired"))
        model = _mapping(desired.get("model"))
        credentials = _mapping(raw.get("credentials"))

        config = cls(
            team_name=_text(team, "name"),
            member_name=_text(member, "name", "runtimeName", "runtime_name"),
            matrix_user_id=_text(member, "matrixUserId", "matrix_user_id"),
            personal_room_id=_text(member, "personalRoomId", "personal_room_id"),
            team_room_id=_text(team, "teamRoomId", "team_room_id"),
            leader_matrix_user_id=_text(
                team,
                "leaderMatrixUserId",
                "leader_matrix_user_id",
            ),
            model=_text(model, "model"),
            matrix_token_env=_text(
                credentials,
                "matrixTokenEnv",
                "matrix_token_env",
            )
            or "AGENTTEAMS_WORKER_MATRIX_TOKEN",
        )
        missing = [
            name
            for name, value in (
                ("team.name", config.team_name),
                ("member.name", config.member_name),
                ("member.matrixUserId", config.matrix_user_id),
            )
            if not value
        ]
        if missing:
            raise ConfigError("runtime config missing required fields: " + ", ".join(missing))
        return config
