"""Typed, secret-safe AgentTeams runtime configuration."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Mapping


class ConfigError(ValueError):
    """Raised when the projected runtime configuration is unsafe or invalid."""


@dataclass(frozen=True)
class ModelConfig:
    """Higress-backed chat model configuration."""

    name: str
    gateway_url: str
    gateway_key: str


@dataclass(frozen=True)
class MatrixConfig:
    """Matrix transport configuration."""

    homeserver_url: str
    user_id: str
    room_id: str
    access_token: str
    encryption_enabled: bool


@dataclass(frozen=True)
class StorageConfig:
    """MinIO/OSS workspace configuration."""

    provider: str
    endpoint: str
    bucket: str
    member_prefix: str
    access_key: str
    secret_key: str


@dataclass(frozen=True)
class CheckpointConfig:
    """Encrypted PostgreSQL checkpoint configuration."""

    dsn: str
    aes_key: str


@dataclass(frozen=True)
class ApprovalConfig:
    """Human approval policy defaults."""

    file_writes: str
    mcp_default: str


@dataclass(frozen=True)
class ExecutionConfig:
    """Execution sandbox selection."""

    mode: str
    idle_timeout_seconds: int
    max_lifetime_seconds: int


@dataclass(frozen=True)
class RuntimeConfig:
    """Validated AgentTeams configuration consumed by a DeepAgents worker."""

    generation: int
    runtime_name: str
    model: ModelConfig
    matrix: MatrixConfig
    storage: StorageConfig
    checkpoint: CheckpointConfig
    approvals: ApprovalConfig
    execution: ExecutionConfig

    @classmethod
    def from_document(
        cls,
        document: Mapping[str, Any],
        *,
        environ: Mapping[str, str],
    ) -> "RuntimeConfig":
        """Validate and parse a controller-projected runtime document."""
        matrix = document.get("matrix")
        if isinstance(matrix, Mapping) and matrix.get("accessToken"):
            raise ConfigError("matrix.accessToken must be supplied through an environment variable")
        desired = document.get("desired")
        model = desired.get("model") if isinstance(desired, Mapping) else None
        if isinstance(model, Mapping) and model.get("gatewayKey"):
            raise ConfigError("desired.model.gatewayKey must be supplied through an environment variable")

        metadata = _required_mapping(document, "metadata")
        member = _required_mapping(document, "member")
        matrix_config = _required_mapping(document, "matrix")
        desired_config = _required_mapping(document, "desired")
        model_config = _required_mapping(desired_config, "model")
        storage_config = _required_mapping(document, "storage")
        credentials = _required_mapping(document, "credentials")

        if _required_str(member, "runtime") != "deepagents":
            raise ConfigError("member.runtime must be deepagents")

        runtime_specific = desired_config.get("runtimeConfig", {})
        runtime_specific = _mapping_value(runtime_specific, "desired.runtimeConfig")
        deepagents_config = runtime_specific.get("deepagents", {})
        deepagents_config = _mapping_value(deepagents_config, "desired.runtimeConfig.deepagents")
        approval_config = _mapping_value(deepagents_config.get("approvals", {}), "deepagents.approvals")
        execution_config = _mapping_value(deepagents_config.get("execution", {}), "deepagents.execution")

        return cls(
            generation=_required_int(metadata, "generation"),
            runtime_name=_required_str(member, "runtimeName"),
            model=ModelConfig(
                name=_required_str(model_config, "model"),
                gateway_url=_required_str(model_config, "gatewayUrl"),
                gateway_key=_secret_from_env(credentials, "gatewayKeyEnv", environ),
            ),
            matrix=MatrixConfig(
                homeserver_url=_required_str(matrix_config, "homeserverUrl"),
                user_id=_required_str(member, "matrixUserId"),
                room_id=_required_str(member, "personalRoomId"),
                access_token=_secret_from_env(credentials, "matrixTokenEnv", environ),
                encryption_enabled=bool(matrix_config.get("encryptionEnabled", True)),
            ),
            storage=StorageConfig(
                provider=_required_str(storage_config, "provider"),
                endpoint=_required_str(storage_config, "endpoint"),
                bucket=_required_str(storage_config, "bucket"),
                member_prefix=_required_str(storage_config, "memberPrefix"),
                access_key=_secret_from_env(credentials, "storageAccessKeyEnv", environ),
                secret_key=_secret_from_env(credentials, "storageSecretKeyEnv", environ),
            ),
            checkpoint=CheckpointConfig(
                dsn=_secret_from_env(credentials, "checkpointDSNEnv", environ),
                aes_key=_secret_from_env(credentials, "checkpointAESKeyEnv", environ),
            ),
            approvals=ApprovalConfig(
                file_writes=str(approval_config.get("fileWrites", "notRequired")),
                mcp_default=str(approval_config.get("mcpDefault", "required")),
            ),
            execution=ExecutionConfig(
                mode=_execution_mode(execution_config.get("mode", "disabled")),
                idle_timeout_seconds=1800,
                max_lifetime_seconds=28800,
            ),
        )


def _mapping_value(value: object, field: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise ConfigError(f"{field} must be an object")
    return value


def _required_mapping(document: Mapping[str, Any], field: str) -> Mapping[str, Any]:
    if field not in document:
        raise ConfigError(f"{field} is required")
    return _mapping_value(document[field], field)


def _required_str(document: Mapping[str, Any], field: str) -> str:
    value = document.get(field)
    if not isinstance(value, str) or not value.strip():
        raise ConfigError(f"{field} must be a non-empty string")
    return value.strip()


def _required_int(document: Mapping[str, Any], field: str) -> int:
    value = document.get(field)
    if not isinstance(value, int):
        raise ConfigError(f"{field} must be an integer")
    return value


def _secret_from_env(
    credentials: Mapping[str, Any],
    field: str,
    environ: Mapping[str, str],
) -> str:
    env_name = _required_str(credentials, field)
    value = environ.get(env_name)
    if not value:
        raise ConfigError(f"environment variable {env_name} referenced by {field} is required")
    return value


def _execution_mode(value: object) -> str:
    if value not in {"disabled", "sandbox"}:
        raise ConfigError("deepagents.execution.mode must be disabled or sandbox")
    return str(value)
