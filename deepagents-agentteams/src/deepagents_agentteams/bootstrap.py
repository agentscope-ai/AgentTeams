"""Secret-safe runtime configuration bootstrap from AgentTeams object storage."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any
from urllib.parse import urlsplit

import yaml
from minio import Minio

from deepagents_agentteams.config import ConfigError

_MAX_RUNTIME_CONFIG_BYTES = 1024 * 1024


def build_storage_client(environ: Mapping[str, str]) -> Minio:
    """Build the bootstrap storage client from Worker secret environment."""
    endpoint = environ.get("AGENTTEAMS_FS_ENDPOINT", "")
    parsed = urlsplit(endpoint)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc or parsed.path not in {"", "/"}:
        raise ConfigError("AGENTTEAMS_FS_ENDPOINT must be an HTTP(S) origin URL")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ConfigError("AGENTTEAMS_FS_ENDPOINT must not contain credentials, query, or fragment")
    access_key = environ.get("AGENTTEAMS_FS_ACCESS_KEY", "")
    secret_key = environ.get("AGENTTEAMS_FS_SECRET_KEY", "")
    if not access_key or not secret_key:
        raise ConfigError("AgentTeams storage credentials are required")
    return Minio(
        parsed.netloc,
        access_key=access_key,
        secret_key=secret_key,
        secure=parsed.scheme == "https",
    )


def fetch_runtime_document(
    environ: Mapping[str, str],
    *,
    client: Any | None = None,
) -> Mapping[str, Any]:
    """Fetch and parse the controller-projected ``runtime/runtime.yaml``."""
    runtime_name = environ.get("AGENTTEAMS_WORKER_NAME", "").strip()
    bucket = environ.get("AGENTTEAMS_FS_BUCKET", "").strip()
    if not runtime_name or not bucket:
        raise ConfigError("AGENTTEAMS_WORKER_NAME and AGENTTEAMS_FS_BUCKET are required")
    storage_client = client or build_storage_client(environ)
    object_name = f"agents/{runtime_name}/runtime/runtime.yaml"
    response = storage_client.get_object(bucket, object_name)
    try:
        payload = response.read(_MAX_RUNTIME_CONFIG_BYTES + 1)
    finally:
        response.close()
        response.release_conn()
    if len(payload) > _MAX_RUNTIME_CONFIG_BYTES:
        raise ConfigError("runtime configuration exceeds the 1 MiB limit")
    document = yaml.safe_load(payload)
    if not isinstance(document, Mapping):
        raise ConfigError("runtime configuration must be a YAML object")
    return document
