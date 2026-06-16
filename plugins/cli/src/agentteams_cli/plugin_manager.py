"""Plugin package installer used by the `agentteams` CLI fallback."""

from __future__ import annotations

import hashlib
import os
import re
import shutil
import subprocess
import tarfile
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Optional, Tuple

from agentteams_cli.config_store import ConfigStore

PLUGIN_NAME_PATTERN = re.compile(r"^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$")


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _validate_plugin_name(name: str) -> None:
    if not PLUGIN_NAME_PATTERN.fullmatch(name):
        raise ValueError(
            "invalid plugin name: use 1-63 lowercase letters, digits, '-' or '_', "
            "starting and ending with a letter or digit"
        )


def _load_metadata(manifest_path: Path) -> Tuple[str, str, list[str]]:
    """Load only the manifest fields the CLI needs.

    The full plugin schema is validated by `plugins/scripts/validate-plugin.rb`.
    Keeping the CLI parser tiny avoids adding a PyYAML dependency for the fallback
    installer path.
    """
    if not manifest_path.exists():
        raise ValueError(f"missing plugin.yaml: {manifest_path}")

    metadata: Dict[str, str] = {}
    dependencies: list[str] = []
    section: Optional[str] = None

    for raw in manifest_path.read_text(encoding="utf-8").splitlines():
        line = raw.rstrip()
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if not line.startswith(" ") and stripped.endswith(":"):
            section = stripped[:-1]
            continue
        if section == "metadata" and line.startswith("  ") and ":" in stripped:
            key, _, value = stripped.partition(":")
            metadata[key.strip()] = value.strip().strip('"').strip("'")
            continue
        if section == "dependencies" and stripped.startswith("- "):
            dependencies.append(stripped[2:].strip())

    name = metadata.get("name", "")
    version = metadata.get("version", "")
    if not name:
        raise ValueError("metadata.name is required")
    _validate_plugin_name(name)
    if not version:
        raise ValueError("metadata.version is required")
    return name, version, dependencies


def _safe_extract_tar(package: Path, target: Path) -> None:
    try:
        with tarfile.open(package, "r:gz") as archive:
            root = target.resolve()
            for member in archive.getmembers():
                member_path = (target / member.name).resolve()
                if not str(member_path).startswith(str(root) + os.sep) and member_path != root:
                    raise ValueError(f"unsafe tar member: {member.name}")
                if member.name.startswith("/") or ".." in Path(member.name).parts:
                    raise ValueError(f"unsafe tar member: {member.name}")
            archive.extractall(target)
    except tarfile.TarError as exc:
        raise ValueError(f"invalid tar package: {exc}") from exc


def _find_plugin_root(search_root: Path) -> Path:
    if (search_root / "plugin.yaml").is_file():
        return search_root
    candidates = [
        path
        for path in search_root.iterdir()
        if path.is_dir() and (path / "plugin.yaml").is_file()
    ]
    if len(candidates) == 1:
        return candidates[0]
    raise ValueError(f"plugin.yaml not found under {search_root}")


def _copytree(src: Path, dst: Path) -> None:
    if dst.exists():
        shutil.rmtree(dst)
    shutil.copytree(
        src,
        dst,
        ignore=shutil.ignore_patterns("__pycache__", ".DS_Store", "*.pyc"),
    )


def _hash_path(path: Path) -> str:
    digest = hashlib.sha256()
    if path.is_file():
        digest.update(path.read_bytes())
    else:
        for file_path in sorted(p for p in path.rglob("*") if p.is_file()):
            digest.update(str(file_path.relative_to(path)).encode("utf-8"))
            digest.update(b"\0")
            digest.update(file_path.read_bytes())
    return "sha256:" + digest.hexdigest()


def _script_env(store: ConfigStore, name: str, content_dir: Path) -> dict[str, str]:
    env = dict(os.environ)
    env.setdefault("AGENTTEAMS_PROJECT_DIR", str(store.project_dir))
    env.setdefault("AGENTTEAMS_PLUGIN_NAME", name)
    env.setdefault("AGENTTEAMS_PLUGIN_DIR", str(content_dir))
    env.setdefault("PILOT_DATA_DIR", str(store.root))
    env.setdefault("PILOT_LOG_DIR", str(store.root / "logs" / name))
    env.setdefault("PILOT_NODE_BIN", shutil.which("node") or "")
    env.setdefault("PILOT_NPM_BIN", shutil.which("npm") or "")
    return env


def _run_lifecycle(store: ConfigStore, name: str, content_dir: Path, script_name: str) -> bool:
    script = content_dir / "scripts" / script_name
    if not script.exists():
        return script_name == "uninstall.sh"
    result = subprocess.run(
        ["bash", str(script)],
        cwd=store.project_dir,
        env=_script_env(store, name, content_dir),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode != 0:
        detail = (result.stderr or result.stdout).strip()
        print(f"ERROR: {script_name} failed for {name}: {detail}")
        return False
    return True


def _prepare_package(package: Path) -> Tuple[Path, Optional[tempfile.TemporaryDirectory[str]]]:
    if not package.exists():
        raise ValueError(f"package not found: {package}")
    if package.is_dir():
        return _find_plugin_root(package), None

    tmp = tempfile.TemporaryDirectory(prefix="agentteams-plugin-")
    tmp_root = Path(tmp.name)
    _safe_extract_tar(package, tmp_root)
    return _find_plugin_root(tmp_root), tmp


def install(
    store: ConfigStore,
    name: str,
    package: Optional[Path] = None,
    source: Optional[Path] = None,
) -> bool:
    if not package and not source:
        print("ERROR: Use --package or --source.")
        return False

    tmp: Optional[tempfile.TemporaryDirectory[str]] = None
    try:
        _validate_plugin_name(name)
        plugin_root, tmp = _prepare_package(package) if package else (_find_plugin_root(source), None)  # type: ignore[arg-type]
        manifest_name, version, dependencies = _load_metadata(plugin_root / "plugin.yaml")
        if manifest_name != name:
            print(f"ERROR: Plugin source metadata.name is '{manifest_name}', not requested plugin '{name}'.")
            return False

        plugin_dir = store.plugin_dir(name)
        content_dir = store.plugin_content_dir(name)
        old_content_dir = content_dir if content_dir.exists() else None
        if old_content_dir:
            if not _run_lifecycle(store, name, old_content_dir, "uninstall.sh"):
                return False

        plugin_dir.mkdir(parents=True, exist_ok=True)
        _copytree(plugin_root, content_dir)

        if not _run_lifecycle(store, name, content_dir, "install.sh"):
            return False

        content_dir_rel = content_dir.relative_to(store.project_dir)
        manifest: Dict[str, Any] = {
            "name": name,
            "version": version,
            "installed_at": _now(),
            "content_dir": str(content_dir_rel),
            "content_hash": _hash_path(content_dir),
            "dependencies": dependencies,
        }
        if package:
            manifest["package"] = str(package)
        if source:
            manifest["source"] = str(source)
        store.save_plugin_manifest(name, manifest)
        print(f"Installed {name} v{version}.")
        return True
    except Exception as exc:
        print(f"ERROR: Failed to install {name}: {exc}")
        return False
    finally:
        if tmp:
            tmp.cleanup()


def update(
    store: ConfigStore,
    name: str,
    package: Optional[Path] = None,
    source: Optional[Path] = None,
) -> bool:
    try:
        _validate_plugin_name(name)
    except ValueError as exc:
        print(f"ERROR: Failed to update {name}: {exc}")
        return False

    old = store.get_plugin_manifest(name)
    if not old:
        print(f"Plugin '{name}' is not installed. Use 'install' first.")
        return False
    if install(store, name, package=package, source=source):
        new = store.get_plugin_manifest(name) or {}
        print(f"Updated {name}: {old.get('version', '?')} -> {new.get('version', '?')}")
        return True
    return False


def uninstall(store: ConfigStore, name: str) -> bool:
    try:
        _validate_plugin_name(name)
    except ValueError as exc:
        print(f"ERROR: Failed to uninstall {name}: {exc}")
        return False

    manifest = store.get_plugin_manifest(name)
    if not manifest:
        print(f"Plugin '{name}' is not installed.")
        return False
    content_dir = store.project_dir / manifest.get("content_dir", store.plugin_content_dir(name))
    if not _run_lifecycle(store, name, content_dir, "uninstall.sh"):
        return False
    store.remove_plugin(name)
    print(f"Uninstalled {name}.")
    return True


def list_plugins(store: ConfigStore) -> list[Dict[str, Any]]:
    return store.list_plugins()
