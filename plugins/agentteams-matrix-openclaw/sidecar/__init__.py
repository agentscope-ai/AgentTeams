"""The AgentTeams Matrix sidecar — the production agent-side runtime.

Bridges the OpenClaw gateway to the Matrix core over the sidecar
protocol (v1, one JSON frame per line on a 0600 Unix socket). The
Matrix core is a separate repository, pointed at by the
``MATRIX_CORE_DIR`` environment variable; it is never vendored here.

Run it:

    python3 -m sidecar --config /path/to/sidecar-config.json

or manage it as a process via ``sidecar.sh`` (start/stop/restart/
status/run). See README.md, "Running the sidecar (production entry)".
"""

from sidecar._core import CoreImportError, core_dir, load_core
from sidecar.bridge import AgentBridge, run_outbound_worker
from sidecar.config import (
    ConfigError,
    DEFAULT_AGENT_NAME,
    SidecarConfig,
    default_config_path,
    default_socket_path,
    load_config,
    resolve_config_path,
)

__all__ = [
    "AgentBridge",
    "ConfigError",
    "CoreImportError",
    "DEFAULT_AGENT_NAME",
    "SidecarConfig",
    "core_dir",
    "default_config_path",
    "default_socket_path",
    "load_config",
    "load_core",
    "resolve_config_path",
    "run_outbound_worker",
]
