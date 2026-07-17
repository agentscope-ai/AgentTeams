"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import logging
import os
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request
import zipfile
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import parse_qs, urlparse

from qwenpaw_worker.update.constants import (
    PACKAGE_PROMPT_FILES,
    PACKAGE_RUNTIME_OWNED_CONFIG_FILES,
    TEAMS_PROMPT_FILE,
)
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _download_path_part, _string, _string_list

logger = logging.getLogger(__name__)

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

