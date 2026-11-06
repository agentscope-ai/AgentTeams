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
MATRIX_MANIFEST = PLUGINS_ROOT / "agentteams-matrix" / "plugin.yaml"
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

# SemVer 2.0.0: pre-release (-alpha.1) and build metadata (+sha) are valid.
_SEMVER = r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?"


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
        assert re.fullmatch(_SEMVER, version), (
            f"{path}: metadata.version must match semver, got: {version!r}"
        )


def _manifest_version(path: Path) -> str:
    if path.suffix == ".json":
        data = json.loads(path.read_text(encoding="utf-8"))
    else:
        data = _load(path)
    return str(data.get("version") or (data.get("metadata") or {}).get("version") or "")


def test_adapter_manifests_share_the_bundle_version() -> None:
    """Every manifest that tracks the bundle carries the bundle's version.

    A pre-release bump that only touches the bundle leaves the adapters on the
    previous version — the packaged zip then advertises one version in its
    bundle manifest and another in its adapter manifests. The hermes *platform*
    plugins are deliberately out of scope: they version their own platform
    contract (``kind: platform`` + ``requires_hermes``), so the exemption is
    asserted rather than assumed.
    """
    tracked = 0
    for bundle in ("agentteams-matrix", "agentteams-teamharness"):
        bundle_dir = PLUGINS_ROOT / bundle
        bundle_version = _manifest_version(bundle_dir / "plugin.json")
        assert re.fullmatch(_SEMVER, bundle_version), (
            f"{bundle}: plugin.json version is not semver: {bundle_version!r}"
        )
        assert _manifest_version(bundle_dir / "plugin.yaml") == bundle_version, (
            f"{bundle}: plugin.yaml version differs from plugin.json"
        )
        for manifest in sorted(bundle_dir.glob("adapters/*/plugin.json")):
            assert _manifest_version(manifest) == bundle_version, (
                f"{manifest}: adapter version must match bundle {bundle_version}"
            )
            tracked += 1
        for manifest in sorted(bundle_dir.glob("adapters/*/openclaw.plugin.json")):
            assert _manifest_version(manifest) == bundle_version, (
                f"{manifest}: openclaw adapter version must match bundle "
                f"{bundle_version}"
            )
            tracked += 1
        for manifest in sorted(bundle_dir.glob("adapters/*/plugin.yaml")):
            data = _load(manifest)
            if data.get("kind") == "platform":
                assert str(data.get("requires_hermes", "")).startswith(">="), (
                    f"{manifest}: a platform plugin must pin requires_hermes"
                )
                continue
            assert _manifest_version(manifest) == bundle_version, (
                f"{manifest}: adapter version must match bundle {bundle_version}"
            )
            tracked += 1
    assert tracked >= 4, (
        "expected at least the four adapter manifests to be covered, "
        f"checked {tracked}"
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


def test_matrix_bundle_manifest_form() -> None:
    manifest = _load(MATRIX_MANIFEST)
    matrix_root = PLUGINS_ROOT / "agentteams-matrix"

    metadata = manifest.get("metadata")
    assert isinstance(metadata, dict), "agentteams-matrix manifest must have metadata"
    assert metadata.get("name") == "agentteams-matrix"
    plugin_json = json.loads((matrix_root / "plugin.json").read_text())
    assert metadata.get("version") == plugin_json.get("version"), (
        "agentteams-matrix plugin.yaml version must match plugin.json"
    )

    # Bundle form is detected by adapters present and surface keys absent.
    for surface_key in ("prompts", "skills", "mcp"):
        assert surface_key not in manifest, (
            f"agentteams-matrix manifest must stay bundle form, not surface form ({surface_key})"
        )

    requires = manifest.get("requires")
    assert isinstance(requires, dict), (
        "agentteams-matrix plugin.yaml must declare a top-level `requires` mapping"
    )
    for key in REQUIRED_KEYS:
        value = requires.get(key)
        assert value is not None and str(value).strip(), (
            f"agentteams-matrix requires.{key} must be present and non-empty"
        )

    adapters = manifest.get("adapters")
    assert isinstance(adapters, list), "agentteams-matrix adapters must be a list"
    expected = [
        ("qwenpaw", "adapters/qwenpaw"),
        ("openclaw", "adapters/openclaw"),
        ("hermes", "adapters/hermes"),
    ]
    assert [(adapter.get("id"), adapter.get("path")) for adapter in adapters] == expected
    for _adapter_id, path in expected:
        assert (matrix_root / path).is_dir(), f"missing adapter dir: {path}"
        assert (matrix_root / path / "README.md").is_file(), f"missing adapter README: {path}"

    assert (matrix_root / "scripts" / "install.sh").is_file()
    assert (matrix_root / "scripts" / "uninstall.sh").is_file()

    assert manifest.get("package", {}).get("include") == [
        "plugin.json",
        "contract.yaml",
        "core",
        "adapters",
        "scripts",
        "DEDUPE.md",
    ]


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
