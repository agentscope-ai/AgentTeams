"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

import yaml

from qwenpaw_worker.update.constants import AGENT_IDENTITY_DATA_ENDPOINT_FORMAT
from qwenpaw_worker.update.utils import (
    _credential_provider_env_name,
    _section,
    _stable_json,
    _string,
    _string_fields,
    _string_list,
    _worker_region_id,
)


@dataclass(frozen=True)
class MemberRuntimeConfig:
    """Normalized runtime.yaml snapshot used by QwenPaw worker and adapter."""

    path: Path
    raw: Dict[str, Any]

    @classmethod
    def load(cls, path: Path) -> "MemberRuntimeConfig":
        if not path.exists():
            raise FileNotFoundError(f"runtime config missing: {path}")
        data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
        if not isinstance(data, dict):
            raise ValueError("runtime config must be a YAML object")
        member = _section(data, "member")
        runtime = _string(member.get("runtime"))
        if runtime and runtime != "qwenpaw":
            raise ValueError(f"runtime must be qwenpaw, got {runtime}")
        return cls(path=path, raw=data)

    @property
    def generation(self) -> str:
        return _string(_section(self.raw, "metadata").get("generation"))

    @property
    def team(self) -> Dict[str, Any]:
        return _section(self.raw, "team")

    @property
    def team_members(self) -> List[Dict[str, str]]:
        raw = self.team.get("members")
        if not isinstance(raw, list):
            return []
        members: List[Dict[str, str]] = []
        for item in raw:
            entry = _string_fields(item, ("name", "runtimeName", "role", "matrixUserId", "personalRoomId"))
            if entry:
                members.append(entry)
        return members

    @property
    def member(self) -> Dict[str, Any]:
        return _section(self.raw, "member")

    @property
    def desired(self) -> Dict[str, Any]:
        return _section(self.raw, "desired")

    @property
    def storage(self) -> Dict[str, Any]:
        return _section(self.raw, "storage")

    @property
    def credentials(self) -> Dict[str, Any]:
        return _section(self.raw, "credentials")

    @property
    def agent_identity_data(self) -> Dict[str, str]:
        return _string_fields(_section(self.raw, "agentIdentityData"), ("endpoint", "regionId"))

    @property
    def agent_identity_data_region_id(self) -> str:
        return _string(self.agent_identity_data.get("regionId")) or _worker_region_id()

    @property
    def agent_identity_data_endpoint(self) -> str:
        endpoint = _string(self.agent_identity_data.get("endpoint"))
        if endpoint:
            return endpoint
        region_id = self.agent_identity_data_region_id
        if region_id:
            return AGENT_IDENTITY_DATA_ENDPOINT_FORMAT.format(region_id=region_id)
        return ""

    @property
    def agent_identity(self) -> Dict[str, str]:
        return _string_fields(_section(self.desired, "agentIdentity"), ("workloadIdentityName",))

    @property
    def workload_identity_name(self) -> str:
        return _string(self.agent_identity.get("workloadIdentityName"))

    @property
    def credential_bindings(self) -> List[Dict[str, Any]]:
        raw = self.desired.get("credentialBindings")
        if not isinstance(raw, list):
            return []
        bindings: List[Dict[str, Any]] = []
        for item in raw:
            if not isinstance(item, dict):
                continue
            credential_ref = _string_fields(
                _section(item, "credentialRef"),
                ("tokenVaultName", "apiKeyCredentialProviderName"),
            )
            if credential_ref:
                binding: Dict[str, Any] = {"credentialRef": credential_ref}
                tool_whitelist = _string_list(item.get("toolWhitelist"))
                if tool_whitelist:
                    binding["toolWhitelist"] = tool_whitelist
                bindings.append(binding)
        return bindings

    @property
    def credential_binding_env_names(self) -> List[str]:
        names: List[str] = []
        for binding in self.credential_bindings:
            name = _credential_provider_env_name(
                _string(binding.get("credentialRef", {}).get("apiKeyCredentialProviderName"))
            )
            if name and name not in names:
                names.append(name)
        return names

    @property
    def credential_binding_env_provider_names(self) -> Dict[str, str]:
        providers: Dict[str, str] = {}
        for binding in self.credential_bindings:
            provider_name = _string(binding.get("credentialRef", {}).get("apiKeyCredentialProviderName"))
            env_name = _credential_provider_env_name(provider_name)
            if env_name and env_name not in providers:
                providers[env_name] = provider_name
        return providers

    @property
    def credential_runtime_identity(self) -> str:
        return _stable_json(
            {
                "agentIdentity": self.agent_identity,
                "agentIdentityData": self.agent_identity_data,
                "agentIdentityDataEndpoint": self.agent_identity_data_endpoint,
                "credentialBindings": self.credential_bindings,
            }
        )

    @property
    def team_name(self) -> str:
        return _string(self.team.get("name"))

    @property
    def member_name(self) -> str:
        return _string(self.member.get("name") or self.member.get("runtimeName"))

    @property
    def member_role(self) -> str:
        return _string(self.member.get("role") or "worker")

    @property
    def agent_package(self) -> Dict[str, Any]:
        return _section(self.desired, "agentPackage")

    @property
    def agent_package_identity(self) -> Tuple[str, str, str, str]:
        package = self.agent_package
        return (
            _string(package.get("ref")),
            _string(package.get("name")),
            _string(package.get("version")),
            _string(package.get("digest")),
        )

    @property
    def model(self) -> Dict[str, Any]:
        return _section(self.desired, "model")

    @property
    def mcp_servers(self) -> Any:
        value = self.desired.get("mcpServers")
        return value if value is not None else {}

    @property
    def channels(self) -> Dict[str, Any]:
        return _section(self.desired, "channels")

    @property
    def dingtalk_channel(self) -> Optional[Dict[str, Any]]:
        value = self.channels.get("dingtalk")
        return value if isinstance(value, dict) else None

    @property
    def channel_policy(self) -> Dict[str, Any]:
        return _section(self.desired, "channelPolicy")

    @property
    def desired_identity(self) -> Tuple[str, str, str, str, str, str, str, str]:
        return (
            *self.agent_package_identity,
            _stable_json(self.model),
            _stable_json(self.mcp_servers),
            _stable_json(self.channels),
            _stable_json(self.channel_policy),
        )

    @property
    def team_context_facts(self) -> Dict[str, Any]:
        team = _string_fields(
            self.team,
            ("name", "teamRoomId", "leaderName", "leaderRuntimeName", "leaderDmRoomId"),
        )
        admin = _string_fields(_section(self.team, "admin"), ("name", "matrixUserId"))
        if admin:
            team["admin"] = admin  # type: ignore[assignment]
        if self.team_members:
            team["members"] = self.team_members  # type: ignore[assignment]

        facts: Dict[str, Any] = {}
        if self.generation:
            facts["metadata"] = {"generation": self.generation}
        if team:
            facts["team"] = team
        member = _string_fields(
            self.member,
            ("name", "runtimeName", "role", "runtime", "matrixUserId", "personalRoomId"),
        )
        if member:
            facts["member"] = member
        return facts

    @property
    def team_context_identity(self) -> str:
        return _stable_json(self.team_context_facts)

    @property
    def output_sanitize_policy(self) -> Dict[str, Any]:
        return _section(self.desired, "outputSanitize")

    @property
    def output_sanitize_keywords(self) -> List[str]:
        return _string_list(self.output_sanitize_policy.get("keywords"))

    @property
    def output_sanitize_env_refs(self) -> List[str]:
        refs = _string_list(self.output_sanitize_policy.get("envRefs"))
        for key in (
            "matrixTokenEnv",
            "gatewayKeyEnv",
            "storageAccessKeyEnv",
            "storageSecretKeyEnv",
        ):
            value = _string(self.credentials.get(key))
            if value and value not in refs:
                refs.append(value)
        return refs

    def changed_from(self, previous: "MemberRuntimeConfig") -> bool:
        return (
            self.generation != previous.generation
            or self.desired_identity != previous.desired_identity
            or self.team_context_identity != previous.team_context_identity
            or self.credential_runtime_identity != previous.credential_runtime_identity
        )
