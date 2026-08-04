"""Typed, secret-safe AgentTeams runtime configuration."""

from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from deepagents_agentteams.approvals import MCPApprovalRule


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
    mcp_rules: tuple[MCPApprovalRule, ...]
    coordinators: tuple[str, ...]


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
    worker_name: str
    worker_uid: str
    runtime_name: str
    controller_url: str
    service_account_token_path: str
    mcp_servers: tuple[dict[str, str], ...]
    room_ids: tuple[str, ...]
    human_approver_ids: frozenset[str]
    agent_matrix_ids: frozenset[str]
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
    ) -> RuntimeConfig:
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
        mcp_servers = _mcp_servers(desired_config.get("mcpServers", []))
        team_config = _mapping_value(document.get("team", {}), "team")
        idle_timeout_seconds = _duration_seconds(
            execution_config.get("idleTimeout", "30m"),
            "deepagents.execution.idleTimeout",
        )
        max_lifetime_seconds = _duration_seconds(
            execution_config.get("maxLifetime", "8h"),
            "deepagents.execution.maxLifetime",
        )
        coordinators = _string_tuple(
            approval_config.get("coordinators", []),
            "deepagents.approvals.coordinators",
        )
        personal_room_id = _required_str(member, "personalRoomId")
        team_room_id = _optional_str(team_config, "teamRoomId")
        room_ids = tuple(dict.fromkeys(room for room in (personal_room_id, team_room_id) if room))
        member_matrix_user_id = _required_str(member, "matrixUserId")
        agent_matrix_ids = set(
            _string_tuple(matrix_config.get("agentUserIds", []), "matrix.agentUserIds")
        )
        agent_matrix_ids.add(member_matrix_user_id)
        human_approver_ids = set(coordinators)
        members = team_config.get("members", [])
        if not isinstance(members, list):
            raise ConfigError("team.members must be an array")
        for index, item in enumerate(members):
            team_member = _mapping_value(item, f"team.members[{index}]")
            matrix_user_id = _optional_str(team_member, "matrixUserId")
            role = _optional_str(team_member, "role")
            if matrix_user_id and role in {"team_leader", "worker"}:
                agent_matrix_ids.add(matrix_user_id)
            elif matrix_user_id and role == "coordinator":
                human_approver_ids.add(matrix_user_id)
        admin = _mapping_value(team_config.get("admin", {}), "team.admin")
        admin_matrix_user_id = _optional_str(admin, "matrixUserId")
        if admin_matrix_user_id:
            human_approver_ids.add(admin_matrix_user_id)
        human_approver_ids.difference_update(agent_matrix_ids)

        return cls(
            generation=_required_int(metadata, "generation"),
            worker_name=_required_str(member, "name"),
            worker_uid=str(member.get("uid") or _required_str(member, "name")),
            runtime_name=_required_str(member, "runtimeName"),
            controller_url=str(environ.get("AGENTTEAMS_CONTROLLER_URL", "")).rstrip("/"),
            service_account_token_path=str(
                credentials.get("serviceAccountTokenPath", "/var/run/secrets/agentteams/token")
            ),
            mcp_servers=mcp_servers,
            room_ids=room_ids,
            human_approver_ids=frozenset(human_approver_ids),
            agent_matrix_ids=frozenset(agent_matrix_ids),
            model=ModelConfig(
                name=_required_str(model_config, "model"),
                gateway_url=_required_str(model_config, "gatewayUrl"),
                gateway_key=_secret_from_env(credentials, "gatewayKeyEnv", environ),
            ),
            matrix=MatrixConfig(
                homeserver_url=_required_str(matrix_config, "homeserverUrl"),
                user_id=member_matrix_user_id,
                room_id=personal_room_id,
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
                aes_key=_checkpoint_key(_secret_from_env(credentials, "checkpointAESKeyEnv", environ)),
            ),
            approvals=ApprovalConfig(
                file_writes=_approval_mode(approval_config.get("fileWrites", "notRequired"), "fileWrites"),
                mcp_default=_approval_mode(approval_config.get("mcpDefault", "required"), "mcpDefault"),
                mcp_rules=_mcp_approval_rules(approval_config.get("mcpRules", [])),
                coordinators=coordinators,
            ),
            execution=ExecutionConfig(
                mode=_execution_mode(execution_config.get("mode", "disabled")),
                idle_timeout_seconds=idle_timeout_seconds,
                max_lifetime_seconds=max_lifetime_seconds,
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


def _optional_str(document: Mapping[str, Any], field: str) -> str:
    value = document.get(field)
    if value is None or value == "":
        return ""
    if not isinstance(value, str):
        raise ConfigError(f"{field} must be a string")
    return value.strip()


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


def _checkpoint_key(value: str) -> str:
    if len(value.encode("utf-8")) not in {16, 24, 32}:
        raise ConfigError("checkpoint AES key must be 16, 24, or 32 UTF-8 bytes")
    return value


def _execution_mode(value: object) -> str:
    if value not in {"disabled", "sandbox"}:
        raise ConfigError("deepagents.execution.mode must be disabled or sandbox")
    return str(value)


def _approval_mode(value: object, field: str) -> str:
    if value not in {"required", "notRequired"}:
        raise ConfigError(f"deepagents.approvals.{field} must be required or notRequired")
    return str(value)


def _string_tuple(value: object, field: str) -> tuple[str, ...]:
    if not isinstance(value, list):
        raise ConfigError(f"{field} must be an array")
    result: list[str] = []
    for item in value:
        if not isinstance(item, str) or not item.strip():
            raise ConfigError(f"{field} entries must be non-empty strings")
        result.append(item.strip())
    return tuple(result)


def _mcp_approval_rules(value: object) -> tuple[MCPApprovalRule, ...]:
    if not isinstance(value, list):
        raise ConfigError("deepagents.approvals.mcpRules must be an array")
    rules: list[MCPApprovalRule] = []
    for index, item in enumerate(value):
        rule = _mapping_value(item, f"deepagents.approvals.mcpRules[{index}]")
        rules.append(
            MCPApprovalRule(
                server=_required_str(rule, "server"),
                tool=_required_str(rule, "tool"),
                mode=_approval_mode(rule.get("mode"), f"mcpRules[{index}].mode"),
            )
        )
    return tuple(rules)


def _mcp_servers(value: object) -> tuple[dict[str, str], ...]:
    if not isinstance(value, list):
        raise ConfigError("desired.mcpServers must be an array")
    result: list[dict[str, str]] = []
    for index, item in enumerate(value):
        server = _mapping_value(item, f"desired.mcpServers[{index}]")
        transport = str(server.get("transport", "http"))
        if transport not in {"http", "sse"}:
            raise ConfigError(f"desired.mcpServers[{index}].transport must be http or sse")
        result.append(
            {
                "name": _required_str(server, "name"),
                "url": _required_str(server, "url"),
                "transport": transport,
            }
        )
    return tuple(result)


_DURATION_PART = re.compile(r"(?P<value>[0-9]+(?:\.[0-9]+)?)(?P<unit>h|m|s)")


def _duration_seconds(value: object, field: str) -> int:
    if not isinstance(value, str) or not value:
        raise ConfigError(f"{field} must be a positive duration")
    position = 0
    seconds = 0.0
    unit_seconds = {"h": 3600, "m": 60, "s": 1}
    for match in _DURATION_PART.finditer(value):
        if match.start() != position:
            raise ConfigError(f"{field} must use h, m, or s duration units")
        seconds += float(match.group("value")) * unit_seconds[match.group("unit")]
        position = match.end()
    if position != len(value) or seconds <= 0 or not seconds.is_integer():
        raise ConfigError(f"{field} must resolve to a positive whole number of seconds")
    return int(seconds)
