"""Guard the generic plugin release contract shared by all yaml-manifest plugins.

Asserts manifest metadata and the optional ``requires`` declaration for every
``plugins/*/plugin.yaml``, the TeamHarness compatibility pin (requires present,
7 stable tool names, contract JSON files), the workerflow opt-out, and the
release workflow triggers so a rename or repackage cannot silently drift the
wire surface.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

import pytest

yaml = pytest.importorskip("yaml")

REPO_ROOT = Path(__file__).resolve().parents[3]
PLUGINS_ROOT = REPO_ROOT / "plugins"
TEAMHARNESS_MANIFEST = PLUGINS_ROOT / "teamharness" / "plugin.yaml"
WORKERFLOW_MANIFEST = PLUGINS_ROOT / "workerflow" / "plugin.yaml"
CONTRACTS_DIR = PLUGINS_ROOT / "teamharness" / "contracts"
WORKFLOW_PATH = REPO_ROOT / ".github" / "workflows" / "build-plugin.yml"
LEGACY_WORKFLOW_PATH = REPO_ROOT / ".github" / "workflows" / "build-teamharness-plugin.yml"

EXPECTED_TOOLS = [
    "health",
    "message",
    "roomflow",
    "filesync",
    "artifact",
    "projectflow",
    "taskflow",
]
REQUIRED_KEYS = ("agentteams", "contracts", "tool_schema")


def _manifest_paths() -> list[Path]:
    paths = sorted(PLUGINS_ROOT.glob("*/plugin.yaml"))
    assert paths, f"no plugin manifests found under {PLUGINS_ROOT}"
    return paths


def _load(path: Path) -> dict:
    assert path.is_file(), f"missing manifest: {path}"
    with path.open(encoding="utf-8") as fh:
        data = yaml.safe_load(fh)
    assert isinstance(data, dict), f"{path} must parse to a mapping"
    return data


def test_every_manifest_has_semver_metadata() -> None:
    for path in _manifest_paths():
        metadata = _load(path).get("metadata")
        assert isinstance(metadata, dict), f"{path}: metadata must be a mapping"
        name = str(metadata.get("name", "")).strip()
        assert name, f"{path}: metadata.name is required"
        version = str(metadata.get("version", ""))
        assert re.fullmatch(r"\d+\.\d+\.\d+", version), (
            f"{path}: metadata.version must match semver, got: {version!r}"
        )


def test_requires_is_optional_and_wellformed() -> None:
    for path in _manifest_paths():
        manifest = _load(path)
        if "requires" not in manifest:
            continue
        requires = manifest["requires"]
        assert isinstance(requires, dict), f"{path}: requires must be a mapping"
        for key in REQUIRED_KEYS:
            value = requires.get(key)
            assert value is not None and str(value).strip(), (
                f"{path}: requires.{key} must be present and non-empty"
            )


def test_teamharness_declares_requires() -> None:
    manifest = _load(TEAMHARNESS_MANIFEST)
    requires = manifest.get("requires")
    assert isinstance(requires, dict), (
        "teamharness plugin.yaml must declare a top-level `requires` mapping"
    )
    for key in REQUIRED_KEYS:
        value = requires.get(key)
        assert value is not None and str(value).strip(), (
            f"teamharness requires.{key} must be present and non-empty"
        )


def test_workerflow_has_no_requires() -> None:
    manifest = _load(WORKERFLOW_MANIFEST)
    assert "requires" not in manifest, (
        "workerflow plugin.yaml declared a top-level `requires` — re-check "
        "its compatibility range before letting validation enforce it"
    )


def test_teamharness_mcp_tool_names_unchanged() -> None:
    manifest = _load(TEAMHARNESS_MANIFEST)
    servers = manifest.get("mcp", {}).get("servers", [])
    assert servers, "mcp.servers must not be empty"
    tools = servers[0].get("tools")
    assert tools == EXPECTED_TOOLS, (
        f"mcp.servers[0].tools drifted from the 7 stable names: {tools!r}"
    )


def test_teamharness_contract_json_files_parse() -> None:
    assert CONTRACTS_DIR.is_dir(), f"missing contracts dir: {CONTRACTS_DIR}"
    json_files = sorted(CONTRACTS_DIR.glob("*.json"))
    assert json_files, f"no contract json files found under {CONTRACTS_DIR}"
    for path in json_files:
        with path.open(encoding="utf-8") as fh:
            data = json.load(fh)
        assert isinstance(data, (dict, list)), (
            f"{path.name} must contain a JSON object or array"
        )


def test_release_workflow_triggers() -> None:
    assert WORKFLOW_PATH.is_file(), f"missing workflow: {WORKFLOW_PATH}"
    with WORKFLOW_PATH.open(encoding="utf-8") as fh:
        workflow = yaml.safe_load(fh)
    assert isinstance(workflow, dict), "workflow file must parse to a mapping"
    # PyYAML (YAML 1.1) parses the bare `on` key as boolean True.
    triggers = workflow.get("on", workflow.get(True))
    assert isinstance(triggers, dict), "workflow must declare `on` triggers"
    assert "workflow_dispatch" in triggers, (
        "workflow must stay manually dispatchable"
    )
    push = triggers.get("push")
    tags = push.get("tags") if isinstance(push, dict) else None
    assert tags and "*-v[0-9]*" in tags, (
        f"push.tags must include '*-v[0-9]*', got: {tags!r}"
    )


def test_legacy_teamharness_workflow_removed() -> None:
    assert not LEGACY_WORKFLOW_PATH.exists(), (
        f"{LEGACY_WORKFLOW_PATH.name} must be gone in favor of build-plugin.yml"
    )
