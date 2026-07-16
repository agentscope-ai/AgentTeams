"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import asyncio
import base64
from dataclasses import dataclass
import hashlib
import hmac
import json
import logging
import os
import re
import shutil
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path
from typing import Any, Callable, Dict, Iterable, List, Optional, Tuple
from urllib.parse import parse_qs, quote, urlencode, urlparse

import yaml

from qwenpaw_worker.config import WorkerConfig
logger = logging.getLogger(__name__)
DEFAULT_AGENT_ID = "default"
TEAMS_PROMPT_FILE = "TEAMS.md"
PACKAGE_PROMPT_FILES = ("AGENTS.md", "SOUL.md")
PACKAGE_RUNTIME_OWNED_CONFIG_FILES = {Path(TEAMS_PROMPT_FILE)}
TEAMS_INTERNAL_CONTROL_MARKER = (
    "<!-- AGENTTEAMS_INTERNAL_CONTROL_FILE: TEAMS.md is managed by "
    "TeamHarness/QwenPaw runtime; agent packages must not overwrite or delete it. -->"
)
TEAMS_CONTEXT_START = "<!-- BEGIN AGENTTEAMS RUNTIME TEAM CONTEXT -->"
TEAMS_CONTEXT_END = "<!-- END AGENTTEAMS RUNTIME TEAM CONTEXT -->"
AGENT_IDENTITY_DATA_ENDPOINT_FORMAT = "agentidentitydata.{region_id}.aliyuncs.com"
REGION_ID_ENV_NAMES = ("AGENTTEAMS_REGION", "ALIBABA_CLOUD_REGION_ID", "REGION_ID")


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


class AgentPackageManager:
    """Download and extract desired AgentSpec packages without restarting."""

    def __init__(self, root_dir: Path, workspace_dir: Optional[Path] = None) -> None:
        self.root_dir = root_dir
        self.workspace_dir = workspace_dir
        self.current_dir = root_dir / "current"
        self.marker_path = root_dir / "current.identity"
        self.root_dir.mkdir(parents=True, exist_ok=True)

    def apply(self, config: MemberRuntimeConfig) -> Optional[Path]:
        identity = config.agent_package_identity
        if not any(identity):
            return None
        if self._current_identity() == identity and self.current_dir.exists():
            self._apply_to_workspace_atomic(self.current_dir)
            return self.current_dir

        package_path = self._fetch(identity[0])
        staging = Path(tempfile.mkdtemp(prefix="qwenpaw-agent-package-", dir=str(self.root_dir)))
        previous = self.current_dir if self.current_dir.exists() else None
        workspace_snapshot = None
        try:
            self._extract(package_path, staging)
            workspace_snapshot = self._snapshot_workspace(staging, previous)
            self._cleanup_stale_workspace_targets(previous, staging)
            self._apply_to_workspace(staging, previous)
            self._commit_current(staging, identity)
            self._cleanup_workspace_snapshot(workspace_snapshot)
            return self.current_dir
        except Exception:
            self._restore_workspace_snapshot(workspace_snapshot)
            shutil.rmtree(staging, ignore_errors=True)
            raise

    def _current_identity(self) -> Tuple[str, str, str, str]:
        if not self.marker_path.exists():
            return ("", "", "", "")
        parts = self.marker_path.read_text(encoding="utf-8").splitlines()
        return tuple((parts + ["", "", "", ""])[:4])  # type: ignore[return-value]

    def _fetch(self, ref: str) -> Path:
        if not ref:
            raise RuntimeError("desired.agentPackage.ref is required")
        parsed = urlparse(ref)
        if parsed.scheme in ("", "file"):
            path = Path(parsed.path if parsed.scheme == "file" else ref)
            if not path.exists():
                raise RuntimeError(f"agent package not found: {ref}")
            return path
        if parsed.scheme in ("http", "https"):
            target = self.root_dir / "downloads" / Path(parsed.path).name
            target.parent.mkdir(parents=True, exist_ok=True)
            urllib.request.urlretrieve(ref, target)
            return target
        if parsed.scheme == "oss":
            return self._fetch_oss(parsed)
        if parsed.scheme == "nacos":
            return self._fetch_nacos(parsed)
        raise RuntimeError(f"unsupported agent package ref scheme: {parsed.scheme}")

    def _fetch_oss(self, parsed) -> Path:
        oss_path = f"{parsed.netloc}{parsed.path}".strip("/")
        if not oss_path:
            raise RuntimeError("oss agent package path is required")
        target = self.root_dir / "downloads" / Path(oss_path).name
        target.parent.mkdir(parents=True, exist_ok=True)
        if target.exists():
            return target

        storage_prefix = os.getenv("AGENTTEAMS_STORAGE_PREFIX", "").strip().rstrip("/") or "agentteams/agentteams-storage"
        remote = f"{storage_prefix}/{oss_path}"
        try:
            subprocess.run(["mc", "cp", remote, str(target)], check=True, capture_output=True, text=True)
        except FileNotFoundError:
            raise RuntimeError("mc binary not found for oss agent package fetch") from None
        except subprocess.CalledProcessError as exc:
            detail = (exc.stderr or exc.stdout or "").strip()
            message = f": {detail}" if detail else ""
            raise RuntimeError(f"fetch oss agent package failed: {remote}{message}") from None
        return target

    def _fetch_nacos(self, parsed) -> Path:
        parts = [part for part in parsed.path.strip("/").split("/") if part]
        if len(parts) < 2:
            raise RuntimeError(
                f"invalid nacos agent package ref: expected nacos://[user:pass@]host:port/"
                f"{{namespace}}/{{agentspec-name}}[/{{version}}], got {parsed.geturl()}"
            )

        namespace, spec_name = parts[0], parts[1]
        version = parts[2] if len(parts) >= 3 else ""
        label = ""
        if version.startswith("label:"):
            label = version.removeprefix("label:")
            version = ""
        query = parse_qs(parsed.query)
        query_version = _string((query.get("version") or [""])[0])
        query_label = _string((query.get("label") or [""])[0])
        if query_version:
            version = query_version
            label = ""
        if query_label:
            label = query_label
            version = ""

        auth_type = (query.get("authType") or [""])[0].strip()
        if auth_type == "sts-agentteams":
            return self._fetch_nacos_cli(parsed, namespace, spec_name, version, label, auth_type)

        output_dir = self._nacos_download_output_dir(namespace, spec_name, version, label)
        target = output_dir / spec_name
        if target.exists():
            shutil.rmtree(target)
        target.mkdir(parents=True, exist_ok=True)

        spec = self._get_nacos_agentspec(parsed, namespace, spec_name, version, label)
        resources = spec.get("resource") or {}
        if isinstance(resources, dict):
            for resource in resources.values():
                if isinstance(resource, dict):
                    self._write_nacos_resource(target, resource)

        content = _string(spec.get("content"))
        try:
            content = json.dumps(json.loads(content), ensure_ascii=False, indent=2)
        except Exception:
            pass
        (target / "manifest.json").write_text(content, encoding="utf-8")
        return target

    def _fetch_nacos_cli(
        self,
        parsed,
        namespace: str,
        spec_name: str,
        version: str,
        label: str,
        auth_type: str,
    ) -> Path:
        host = parsed.hostname
        if not host:
            raise RuntimeError(f"invalid nacos agent package ref: missing host in {parsed.geturl()}")
        port = parsed.port or 8848

        output_dir = self._nacos_download_output_dir(namespace, spec_name, version, label)
        target = output_dir / spec_name
        if target.exists():
            shutil.rmtree(target)
        output_dir.mkdir(parents=True, exist_ok=True)

        command = [
            "nacos-cli",
            "--host",
            host,
            "--port",
            str(port),
            "--namespace",
            namespace,
        ]
        if auth_type:
            command.extend(["--auth-type", auth_type])
        if auth_type == "sts-agentteams":
            access_key, secret_key, security_token = self._nacos_sts_credentials()
            command.extend(["--access-key", access_key, "--secret-key", secret_key])
            if security_token:
                command.extend(["--security-token", security_token])
        command.extend(["agentspec-get", spec_name, "-o", str(output_dir)])
        if version:
            command.extend(["--version", version])
        if label:
            command.extend(["--label", label])

        logger.info(
            "fetching nacos agentspec package component=update step=fetch_agentspec host=%s port=%s namespace=%s "
            "spec=%s version=%s label=%s",
            host,
            port,
            namespace,
            spec_name,
            version or "-",
            label or "-",
        )
        try:
            subprocess.run(command, check=True, capture_output=True, text=True)
        except FileNotFoundError:
            raise RuntimeError("nacos-cli binary not found for nacos agent package fetch") from None
        except subprocess.CalledProcessError as exc:
            detail = (exc.stderr or exc.stdout or "").strip()
            message = f": {detail}" if detail else ""
            raise RuntimeError(f"fetch agentspec {spec_name} from nacos with nacos-cli failed{message}") from None

        if not target.exists():
            raise RuntimeError(f"nacos-cli agentspec download finished but {target} was not created")
        if not target.is_dir():
            raise RuntimeError(f"nacos-cli agentspec output {target} is not a directory")
        return target

    def _nacos_download_output_dir(
        self,
        namespace: str,
        spec_name: str,
        version: str,
        label: str,
    ) -> Path:
        if version:
            selector = f"version-{version}"
        elif label:
            selector = f"label-{label}"
        else:
            selector = "latest"
        return (
            self.root_dir
            / "downloads"
            / "nacos"
            / _download_path_part(namespace, "default")
            / _download_path_part(spec_name, "agentspec")
            / _download_path_part(selector, "latest")
        )

    def _get_nacos_agentspec(self, parsed, namespace: str, spec_name: str, version: str, label: str) -> Dict[str, Any]:
        host = parsed.hostname
        if not host:
            raise RuntimeError(f"invalid nacos agent package ref: missing host in {parsed.geturl()}")
        port = parsed.port or 8848
        params = {"namespaceId": namespace, "name": spec_name}
        if version:
            params["version"] = version
        if label:
            params["label"] = label
        url = f"http://{host}:{port}/nacos/v3/client/ai/agentspecs?{urlencode(params)}"
        request = urllib.request.Request(url, headers=self._nacos_auth_headers(parsed, namespace))
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"fetch agentspec {spec_name} from nacos failed: HTTP {exc.code}: {body}") from None
        except urllib.error.URLError as exc:
            raise RuntimeError(f"fetch agentspec {spec_name} from nacos failed: {exc.reason}") from None
        except json.JSONDecodeError as exc:
            raise RuntimeError(f"fetch agentspec {spec_name} from nacos failed: invalid JSON response") from exc

        if int(payload.get("code", 0)) != 0:
            raise RuntimeError(
                f"fetch agentspec {spec_name} from nacos failed: code={payload.get('code')}, "
                f"message={payload.get('message')}"
            )
        data = payload.get("data") or {}
        if not isinstance(data, dict):
            raise RuntimeError(f"fetch agentspec {spec_name} from nacos failed: response data must be an object")
        return data

    def _nacos_auth_headers(self, parsed, namespace: str) -> Dict[str, str]:
        query = parse_qs(parsed.query)
        auth_type = (query.get("authType") or [""])[0].strip()
        token = os.getenv("AGENTTEAMS_NACOS_TOKEN", "").strip()
        username = parsed.username or os.getenv("AGENTTEAMS_NACOS_USERNAME", "")
        password = parsed.password or os.getenv("AGENTTEAMS_NACOS_PASSWORD", "")

        if auth_type == "sts-agentteams":
            return self._nacos_sts_auth_headers(namespace)
        if auth_type == "":
            if token:
                return {"Authorization": f"Bearer {token}"}
            auth_type = "nacos" if username or password else "none"
        if auth_type == "none":
            return {}
        if auth_type != "nacos":
            raise RuntimeError(f"unsupported nacos auth type: {auth_type}")
        if not username or not password:
            raise RuntimeError("nacos auth requires username and password")

        access_token = self._nacos_login(parsed, username, password)
        return {"Authorization": f"Bearer {access_token}"}

    def _nacos_sts_auth_headers(self, namespace: str) -> Dict[str, str]:
        access_key, secret_key, security_token = self._nacos_sts_credentials()
        timestamp = str(int(time.time() * 1000))
        sign_data = f"{namespace}+DEFAULT_GROUP+{timestamp}" if namespace else timestamp
        signature = base64.b64encode(
            hmac.new(secret_key.encode("utf-8"), sign_data.encode("utf-8"), hashlib.sha1).digest()
        ).decode("utf-8")
        headers = {
            "Spas-AccessKey": access_key,
            "Timestamp": timestamp,
            "Spas-Signature": signature,
        }
        if security_token:
            headers["Spas-SecurityToken"] = security_token
        return headers

    def _nacos_sts_credentials(self) -> Tuple[str, str, str]:
        sts = self._fetch_controller_sts()
        access_key = _string(
            sts.get("access_key_id")
            or sts.get("accessKeyId")
            or sts.get("accessKeyID")
            or sts.get("AccessKeyID")
        )
        secret_key = _string(
            sts.get("access_key_secret")
            or sts.get("accessKeySecret")
            or sts.get("AccessKeySecret")
        )
        security_token = _string(
            sts.get("security_token")
            or sts.get("securityToken")
            or sts.get("SecurityToken")
        )
        if not access_key or not secret_key:
            raise RuntimeError("controller STS response missing access key fields")
        return access_key, secret_key, security_token

    def _fetch_controller_sts(self) -> Dict[str, Any]:
        controller_url = os.getenv("AGENTTEAMS_CONTROLLER_URL", "").strip().rstrip("/")
        if not controller_url:
            raise RuntimeError("nacos authType=sts-agentteams requires AGENTTEAMS_CONTROLLER_URL")
        bearer = self._controller_bearer_token()
        headers = {"Authorization": f"Bearer {bearer}"}
        request = urllib.request.Request(
            f"{controller_url}/api/v1/credentials/sts",
            data=b"",
            headers=headers,
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                payload = json.loads(response.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"controller STS request failed: HTTP {exc.code}: {body}") from None
        except urllib.error.URLError as exc:
            raise RuntimeError(f"controller STS request failed: {exc.reason}") from None
        except json.JSONDecodeError as exc:
            raise RuntimeError("controller STS request failed: invalid JSON response") from exc
        if not isinstance(payload, dict):
            raise RuntimeError("controller STS response must be an object")
        return payload

    def _controller_bearer_token(self) -> str:
        token = os.getenv("AGENTTEAMS_AUTH_TOKEN", "").strip()
        if token:
            return token
        token_file = os.getenv("AGENTTEAMS_AUTH_TOKEN_FILE", "").strip()
        if token_file:
            path = Path(token_file)
            if not path.exists():
                raise RuntimeError(f"AGENTTEAMS_AUTH_TOKEN_FILE does not exist: {token_file}")
            token = path.read_text(encoding="utf-8").strip()
            if token:
                return token
        raise RuntimeError("nacos authType=sts-agentteams requires AGENTTEAMS_AUTH_TOKEN or AGENTTEAMS_AUTH_TOKEN_FILE")

    def _nacos_login(self, parsed, username: str, password: str) -> str:
        host = parsed.hostname
        if not host:
            raise RuntimeError(f"invalid nacos agent package ref: missing host in {parsed.geturl()}")
        port = parsed.port or 8848
        body = urlencode({"username": username, "password": password}).encode("utf-8")
        for path in ("/nacos/v3/auth/user/login", "/nacos/v1/auth/login"):
            request = urllib.request.Request(
                f"http://{host}:{port}{path}",
                data=body,
                headers={"Content-Type": "application/x-www-form-urlencoded"},
                method="POST",
            )
            try:
                with urllib.request.urlopen(request, timeout=60) as response:
                    payload = json.loads(response.read().decode("utf-8"))
            except Exception:
                continue
            data = payload.get("data") if isinstance(payload.get("data"), dict) else payload
            token = data.get("accessToken") if isinstance(data, dict) else ""
            if token:
                return _string(token)
        raise RuntimeError("nacos login failed")

    def _write_nacos_resource(self, target: Path, resource: Dict[str, Any]) -> None:
        content = resource.get("content")
        if content in (None, ""):
            return
        rel = self._nacos_resource_path(resource)
        if not rel:
            return
        self._ensure_inside_target(target, [rel])
        data = str(content).encode("utf-8")
        metadata = resource.get("metadata") if isinstance(resource.get("metadata"), dict) else {}
        if metadata.get("encoding") == "base64":
            data = base64.b64decode(str(content))
        destination = target / rel
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(data)

    def _nacos_resource_path(self, resource: Dict[str, Any]) -> str:
        resource_type = _string(resource.get("type"))
        resource_name = _string(resource.get("name")).strip("/")
        if not resource_type:
            return resource_name
        prefix = f"{resource_type}/"
        return resource_name if resource_name.startswith(prefix) else prefix + resource_name

    def _extract(self, package_path: Path, target_dir: Path) -> None:
        if package_path.is_dir():
            shutil.copytree(package_path, target_dir, dirs_exist_ok=True)
            return
        if tarfile.is_tarfile(package_path):
            with tarfile.open(package_path) as archive:
                self._safe_extract_tar(archive, target_dir)
            return
        if zipfile.is_zipfile(package_path):
            with zipfile.ZipFile(package_path) as archive:
                self._safe_extract_zip(archive, target_dir)
            return
        raise RuntimeError(f"unsupported agent package format: {package_path}")

    def _ensure_inside_target(self, target_dir: Path, names: Iterable[str]) -> None:
        target_root = target_dir.resolve()
        for name in names:
            resolved = (target_dir / name).resolve()
            try:
                resolved.relative_to(target_root)
            except ValueError:
                raise RuntimeError(f"unsafe agent package path: {name}")

    def _safe_extract_tar(self, archive: tarfile.TarFile, target_dir: Path) -> None:
        members = archive.getmembers()
        for member in members:
            if member.issym() or member.islnk():
                raise RuntimeError(f"unsafe agent package link: {member.name}")
        self._ensure_inside_target(target_dir, (member.name for member in members))
        archive.extractall(target_dir, members=members)

    def _safe_extract_zip(self, archive: zipfile.ZipFile, target_dir: Path) -> None:
        names = archive.namelist()
        self._ensure_inside_target(target_dir, names)
        archive.extractall(target_dir)

    def _apply_to_workspace_atomic(self, package_dir: Path) -> None:
        snapshot = self._snapshot_workspace(package_dir)
        try:
            self._apply_to_workspace(package_dir)
            self._cleanup_workspace_snapshot(snapshot)
        except Exception:
            self._restore_workspace_snapshot(snapshot)
            raise

    def _apply_to_workspace(self, package_dir: Path, previous_package_dir: Optional[Path] = None) -> None:
        if self.workspace_dir is None:
            return
        workspace_dir = self.workspace_dir
        package_root = self._package_content_root(package_dir)
        previous_root = self._package_content_root(previous_package_dir) if previous_package_dir is not None else None
        workspace_dir.mkdir(parents=True, exist_ok=True)

        config_dir = package_root / "config"
        if config_dir.is_dir():
            self._copy_config_to_workspace(config_dir)
        self._clear_missing_package_prompt_files(package_root)

        self._apply_package_mcp_config(package_root, previous_root)

        skills_dir = package_root / "skills"
        if skills_dir.is_dir():
            skill_names = self._copy_skills_to_workspace(skills_dir)
            self._reconcile_workspace_skills(skill_names)

    def _package_content_root(self, package_dir: Path) -> Path:
        if self._looks_like_agent_package(package_dir):
            return package_dir
        children = [path for path in package_dir.iterdir() if path.is_dir()]
        if len(children) == 1 and self._looks_like_agent_package(children[0]):
            return children[0]
        return package_dir

    def _looks_like_agent_package(self, path: Path) -> bool:
        markers = (
            "manifest.json",
            "template.json",
            "config",
            "skills",
            "AGENTS.md",
            "SOUL.md",
            "MEMORY.md",
            "BOOTSTRAP.md",
            "crons",
            "mcp.json",
        )
        return any((path / marker).exists() for marker in markers)

    def _config_files(self, config_dir: Path) -> List[Tuple[Path, Path]]:
        if self.workspace_dir is None:
            return []
        files: List[Tuple[Path, Path]] = []
        for child in sorted(config_dir.rglob("*")):
            if child.is_file():
                rel = child.relative_to(config_dir)
                if rel == Path("config/mcporter.json"):
                    continue
                if rel in PACKAGE_RUNTIME_OWNED_CONFIG_FILES:
                    continue
                files.append((child, self.workspace_dir / rel))
        return files

    def _workspace_targets(self, package_dir: Path) -> List[Path]:
        if self.workspace_dir is None:
            return []
        package_root = self._package_content_root(package_dir)
        workspace_dir = self.workspace_dir
        targets: List[Path] = []

        config_dir = package_root / "config"
        if config_dir.is_dir():
            targets.extend(target for _source, target in self._config_files(config_dir))
        for file_name in PACKAGE_PROMPT_FILES:
            targets.append(workspace_dir / file_name)

        skills_dir = package_root / "skills"
        if skills_dir.is_dir():
            targets.extend(workspace_dir / "skills" / source.name for source in skills_dir.iterdir() if source.is_dir())
            targets.append(workspace_dir / "skill.json")

        return self._dedupe_paths(targets)

    def _workspace_state_targets(self, package_dir: Path) -> List[Path]:
        if self.workspace_dir is None:
            return []
        package_root = self._package_content_root(package_dir)
        if self._package_mcp_clients(package_root):
            return [self.workspace_dir / "agent.json"]
        return []

    def _snapshot_workspace(
        self,
        package_dir: Path,
        previous_package_dir: Optional[Path] = None,
    ) -> Optional[Tuple[Path, List[Tuple[Path, Path, bool]]]]:
        targets = self._dedupe_paths(self._workspace_targets(package_dir) + self._workspace_state_targets(package_dir))
        if previous_package_dir is not None:
            targets = self._dedupe_paths(
                targets + self._workspace_targets(previous_package_dir) + self._workspace_state_targets(previous_package_dir)
            )
        if not targets:
            return None
        backup_root = Path(tempfile.mkdtemp(prefix="qwenpaw-workspace-rollback-", dir=str(self.root_dir)))
        entries: List[Tuple[Path, Path, bool]] = []
        try:
            for index, target in enumerate(targets):
                backup = backup_root / str(index)
                if target.exists():
                    if target.is_dir():
                        shutil.copytree(target, backup)
                    else:
                        backup.parent.mkdir(parents=True, exist_ok=True)
                        shutil.copy2(target, backup)
                    entries.append((target, backup, True))
                else:
                    entries.append((target, backup, False))
            return backup_root, entries
        except Exception:
            shutil.rmtree(backup_root, ignore_errors=True)
            raise

    def _cleanup_stale_workspace_targets(self, previous_package_dir: Optional[Path], package_dir: Path) -> None:
        if previous_package_dir is None or self.workspace_dir is None:
            return
        current_targets = {str(path) for path in self._workspace_targets(package_dir)}
        for target in self._workspace_targets(previous_package_dir):
            if str(target) in current_targets or not target.exists():
                continue
            if target.parent == self.workspace_dir and target.name in PACKAGE_PROMPT_FILES:
                target.write_text("", encoding="utf-8")
                continue
            if target.is_dir():
                shutil.rmtree(target)
            else:
                target.unlink()
            self._cleanup_empty_parent_dirs(target.parent)

    def _cleanup_empty_parent_dirs(self, path: Path) -> None:
        if self.workspace_dir is None:
            return
        workspace_root = self.workspace_dir.resolve()
        current = path
        while current != self.workspace_dir and str(current.resolve()).startswith(str(workspace_root)):
            try:
                current.rmdir()
            except OSError:
                return
            current = current.parent

    def _dedupe_paths(self, paths: Iterable[Path]) -> List[Path]:
        result = []
        seen = set()
        for path in paths:
            key = str(path)
            if key not in seen:
                result.append(path)
                seen.add(key)
        return result

    def _restore_workspace_snapshot(self, snapshot: Optional[Tuple[Path, List[Tuple[Path, Path, bool]]]]) -> None:
        if snapshot is None:
            return
        backup_root, entries = snapshot
        try:
            for target, backup, existed in reversed(entries):
                if target.exists():
                    if target.is_dir():
                        shutil.rmtree(target)
                    else:
                        target.unlink()
                if existed:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    if backup.is_dir():
                        shutil.copytree(backup, target)
                    else:
                        shutil.copy2(backup, target)
        finally:
            shutil.rmtree(backup_root, ignore_errors=True)

    def _cleanup_workspace_snapshot(self, snapshot: Optional[Tuple[Path, List[Tuple[Path, Path, bool]]]]) -> None:
        if snapshot is not None:
            shutil.rmtree(snapshot[0], ignore_errors=True)

    def _commit_current(self, staging: Path, identity: Tuple[str, str, str, str]) -> None:
        backup = Path(tempfile.mkdtemp(prefix="qwenpaw-agent-package-current-", dir=str(self.root_dir)))
        shutil.rmtree(backup, ignore_errors=True)
        moved_current = False
        moved_staging = False
        try:
            if self.current_dir.exists():
                self.current_dir.rename(backup)
                moved_current = True
            staging.rename(self.current_dir)
            moved_staging = True
            self._write_current_identity(identity)
            if moved_current:
                shutil.rmtree(backup, ignore_errors=True)
        except Exception:
            if moved_staging and self.current_dir.exists():
                shutil.rmtree(self.current_dir, ignore_errors=True)
            if moved_current and backup.exists():
                backup.rename(self.current_dir)
            raise

    def _write_current_identity(self, identity: Tuple[str, str, str, str]) -> None:
        tmp = self.marker_path.with_name(f".{self.marker_path.name}.tmp")
        tmp.write_text("\n".join(identity), encoding="utf-8")
        tmp.replace(self.marker_path)

    def _copy_config_to_workspace(self, config_dir: Path) -> None:
        if self.workspace_dir is None:
            return
        for source, target in self._config_files(config_dir):
            self._replace_path(source, target)

    def _clear_missing_package_prompt_files(self, package_root: Path) -> None:
        if self.workspace_dir is None:
            return
        config_dir = package_root / "config"
        for file_name in PACKAGE_PROMPT_FILES:
            if (config_dir / file_name).is_file():
                continue
            target = self.workspace_dir / file_name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("", encoding="utf-8")

    def _apply_package_mcp_config(self, package_root: Path, previous_package_root: Optional[Path]) -> None:
        current_clients = self._package_mcp_clients(package_root)
        previous_clients = self._package_mcp_clients(previous_package_root)
        if not current_clients and not previous_clients:
            return
        try:
            from qwenpaw.config.config import MCPClientConfig, MCPConfig, load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_package_mcp action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "mcp", None) is None:
            agent_config.mcp = MCPConfig()
        clients = dict(getattr(agent_config.mcp, "clients", None) or {})
        for name in previous_clients:
            if name not in current_clients:
                clients.pop(name, None)
        for name, payload in current_clients.items():
            clients[name] = MCPClientConfig(**payload)
        agent_config.mcp.clients = clients
        save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _package_mcp_clients(self, package_root: Optional[Path]) -> Dict[str, Dict[str, Any]]:
        if package_root is None:
            return {}
        mcp_path = package_root / "mcp.json"
        if not mcp_path.is_file():
            return {}
        try:
            raw = json.loads(_strip_json_line_comments(mcp_path.read_text(encoding="utf-8")))
        except json.JSONDecodeError as exc:
            raise RuntimeError(f"agent package mcp.json is invalid JSON: {mcp_path}") from exc
        if not isinstance(raw, dict):
            raise RuntimeError("agent package mcp.json must be a JSON object")

        clients_raw: Any = raw
        if isinstance(raw.get("mcpServers"), (dict, list)):
            clients_raw = raw["mcpServers"]
        elif isinstance(raw.get("clients"), (dict, list)):
            clients_raw = raw["clients"]
        elif isinstance(raw.get("mcp"), dict) and isinstance(raw["mcp"].get("clients"), (dict, list)):
            clients_raw = raw["mcp"]["clients"]

        clients: Dict[str, Dict[str, Any]] = {}
        if isinstance(clients_raw, list):
            for item in clients_raw:
                if not isinstance(item, dict):
                    continue
                name = _string(item.get("name") or item.get("id"))
                if not name:
                    continue
                clients[name] = self._qwenpaw_mcp_client_payload(name, item)
            return clients

        if isinstance(clients_raw, dict):
            for name, item in clients_raw.items():
                if not isinstance(item, dict):
                    continue
                client_name = _string(item.get("name") or name)
                if not client_name:
                    continue
                clients[client_name] = self._qwenpaw_mcp_client_payload(client_name, item)
        return clients

    def _qwenpaw_mcp_client_payload(self, name: str, item: Dict[str, Any]) -> Dict[str, Any]:
        payload = dict(item)
        payload.pop("id", None)
        if "name" not in payload:
            payload["name"] = name
        if "enabled" not in payload and "isActive" in payload:
            payload["enabled"] = bool(payload.pop("isActive"))
        else:
            payload.pop("isActive", None)
        if "url" not in payload and "baseUrl" in payload:
            payload["url"] = payload.pop("baseUrl")
        else:
            payload.pop("baseUrl", None)
        if "transport" not in payload and "type" in payload:
            payload["transport"] = payload.pop("type")
        else:
            payload.pop("type", None)
        payload = self._expand_mcp_workspace_placeholders(payload)
        self._ensure_mcp_stdio_workspace_env(payload)
        return payload

    def _mcp_workspace_env_value(self) -> str:
        if self.workspace_dir is not None:
            return str(self.workspace_dir)
        return _string(os.getenv("AGENT_WORKSPACE"))

    def _expand_mcp_workspace_placeholders(self, value: Any) -> Any:
        workspace = self._mcp_workspace_env_value()
        if not workspace:
            return value
        if isinstance(value, str):
            return (
                value.replace("${AGENT_WORKSPACE}", workspace)
                .replace("{AGENT_WORKSPACE}", workspace)
            )
        if isinstance(value, list):
            return [self._expand_mcp_workspace_placeholders(item) for item in value]
        if isinstance(value, dict):
            return {
                key: self._expand_mcp_workspace_placeholders(item)
                for key, item in value.items()
            }
        return value

    def _ensure_mcp_stdio_workspace_env(self, payload: Dict[str, Any]) -> None:
        workspace = self._mcp_workspace_env_value()
        if not workspace:
            return
        transport = _string(payload.get("transport") or "stdio").lower()
        if transport != "stdio" or not _string(payload.get("command")):
            return
        env = payload.get("env")
        if not isinstance(env, dict):
            env = {}
        env["AGENT_WORKSPACE"] = workspace
        payload["env"] = env

    def _copy_skills_to_workspace(self, skills_dir: Path) -> List[str]:
        if self.workspace_dir is None:
            return []
        target_root = self.workspace_dir / "skills"
        target_root.mkdir(parents=True, exist_ok=True)
        copied = []
        for source in skills_dir.iterdir():
            if source.is_dir():
                self._replace_path(source, target_root / source.name)
                copied.append(source.name)
        return copied

    def _replace_path(self, source: Path, target: Path) -> None:
        if target.exists():
            if target.is_dir():
                shutil.rmtree(target)
            else:
                target.unlink()
        target.parent.mkdir(parents=True, exist_ok=True)
        if source.is_dir():
            shutil.copytree(source, target)
        elif source.is_file():
            shutil.copy2(source, target)

    def _reconcile_workspace_skills(self, skill_names: Optional[List[str]] = None) -> None:
        if self.workspace_dir is None:
            return
        try:
            from qwenpaw.agents.skill_system.registry import reconcile_workspace_manifest
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=reconcile_workspace_skills action=skip")
            return
        reconcile_workspace_manifest(self.workspace_dir)
        self._enable_workspace_skills(skill_names or [])

    def _enable_workspace_skills(self, skill_names: List[str]) -> None:
        if self.workspace_dir is None or not skill_names:
            return
        manifest_path = self.workspace_dir / "skill.json"
        if not manifest_path.exists():
            return
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except Exception:
            logger.warning("workspace skill manifest is invalid component=update step=enable_workspace_skills action=skip")
            return
        skills = manifest.setdefault("skills", {})
        changed = False
        for skill_name in skill_names:
            entry = skills.get(skill_name)
            if isinstance(entry, dict) and entry.get("enabled") is not True:
                entry["enabled"] = True
                changed = True
        if changed:
            tmp = manifest_path.with_name(f".{manifest_path.name}.tmp")
            tmp.write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
            tmp.replace(manifest_path)


@dataclass(frozen=True)
class ApplyResult:
    runtime_config: MemberRuntimeConfig
    changed: bool
    agent_package_dir: Optional[Path]


@dataclass(frozen=True)
class _QwenPawApiResponse:
    status: int
    payload: Any
    text: str


class QwenPawModelRuntimeSync:
    """Sync runtime model state into the running QwenPaw app."""

    def __init__(
        self,
        port: int,
        agent_id: str = DEFAULT_AGENT_ID,
        timeout: float = 10.0,
    ) -> None:
        self.api_root = f"http://127.0.0.1:{port}/api/models"
        self.agent_id = agent_id
        self.timeout = timeout

    def __call__(self, runtime_config: MemberRuntimeConfig) -> None:
        self.sync(runtime_config)

    def sync(self, runtime_config: MemberRuntimeConfig) -> None:
        started_at = time.monotonic()
        fields = self._model_fields(runtime_config)
        if fields is None:
            return
        provider_id, model_name, provider_name, base_url, api_key, chat_model = fields
        if not base_url or not api_key:
            logger.info(
                "runtime model sync skipped component=update step=model_runtime_sync event=skip "
                "generation=%s provider_id=%s model=%s reason=missing_provider_config",
                runtime_config.generation,
                provider_id,
                model_name,
            )
            return

        provider_count = self._provider_count(self._request("GET", "").payload)
        provider_info = self._configure_provider(provider_id, provider_name, base_url, api_key, chat_model)
        if not self._provider_has_model(provider_info, model_name):
            provider_info = self._add_model(provider_id, model_name)
        self._set_active_model(provider_id, model_name)
        self._verify_active_model(provider_id, model_name)
        logger.info(
            "runtime model sync complete component=update step=model_runtime_sync event=complete "
            "generation=%s provider_id=%s model=%s provider_count=%s changed=%s duration_ms=%s",
            runtime_config.generation,
            provider_id,
            model_name,
            provider_count,
            True,
            _duration_ms(started_at),
        )

    def _model_fields(self, runtime_config: MemberRuntimeConfig) -> Optional[Tuple[str, str, str, str, str, str]]:
        model = runtime_config.model
        provider_id = _string(model.get("providerId") or model.get("provider_id") or model.get("provider"))
        model_name = _string(model.get("model") or model.get("name"))
        if not provider_id or not model_name:
            return None
        base_url = _string(
            model.get("baseUrl")
            or model.get("base_url")
            or model.get("gatewayUrl")
            or model.get("gateway_url")
            or model.get("endpoint")
            or os.getenv("AGENTTEAMS_AI_GATEWAY_URL")
        )
        api_key = _string(model.get("apiKey") or model.get("api_key"))
        api_key_env = _string(
            model.get("apiKeyEnv")
            or model.get("api_key_env")
            or runtime_config.credentials.get("gatewayKeyEnv")
            or "AGENTTEAMS_WORKER_GATEWAY_KEY"
        )
        if not api_key and api_key_env:
            api_key = _string(os.getenv(api_key_env))
        return (
            provider_id,
            model_name,
            _string(model.get("providerName") or model.get("provider_name") or provider_id),
            self._openai_compatible_base_url(base_url) if base_url else "",
            api_key,
            _string(model.get("chatModel") or model.get("chat_model") or "OpenAIChatModel"),
        )

    def _configure_provider(
        self,
        provider_id: str,
        provider_name: str,
        base_url: str,
        api_key: str,
        chat_model: str,
    ) -> Dict[str, Any]:
        payload = {"api_key": api_key, "base_url": base_url, "chat_model": chat_model}
        path = f"/{quote(provider_id, safe='')}/config"
        response = self._request("PUT", path, payload, ok_statuses=(200, 404))
        if response.status == 404:
            self._request(
                "POST",
                "/custom-providers",
                {
                    "id": provider_id,
                    "name": provider_name,
                    "default_base_url": base_url,
                    "api_key_prefix": "",
                    "chat_model": chat_model,
                    "models": [],
                },
                ok_statuses=(200, 201),
            )
            response = self._request("PUT", path, payload)
        return response.payload if isinstance(response.payload, dict) else {}

    def _add_model(self, provider_id: str, model_name: str) -> Dict[str, Any]:
        response = self._request(
            "POST",
            f"/{quote(provider_id, safe='')}/models",
            {"id": model_name, "name": model_name},
            ok_statuses=(200, 201, 400, 404, 409, 422),
        )
        if response.status >= 400 and not self._already_exists(response.text):
            raise RuntimeError(
                f"qwenpaw model runtime sync request failed: POST provider model HTTP {response.status}"
            )
        return response.payload if isinstance(response.payload, dict) else {}

    def _set_active_model(self, provider_id: str, model_name: str) -> None:
        self._request(
            "PUT",
            "/active",
            {
                "scope": "agent",
                "agent_id": self.agent_id,
                "provider_id": provider_id,
                "model": model_name,
            },
        )

    def _verify_active_model(self, provider_id: str, model_name: str) -> None:
        response = self._request(
            "GET",
            f"/active?{urlencode({'scope': 'effective', 'agent_id': self.agent_id})}",
        )
        payload = response.payload if isinstance(response.payload, dict) else {}
        active = payload.get("active_llm") if isinstance(payload.get("active_llm"), dict) else {}
        actual_provider = _string(active.get("provider_id") or active.get("providerId"))
        actual_model = _string(active.get("model"))
        if actual_provider != provider_id or actual_model != model_name:
            raise RuntimeError("qwenpaw model runtime sync verification failed")

    def _request(
        self,
        method: str,
        path: str,
        payload: Optional[Dict[str, Any]] = None,
        ok_statuses: Iterable[int] = (200, 201),
    ) -> _QwenPawApiResponse:
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {"Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            f"{self.api_root}{path}",
            data=body,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                text = response.read().decode("utf-8", errors="replace")
                return _QwenPawApiResponse(
                    status=response.status,
                    payload=self._json_payload(text, response.status),
                    text=text,
                )
        except urllib.error.HTTPError as exc:
            text = exc.read().decode("utf-8", errors="replace")
            if exc.code in ok_statuses:
                return _QwenPawApiResponse(
                    status=exc.code,
                    payload=self._json_payload(text, exc.code),
                    text=text,
                )
            raise RuntimeError(
                f"qwenpaw model runtime sync request failed: {method} {path.split('?', 1)[0]} HTTP {exc.code}"
            ) from None
        except urllib.error.URLError as exc:
            raise RuntimeError(
                f"qwenpaw model runtime sync request failed: {method} {path.split('?', 1)[0]} "
                f"{type(exc.reason).__name__}"
            ) from None

    def _json_payload(self, text: str, status: int) -> Any:
        if not text:
            return {}
        try:
            return json.loads(text)
        except json.JSONDecodeError as exc:
            if status >= 400:
                return {}
            raise RuntimeError("qwenpaw model runtime sync request failed: invalid JSON response") from exc

    def _provider_count(self, payload: Any) -> int:
        return len(payload) if isinstance(payload, list) else 0

    def _provider_has_model(self, payload: Dict[str, Any], model_name: str) -> bool:
        for key in ("models", "extra_models"):
            value = payload.get(key)
            if not isinstance(value, list):
                continue
            for item in value:
                if isinstance(item, dict) and _string(item.get("id")) == model_name:
                    return True
        return False

    def _already_exists(self, text: str) -> bool:
        lower = text.lower()
        return "already" in lower or "exist" in lower or "duplicate" in lower

    def _openai_compatible_base_url(self, base_url: str) -> str:
        value = base_url.rstrip("/")
        if value.endswith("/v1"):
            return value
        return f"{value}/v1"


class RuntimeUpdater:
    """Apply controller-projected runtime desired state inside one worker pod."""

    def __init__(
        self,
        config: WorkerConfig,
        adapter_apply: Optional[Callable[[], None]] = None,
        package_manager: Optional[AgentPackageManager] = None,
        runtime_config_pull: Optional[Callable[[], None]] = None,
        model_runtime_sync: Optional[Callable[[MemberRuntimeConfig], None]] = None,
        team_context_renderer: Optional[Callable[[MemberRuntimeConfig], str]] = None,
    ) -> None:
        self.config = config
        self.adapter_apply = adapter_apply
        self.runtime_config_pull = runtime_config_pull
        self.model_runtime_sync = model_runtime_sync or QwenPawModelRuntimeSync(config.console_port)
        self.team_context_renderer = team_context_renderer
        self.package_manager = package_manager or AgentPackageManager(
            config.qwenpaw_working_dir / "agent-packages",
            workspace_dir=config.default_workspace_dir,
        )
        self.current_config: Optional[MemberRuntimeConfig] = None

    def load(self) -> MemberRuntimeConfig:
        if self.runtime_config_pull is not None:
            self.runtime_config_pull()
        return MemberRuntimeConfig.load(self.config.runtime_config_path)

    def apply_once(
        self,
        runtime_config: Optional[MemberRuntimeConfig] = None,
        force: bool = False,
        reapply_adapter: bool = True,
    ) -> ApplyResult:
        started_at = time.monotonic()
        config = runtime_config or self.load()
        previous = self.current_config
        changed = force or previous is None or config.changed_from(previous)
        if not changed:
            logger.info(
                "runtime config apply skipped component=update worker=%s generation=%s changed=%s "
                "mcp_server_count=%s channel_names=%s credential_binding_count=%s duration_ms=%s",
                self.config.worker_name,
                config.generation,
                False,
                _count_collection(config.mcp_servers),
                _named_keys(config.channels),
                len(config.credential_bindings),
                _duration_ms(started_at),
            )
            return ApplyResult(runtime_config=config, changed=False, agent_package_dir=None)

        adapter_should_apply = (
            reapply_adapter
            and self.adapter_apply is not None
            and not self._adapter_neutral_change(config)
        )
        logger.info(
            "runtime config apply begin component=update worker=%s generation=%s team=%s member=%s role=%s "
            "force=%s reapply_adapter=%s adapter_applied=%s mcp_server_count=%s channel_names=%s "
            "credential_binding_count=%s duration_ms=%s",
            self.config.worker_name,
            config.generation,
            config.team_name,
            config.member_name,
            config.member_role,
            force,
            reapply_adapter,
            adapter_should_apply,
            _count_collection(config.mcp_servers),
            _named_keys(config.channels),
            len(config.credential_bindings),
            _duration_ms(started_at),
        )
        self._apply_member_identity(config)
        self._apply_model(config)
        self._apply_mcp_servers(config)
        self._apply_matrix_channel(config)
        self._apply_dingtalk_channel(config)
        self._apply_channel_policy(config)
        self._apply_team_context_prompt(config)

        applied_package = self.package_manager.apply(config)

        adapter_applied = False
        if adapter_should_apply:
            self.adapter_apply()
            adapter_applied = True

        self._sync_model_runtime_if_needed(previous, config)
        self.current_config = config
        logger.info(
            "runtime config apply complete component=update worker=%s generation=%s changed=%s "
            "agent_package_dir=%s mcp_server_count=%s channel_names=%s credential_binding_count=%s "
            "adapter_applied=%s duration_ms=%s",
            self.config.worker_name,
            config.generation,
            True,
            applied_package,
            _count_collection(config.mcp_servers),
            _named_keys(config.channels),
            len(config.credential_bindings),
            adapter_applied,
            _duration_ms(started_at),
        )
        return ApplyResult(runtime_config=config, changed=True, agent_package_dir=applied_package)

    def _sync_model_runtime_if_needed(
        self,
        previous: Optional[MemberRuntimeConfig],
        config: MemberRuntimeConfig,
    ) -> None:
        if previous is None or self.model_runtime_sync is None:
            return
        if _stable_json(config.model) == _stable_json(previous.model):
            return
        started_at = time.monotonic()
        try:
            self.model_runtime_sync(config)
        except Exception as exc:
            logger.warning(
                "runtime model sync failed component=update step=model_runtime_sync event=failed "
                "worker=%s generation=%s changed=%s error_type=%s safe_error_summary=%s duration_ms=%s",
                self.config.worker_name,
                config.generation,
                True,
                type(exc).__name__,
                type(exc).__name__,
                _duration_ms(started_at),
            )
            raise

    def _adapter_neutral_change(self, config: MemberRuntimeConfig) -> bool:
        previous = self.current_config
        if previous is None:
            return False
        return (
            config.agent_package_identity == previous.agent_package_identity
            and _stable_json(config.model) == _stable_json(previous.model)
            and _stable_json(config.channel_policy) == _stable_json(previous.channel_policy)
            and config.credential_runtime_identity == previous.credential_runtime_identity
            and self._team_context_content_identity(config) == self._team_context_content_identity(previous)
            and (
                _stable_json(config.mcp_servers) != _stable_json(previous.mcp_servers)
                or _stable_json(config.channels) != _stable_json(previous.channels)
            )
        )

    def _team_context_content_identity(self, config: MemberRuntimeConfig) -> str:
        facts = dict(config.team_context_facts)
        facts.pop("metadata", None)
        return _stable_json(facts)

    def _load_and_apply_once(self) -> None:
        self.apply_once(runtime_config=self.load(), reapply_adapter=False)

    def _apply_team_context_prompt(self, config: MemberRuntimeConfig) -> None:
        block = self._runtime_team_context_block(config)
        if not block:
            return
        path = self.config.default_workspace_dir / TEAMS_PROMPT_FILE
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            existing = path.read_text(encoding="utf-8")
        else:
            existing = self._render_full_team_context_prompt(config)
            if not existing:
                logger.warning(
                    "full TeamHarness TEAMS renderer unavailable component=update worker=%s action=fallback",
                    self.config.worker_name,
                )
                existing = "# TeamHarness Runtime Context\n"
        existing = self._ensure_teams_internal_marker(existing)
        if TEAMS_CONTEXT_START in existing and TEAMS_CONTEXT_END in existing:
            prefix, rest = existing.split(TEAMS_CONTEXT_START, 1)
            _old, suffix = rest.split(TEAMS_CONTEXT_END, 1)
            text = prefix.rstrip() + "\n\n" + block + suffix
        else:
            text = existing.rstrip() + "\n\n" + block + "\n"
        tmp = path.with_name(f".{path.name}.tmp")
        tmp.write_text(text, encoding="utf-8")
        tmp.replace(path)

    def _render_full_team_context_prompt(self, config: MemberRuntimeConfig) -> str:
        if self.team_context_renderer is None:
            return ""
        try:
            text = self.team_context_renderer(config)
        except Exception as exc:
            logger.warning(
                "full TeamHarness TEAMS renderer failed component=update worker=%s error_type=%s",
                self.config.worker_name,
                type(exc).__name__,
            )
            return ""
        return text if isinstance(text, str) and text.strip() else ""

    def _ensure_teams_internal_marker(self, text: str) -> str:
        if TEAMS_INTERNAL_CONTROL_MARKER in text:
            return text
        body = text.lstrip("\n")
        return f"{TEAMS_INTERNAL_CONTROL_MARKER}\n{body}" if body else f"{TEAMS_INTERNAL_CONTROL_MARKER}\n"

    def _runtime_team_context_block(self, config: MemberRuntimeConfig) -> str:
        facts = config.team_context_facts
        if not facts:
            return ""
        team = _section(facts, "team")
        member = _section(facts, "member")
        lines = [
            TEAMS_CONTEXT_START,
            "## Runtime Team Context",
            "",
        ]
        for key, value in (
            ("team.name", team.get("name")),
            ("team.teamRoomId", team.get("teamRoomId")),
            ("team.leaderName", team.get("leaderName")),
            ("team.leaderRuntimeName", team.get("leaderRuntimeName")),
            ("team.leaderDmRoomId", team.get("leaderDmRoomId")),
            ("team.admin.name", _section(team, "admin").get("name")),
            ("team.admin.matrixUserId", _section(team, "admin").get("matrixUserId")),
            ("member.name", member.get("name")),
            ("member.runtimeName", member.get("runtimeName")),
            ("member.role", member.get("role")),
            ("member.runtime", member.get("runtime")),
            ("member.matrixUserId", member.get("matrixUserId")),
            ("member.personalRoomId", member.get("personalRoomId")),
        ):
            text = _string(value)
            if text:
                lines.append(f"- {key}: {text}")
        members = team.get("members")
        if isinstance(members, list) and members:
            lines.extend(["", "### Team Members"])
            for item in members:
                entry = _string_fields(item, ("name", "runtimeName", "role", "matrixUserId", "personalRoomId"))
                if entry:
                    lines.append("- " + ", ".join(f"{key}: {value}" for key, value in entry.items()))
        lines.extend(["", "Do not write secrets, credentials, or live task status into this file.", TEAMS_CONTEXT_END])
        return "\n".join(lines)

    def _apply_member_identity(self, config: MemberRuntimeConfig) -> None:
        role = config.member_role
        if role:
            self.config.agent_role = role
            os.environ["AGENTTEAMS_AGENT_ROLE"] = role
            os.environ["AGENTTEAMS_WORKER_ROLE"] = role

    def _apply_model(self, config: MemberRuntimeConfig) -> None:
        model = config.model
        if not model:
            return
        provider_id = _string(model.get("providerId") or model.get("provider_id") or model.get("provider"))
        model_name = _string(model.get("model") or model.get("name"))
        if not provider_id or not model_name:
            return
        try:
            from qwenpaw.config.config import ModelSlotConfig, load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_model action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        agent_config.active_model = ModelSlotConfig(provider_id=provider_id, model=model_name)
        save_agent_config(DEFAULT_AGENT_ID, agent_config)
        self._apply_openai_compatible_provider(config, provider_id, model_name, ModelSlotConfig)

    def _apply_openai_compatible_provider(
        self,
        config: MemberRuntimeConfig,
        provider_id: str,
        model_name: str,
        model_slot_config_class: Any,
    ) -> None:
        model = config.model
        base_url = _string(
            model.get("baseUrl")
            or model.get("base_url")
            or model.get("gatewayUrl")
            or model.get("gateway_url")
            or model.get("endpoint")
            or os.getenv("AGENTTEAMS_AI_GATEWAY_URL")
        )
        api_key = _string(model.get("apiKey") or model.get("api_key"))
        api_key_env = _string(
            model.get("apiKeyEnv")
            or model.get("api_key_env")
            or config.credentials.get("gatewayKeyEnv")
            or "AGENTTEAMS_WORKER_GATEWAY_KEY"
        )
        if not api_key and api_key_env:
            api_key = _string(os.getenv(api_key_env))
        if not base_url or not api_key:
            return

        try:
            from qwenpaw.providers.provider import ModelInfo, ProviderInfo
            from qwenpaw.providers.provider_manager import ProviderManager
        except ImportError:
            logger.info("qwenpaw provider package unavailable component=update step=apply_provider action=skip")
            return

        manager = ProviderManager.get_instance()
        provider_data = ProviderInfo(
            id=provider_id,
            name=_string(model.get("providerName") or model.get("provider_name") or provider_id),
            base_url=self._openai_compatible_base_url(base_url),
            api_key=api_key,
            chat_model=_string(model.get("chatModel") or model.get("chat_model") or "OpenAIChatModel"),
            models=[ModelInfo(id=model_name, name=model_name)],
            is_custom=True,
            support_model_discovery=False,
            support_connection_check=False,
        )
        provider = manager._provider_from_data(provider_data.model_dump())
        manager.custom_providers[provider_id] = provider
        manager.save_provider_config(provider_id, provider)
        manager.active_model = model_slot_config_class(provider_id=provider_id, model=model_name)
        manager.save_active_model(manager.active_model)

    def _openai_compatible_base_url(self, base_url: str) -> str:
        value = base_url.rstrip("/")
        if value.endswith("/v1"):
            return value
        return f"{value}/v1"

    def _apply_mcp_servers(self, config: MemberRuntimeConfig) -> None:
        servers = self._mcporter_servers(config)
        legacy_path = self.config.default_workspace_dir / "mcporter-servers.json"
        if legacy_path.exists():
            legacy_path.unlink()

        path = self.config.default_workspace_dir / "config" / "mcporter.json"
        payload = {"mcpServers": servers}
        text = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def _apply_channel_policy(self, config: MemberRuntimeConfig) -> None:
        group_allow, dm_allow, group_deny, dm_deny = self._matrix_policy_ids(config)
        if not (group_allow or dm_allow or group_deny or dm_deny):
            return

        self_allow = _string(config.member.get("matrixUserId"))
        self_allowlist = [self_allow] if self_allow else []
        whitelist = self._dedupe(self_allowlist + group_allow + dm_allow)
        blacklist = self._dedupe(group_deny + dm_deny)
        deny_set = set(blacklist)
        whitelist = [value for value in whitelist if value not in deny_set]
        self._apply_matrix_channel_access_flags(
            group_enabled=bool(group_allow or group_deny),
            dm_enabled=bool(dm_allow or dm_deny),
        )
        self._write_matrix_access_control(whitelist, blacklist)

    def _apply_matrix_channel(self, config: MemberRuntimeConfig) -> None:
        desired = self._matrix_channel_desired_state(config)
        if desired is None:
            return
        try:
            from qwenpaw.config.config import load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_matrix_channel action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "channels", None) is None:
            try:
                from qwenpaw.config.config import ChannelConfig
            except ImportError:
                return
            agent_config.channels = ChannelConfig()
        matrix_config = getattr(agent_config.channels, "matrix", None)
        if matrix_config is None:
            return

        changed = False
        desired_fields = {
            "enabled": True,
            "homeserver": desired["homeserver"],
            "user_id": desired["user_id"],
            "access_token": desired["access_token"],
            "password": "",
            "encryption": _env_bool("AGENTTEAMS_MATRIX_E2EE"),
            "group_disabled": False,
            "dm_disabled": False,
            "filter_tool_messages": False,
            "filter_thinking": False,
        }
        for field, value in desired_fields.items():
            if getattr(matrix_config, field, None) != value:
                setattr(matrix_config, field, value)
                changed = True

        groups = dict(getattr(matrix_config, "groups", None) or {})
        if self._ensure_require_mention_group(groups, "*"):
            changed = True
        room_id = desired["room_id"]
        if room_id:
            if self._ensure_require_mention_group(groups, room_id):
                changed = True
        if getattr(matrix_config, "groups", None) != groups:
            matrix_config.groups = groups
            changed = True

        if changed:
            save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _apply_dingtalk_channel(self, config: MemberRuntimeConfig) -> None:
        desired = config.dingtalk_channel
        if desired is None:
            return
        try:
            from qwenpaw.config.config import load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_dingtalk_channel action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "channels", None) is None:
            try:
                from qwenpaw.config.config import ChannelConfig
            except ImportError:
                return
            agent_config.channels = ChannelConfig()
        dingtalk_config = getattr(agent_config.channels, "dingtalk", None)
        if dingtalk_config is None:
            return

        changed = False
        if not _bool(desired.get("enabled")):
            if getattr(dingtalk_config, "enabled", None) is not False:
                dingtalk_config.enabled = False
                changed = True
            if changed:
                save_agent_config(DEFAULT_AGENT_ID, agent_config)
            return

        streaming_enabled = _bool(desired.get("streaming_enabled"))
        client_id = _string(desired.get("client_id"))
        client_secret = _string(desired.get("client_secret"))
        robot_code = _string(desired.get("robot_code"))
        desired_fields = {
            "enabled": True,
            "client_id": client_id,
            "client_secret": client_secret,
            "robot_code": robot_code,
            "filter_thinking": _bool(desired.get("filter_thinking")),
            "filter_tool_messages": _bool(desired.get("filter_tool_messages")),
            "streaming_enabled": streaming_enabled,
        }
        if streaming_enabled:
            missing = [
                name
                for name, value in (
                    ("client_id", client_id),
                    ("client_secret", client_secret),
                    ("robot_code", robot_code),
                    ("card_template_id", _string(desired.get("card_template_id"))),
                )
                if not value
            ]
            if missing:
                raise ValueError(
                    "DingTalk streaming requires client_id, client_secret, "
                    "robot_code, and card_template_id. Create and publish the "
                    "streaming card template in DingTalk Open Platform, select "
                    "card mode, then set card_template_id; missing "
                    f"{', '.join(missing)}"
                )
            card_template_id = _string(desired.get("card_template_id"))
            previous_message_type = _string(getattr(dingtalk_config, "message_type", ""))
            previous_template_id = _string(getattr(dingtalk_config, "card_template_id", ""))
            if (
                previous_message_type == "card"
                and previous_template_id
                and previous_template_id != card_template_id
            ):
                logger.warning(
                    "DingTalk streaming enabled; current runtime card configuration will switch "
                    "component=update step=apply_dingtalk_channel previous_template=%s next_template=%s "
                    "existing_template_deleted=False",
                    previous_template_id,
                    card_template_id,
                )
            desired_fields.update(
                {
                    "message_type": "card",
                    "card_template_id": card_template_id,
                    "card_template_key": _string(
                        desired.get("card_template_key") or "content"
                    ),
                    "card_auto_layout": False,
                }
            )
        else:
            if "message_type" in desired:
                desired_fields["message_type"] = _string(desired.get("message_type") or "markdown")
            if "card_template_id" in desired:
                desired_fields["card_template_id"] = _string(desired.get("card_template_id"))
            if "card_template_key" in desired:
                desired_fields["card_template_key"] = _string(
                    desired.get("card_template_key") or "content"
                )
            if "card_auto_layout" in desired:
                desired_fields["card_auto_layout"] = _bool(desired.get("card_auto_layout"))
        for field, value in desired_fields.items():
            if getattr(dingtalk_config, field, None) != value:
                setattr(dingtalk_config, field, value)
                changed = True

        if changed:
            save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _matrix_channel_desired_state(self, config: MemberRuntimeConfig) -> Optional[Dict[str, str]]:
        homeserver = _string(
            os.getenv("AGENTTEAMS_MATRIX_URL")
            or os.getenv("AGENTTEAMS_MATRIX_SERVER")
            or os.getenv("AGENTTEAMS_MATRIX_HOMESERVER")
        ).rstrip("/")
        user_id = _string(config.member.get("matrixUserId") or os.getenv("AGENTTEAMS_MATRIX_USER_ID"))
        if not user_id:
            matrix_domain = _string(os.getenv("AGENTTEAMS_MATRIX_DOMAIN"))
            if config.member_name and matrix_domain:
                user_id = f"@{config.member_name}:{matrix_domain}"
        token_env = _string(config.credentials.get("matrixTokenEnv") or "AGENTTEAMS_WORKER_MATRIX_TOKEN")
        access_token = _string(os.getenv(token_env)) if token_env else ""
        if not access_token:
            access_token = _string(os.getenv("AGENTTEAMS_MATRIX_TOKEN"))
        room_id = _string(config.team.get("teamRoomId") or config.member.get("personalRoomId"))
        if not (homeserver and user_id and access_token):
            return None
        return {
            "homeserver": homeserver,
            "user_id": user_id,
            "access_token": access_token,
            "room_id": room_id,
        }

    def _ensure_require_mention_group(self, groups: Dict[str, Any], room_id: str) -> bool:
        room_cfg = dict(groups.get(room_id) or {})
        changed = False
        if room_cfg.pop("autoReply", None) is not None:
            changed = True
        if room_cfg.get("requireMention") is not True:
            room_cfg["requireMention"] = True
            changed = True
        if changed:
            groups[room_id] = room_cfg
        return changed

    def _matrix_policy_ids(self, config: MemberRuntimeConfig) -> Tuple[List[str], List[str], List[str], List[str]]:
        policy = config.channel_policy
        domain = _string(os.getenv("AGENTTEAMS_MATRIX_DOMAIN"))
        group_allow = self._default_group_allow(config, domain)
        dm_allow = list(group_allow)
        group_allow.extend(self._matrix_ids(_string_list(policy.get("groupAllowExtra")), domain))
        dm_allow.extend(self._matrix_ids(_string_list(policy.get("dmAllowExtra")), domain))
        group_deny = self._matrix_ids(_string_list(policy.get("groupDenyExtra")), domain)
        dm_deny = self._matrix_ids(_string_list(policy.get("dmDenyExtra")), domain)
        return (
            self._dedupe(group_allow),
            self._dedupe(dm_allow),
            self._dedupe(group_deny),
            self._dedupe(dm_deny),
        )

    def _default_group_allow(self, config: MemberRuntimeConfig, domain: str) -> List[str]:
        team_admin = _string(_section(config.team, "admin").get("matrixUserId"))
        system_admin_user = _string(os.getenv("AGENTTEAMS_ADMIN_USER") or "admin")
        system_admin = self._matrix_id(system_admin_user, domain)
        admin = team_admin or system_admin
        roster_allow = self._team_roster_group_allow(config, domain, admin)
        if roster_allow:
            # Ensure system admin is always present even when team admin differs
            if system_admin and system_admin not in roster_allow:
                roster_allow.append(system_admin)
            return roster_allow
        if config.team and config.member_role not in {"team_leader", "leader"}:
            leader = _string(config.team.get("leaderRuntimeName") or config.team.get("leaderName"))
            return [item for item in (self._matrix_id(leader, domain), admin, system_admin) if item]
        manager = self._matrix_id("manager", domain)
        return [item for item in (manager, admin, system_admin) if item]

    def _team_roster_group_allow(self, config: MemberRuntimeConfig, domain: str, admin: str) -> List[str]:
        members = config.team_members
        if not config.team or not members:
            return []

        current_names = {
            _string(config.member.get("name")),
            _string(config.member.get("runtimeName")),
        }
        current_names.discard("")
        current_mxid = _string(config.member.get("matrixUserId"))
        leader_roles = {"team_leader", "leader"}
        leader_ids: List[str] = []
        peer_ids: List[str] = []

        for member in members:
            mxid = self._member_matrix_id(member, domain)
            if not mxid:
                continue
            if mxid == current_mxid or _string(member.get("runtimeName") or member.get("name")) in current_names:
                continue
            if _string(member.get("role")) in leader_roles:
                leader_ids.append(mxid)
            else:
                peer_ids.append(mxid)

        if config.member_role in leader_roles:
            manager = self._matrix_id("manager", domain)
            return [item for item in (manager, admin, *peer_ids) if item]

        leader = _string(config.team.get("leaderRuntimeName") or config.team.get("leaderName"))
        if not leader_ids:
            leader_ids = [self._matrix_id(leader, domain)]
        return [item for item in (*leader_ids, admin, *peer_ids) if item]

    def _member_matrix_id(self, member: Dict[str, str], domain: str) -> str:
        mxid = _string(member.get("matrixUserId"))
        if mxid:
            return mxid
        return self._matrix_id(_string(member.get("runtimeName") or member.get("name")), domain)

    def _matrix_ids(self, values: List[str], domain: str) -> List[str]:
        return [mxid for mxid in (self._matrix_id(value, domain) for value in values) if mxid]

    def _matrix_id(self, value: str, domain: str) -> str:
        text = _string(value)
        if not text:
            return ""
        if text.startswith("@") or text.startswith("!"):
            return text
        return f"@{text}:{domain}" if domain else ""

    def _mcporter_servers(self, config: MemberRuntimeConfig) -> Dict[str, Any]:
        raw = config.mcp_servers
        gateway_key = self._gateway_key(config)
        if isinstance(raw, dict) and isinstance(raw.get("mcpServers"), dict):
            raw = raw["mcpServers"]

        servers: Dict[str, Any] = {}
        if isinstance(raw, list):
            for item in raw:
                if isinstance(item, dict):
                    name = _string(item.get("name"))
                    payload = self._mcporter_server_payload(item, gateway_key)
                    if name and payload:
                        servers[name] = payload
            return servers

        if isinstance(raw, dict):
            for name, item in raw.items():
                if isinstance(item, dict):
                    payload = self._mcporter_server_payload(item, gateway_key)
                    if _string(name) and payload:
                        servers[_string(name)] = payload
        return servers

    def _mcporter_server_payload(self, item: Dict[str, Any], gateway_key: str) -> Dict[str, Any]:
        url = _string(item.get("url"))
        if not url:
            return {}
        headers = item.get("headers")
        headers = dict(headers) if isinstance(headers, dict) else {}
        if gateway_key and "Authorization" not in headers:
            headers["Authorization"] = f"Bearer {gateway_key}"
        return {
            "url": url,
            "transport": _string(item.get("transport") or "http"),
            "headers": headers,
        }

    def _gateway_key(self, config: MemberRuntimeConfig) -> str:
        env_name = _string(config.credentials.get("gatewayKeyEnv") or "AGENTTEAMS_WORKER_GATEWAY_KEY")
        return os.getenv(env_name, "") if env_name else ""

    def _apply_matrix_channel_access_flags(self, group_enabled: bool, dm_enabled: bool) -> None:
        try:
            from qwenpaw.config.config import load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_channel_access_flags action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "channels", None) is None:
            try:
                from qwenpaw.config.config import ChannelConfig
            except ImportError:
                return
            agent_config.channels = ChannelConfig()
        matrix_config = getattr(agent_config.channels, "matrix", None)
        if matrix_config is None:
            return
        matrix_config.access_control_group = group_enabled
        matrix_config.access_control_dm = dm_enabled
        save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _write_matrix_access_control(self, whitelist: List[str], blacklist: List[str]) -> None:
        try:
            from qwenpaw.app.channels.access_control import get_access_control_store
        except ImportError:
            self._write_matrix_access_control_json(whitelist, blacklist)
            return

        store = get_access_control_store(self.config.default_workspace_dir)
        store.set_whitelist("matrix", whitelist)
        store.set_blacklist("matrix", blacklist)

    def _write_matrix_access_control_json(self, whitelist: List[str], blacklist: List[str]) -> None:
        path = self.config.default_workspace_dir / "access_control.json"
        existing: Dict[str, Any] = {}
        if path.exists():
            try:
                loaded = json.loads(path.read_text(encoding="utf-8"))
                existing = loaded if isinstance(loaded, dict) else {}
            except json.JSONDecodeError:
                existing = {}
        matrix = _section(existing, "matrix")
        pending = matrix.get("pending")
        existing["matrix"] = {
            "whitelist": {value: "" for value in whitelist},
            "blacklist": {value: "" for value in blacklist},
            "pending": pending if isinstance(pending, list) else [],
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(existing, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")

    def _dedupe(self, values: List[str]) -> List[str]:
        result = []
        seen = set()
        for value in values:
            if value not in seen:
                result.append(value)
                seen.add(value)
        return result

    async def loop(self) -> None:
        logger.info(
            "runtime config update loop started component=update worker=%s interval_seconds=%s",
            self.config.worker_name,
            self.config.runtime_config_poll_interval,
        )
        try:
            while True:
                await asyncio.sleep(self.config.runtime_config_poll_interval)
                try:
                    started_at = time.monotonic()
                    await asyncio.to_thread(self._load_and_apply_once)
                except asyncio.CancelledError:
                    raise
                except Exception as exc:
                    logger.warning(
                        "runtime config update failed component=update worker=%s error_type=%s duration_ms=%s",
                        self.config.worker_name,
                        type(exc).__name__,
                        _duration_ms(started_at),
                    )
        except asyncio.CancelledError:
            logger.info("runtime config update loop stopped component=update worker=%s", self.config.worker_name)
            raise
