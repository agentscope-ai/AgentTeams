"""Sidecar configuration: JSON file + environment override, fail-closed.

Field precedence: environment wins over the JSON file, which wins over
built-in defaults. The file is optional; when it exists it must be a
regular file with mode 0600 — it carries the agent's Matrix credentials,
so anything more permissive is refused with a chmod hint rather than
silently read. Empty string values are configuration errors in both
places (an explicit blank is never "use the default"). The token is
masked out of ``repr()`` and of every error message.
"""

from __future__ import annotations

import dataclasses
import json
import os
import stat

DEFAULT_AGENT_NAME = "agentteams"

# config field -> environment variable that overrides it
FIELD_ENV = {
    "homeserver_url": "AGENTTEAMS_MATRIX_URL",
    "agent_user": "AGENTTEAMS_MATRIX_USER",
    "agent_access_token": "AGENTTEAMS_MATRIX_TOKEN",
    "socket_path": "AGENTTEAMS_MATRIX_SOCKET",
    "agent_name": "AGENTTEAMS_MATRIX_AGENT",
}
KNOWN_KEYS = tuple(FIELD_ENV)
REQUIRED_KEYS = ("homeserver_url", "agent_user", "agent_access_token")


class ConfigError(RuntimeError):
    """The sidecar configuration is missing, invalid, or unsafe."""


@dataclasses.dataclass(frozen=True)
class SidecarConfig:
    homeserver_url: str
    agent_user: str
    agent_access_token: str
    socket_path: str
    agent_name: str

    def __repr__(self) -> str:  # the token must never print
        return (
            f"SidecarConfig(homeserver_url={self.homeserver_url!r}, "
            f"agent_user={self.agent_user!r}, "
            "agent_access_token=<masked>, "
            f"socket_path={self.socket_path!r}, "
            f"agent_name={self.agent_name!r})"
        )


def _state_dir(env_name: str, default_rel: str) -> str:
    value = os.environ.get(env_name, "")
    if value:
        return value
    return os.path.join(os.path.expanduser("~"), *default_rel)


def default_socket_path() -> str:
    state_dir = _state_dir("AGENTTEAMS_MATRIX_STATE_DIR", (".agentteams", "matrix"))
    return os.path.join(state_dir, "sidecar.sock")


def default_config_path() -> str:
    state_dir = _state_dir(
        "AGENTTEAMS_OPENCLAW_STATE_DIR", (".agentteams", "openclaw-matrix")
    )
    return os.path.join(state_dir, "sidecar-config.json")


def resolve_config_path(cli_value: str | None = None) -> str:
    """CLI flag > AGENTTEAMS_MATRIX_SIDECAR_CONFIG > the state-dir default."""
    return (
        cli_value
        or os.environ.get("AGENTTEAMS_MATRIX_SIDECAR_CONFIG", "")
        or default_config_path()
    )


def _read_file(path: str) -> dict:
    st = os.stat(path)
    if not stat.S_ISREG(st.st_mode):
        raise ConfigError(f"config path {path} is not a regular file")
    mode = st.st_mode & 0o777
    if mode != 0o600:
        raise ConfigError(
            f"config file {path} has mode {oct(mode)}; it holds the agent "
            f"credentials and must be 0600 (run: chmod 600 {path})"
        )
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError) as exc:
        raise ConfigError(f"cannot read config file {path}: {exc}") from None
    if not isinstance(data, dict):
        raise ConfigError(f"config file {path} must contain a JSON object")
    unknown = sorted(set(data) - set(KNOWN_KEYS))
    if unknown:
        raise ConfigError(
            f"config file {path} has unknown keys: {', '.join(unknown)} "
            f"(known keys: {', '.join(KNOWN_KEYS)})"
        )
    values = {}
    for key, value in data.items():
        if not isinstance(value, str):
            raise ConfigError(f"config file {path}: {key!r} must be a string")
        if value == "":
            raise ConfigError(
                f"config file {path}: {key!r} is empty; give it a value or "
                "remove the key"
            )
        values[key] = value
    return values


def _validate(values: dict) -> None:
    url = values["homeserver_url"]
    if not (url.startswith("http://") or url.startswith("https://")):
        raise ConfigError(
            f"homeserver_url must start with http:// or https://, got {url!r}"
        )
    socket_path = values["socket_path"]
    if not os.path.isabs(socket_path):
        raise ConfigError(
            f"socket_path must be an absolute path, got {socket_path!r}"
        )


def load_config(cli_path: str | None = None) -> SidecarConfig:
    """Build a validated :class:`SidecarConfig`; raise ConfigError otherwise."""
    path = resolve_config_path(cli_path)
    values: dict = {}
    if os.path.exists(path):
        values.update(_read_file(path))
    for key, env_name in FIELD_ENV.items():
        env_value = os.environ.get(env_name, "")
        if env_value:
            values[key] = env_value
    values.setdefault("socket_path", default_socket_path())
    values.setdefault("agent_name", DEFAULT_AGENT_NAME)

    missing = [key for key in REQUIRED_KEYS if not values.get(key)]
    if missing:
        named = ", ".join(f"{key} ({FIELD_ENV[key]})" for key in missing)
        raise ConfigError(
            f"missing required configuration: {named} — set the environment "
            f"variables or write them into {path}"
        )
    _validate(values)
    return SidecarConfig(**{key: values[key] for key in FIELD_ENV})
