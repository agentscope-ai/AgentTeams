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
BRIDGE_MANIFEST = PLUGINS_ROOT / "qwen-code-bridge" / "plugin.yaml"
SCHEMA_PATH = PLUGINS_ROOT / "schemas" / "plugin.schema.json"
CONTRACTS_DIR = PLUGINS_ROOT / "teamharness" / "contracts"
WORKFLOW_PATH = REPO_ROOT / ".github" / "workflows" / "build-plugin.yml"
LEGACY_WORKFLOW_PATH = REPO_ROOT / ".github" / "workflows" / "build-teamharness-plugin.yml"
SURFACE_BRANCH_REQUIRED = (
    "apiVersion",
    "kind",
    "metadata",
    "prompts",
    "skills",
    "mcp",
    "adapters",
    "package",
)
BUNDLE_BRANCH_REQUIRED = (
    "apiVersion",
    "kind",
    "metadata",
    "requires",
    "adapters",
    "package",
)

ENTRY_BRANCH_REQUIRED = (
    "apiVersion",
    "kind",
    "metadata",
    "runtime",
    "entry",
    "requires",
    "package",
)

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
        if "entry" in manifest:
            # entry-form (bridge) manifest pins the host runtime instead of
            # the TeamHarness triple
            value = requires.get("qwenpaw")
            assert value is not None and str(value).strip(), (
                f"{path}: requires.qwenpaw must be present and non-empty"
            )
            continue
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


def test_bridge_manifest_entry_form() -> None:
    """Golden: the bridge manifest passes entry-form validation."""
    manifest = _load(BRIDGE_MANIFEST)
    assert "entry" in manifest, "bridge manifest must be entry-form"
    assert str(manifest.get("runtime", "")).strip() == "qwenpaw"
    root = BRIDGE_MANIFEST.parent
    entry = manifest["entry"]
    assert isinstance(entry, dict), "entry must be a mapping"
    assert (root / str(entry["backend"])).is_file(), "entry.backend must exist"
    assert (root / str(entry["frontend"])).is_file(), "entry.frontend must exist"
    includes = manifest.get("package", {}).get("include")
    assert isinstance(includes, list) and includes
    for item in includes:
        assert (root / str(item)).exists(), f"package.include missing: {item}"
    assert str(manifest["requires"]["qwenpaw"]).strip(), (
        "requires.qwenpaw must be non-empty"
    )
    for surface in ("prompts", "skills", "mcp", "hooks"):
        assert surface not in manifest, (
            f"entry-form manifest must not declare {surface}"
        )


def _load_schema() -> dict:
    assert SCHEMA_PATH.is_file(), f"missing schema: {SCHEMA_PATH}"
    with SCHEMA_PATH.open(encoding="utf-8") as fh:
        schema = json.load(fh)
    assert isinstance(schema, dict), "plugin.schema.json must be a JSON object"
    return schema


def _entry_branch(schema: dict) -> dict:
    branches = schema.get("oneOf")
    assert isinstance(branches, list), (
        "plugin.schema.json must branch via a top-level oneOf"
    )
    for branch in branches:
        if branch.get("title") == "entry":
            return branch
    raise AssertionError("plugin.schema.json is missing the 'entry' oneOf branch")


def test_plugin_schema_has_surface_entry_and_bundle_branches() -> None:
    """The schema must accept the surface, entry and bundle manifest forms via
    a top-level oneOf, mirroring the branches in validate-plugin.rb."""
    schema = _load_schema()
    branches = schema.get("oneOf")
    assert isinstance(branches, list) and len(branches) == 3, (
        "plugin.schema.json must have exactly three oneOf branches, got: "
        f"{len(branches) if isinstance(branches, list) else type(branches)!r}"
    )
    by_title = {branch.get("title"): branch for branch in branches}
    surface = by_title.get("surface")
    entry = by_title.get("entry")
    bundle = by_title.get("bundle")
    assert surface is not None and entry is not None and bundle is not None, (
        "oneOf branches must be titled 'surface', 'entry' and 'bundle', got: "
        f"{sorted(str(t) for t in by_title)}"
    )
    assert tuple(surface.get("required", [])) == SURFACE_BRANCH_REQUIRED, (
        f"surface branch required keys drifted: {surface.get('required')!r}"
    )
    assert tuple(bundle.get("required", [])) == BUNDLE_BRANCH_REQUIRED, (
        "bundle oneOf branch required fields drifted: " + repr(bundle.get("required"))
    )
    assert tuple(entry.get("required", [])) == ENTRY_BRANCH_REQUIRED, (
        f"entry branch required keys drifted: {entry.get('required')!r}"
    )
    entry_props = entry.get("properties", {})
    assert tuple(entry_props.get("entry", {}).get("required", [])) == (
        "backend",
        "frontend",
    ), "entry branch: entry must require backend and frontend"
    requires_spec = entry_props.get("requires", {})
    assert "qwenpaw" in requires_spec.get("required", []), (
        "entry branch: requires must require qwenpaw"
    )
    qwenpaw_spec = requires_spec.get("properties", {}).get("qwenpaw", {})
    assert qwenpaw_spec.get("type") == "string" and qwenpaw_spec.get("minLength", 0) >= 1, (
        "entry branch: requires.qwenpaw must be a non-empty string"
    )
    include_spec = (
        entry_props.get("package", {}).get("properties", {}).get("include", {})
    )
    assert include_spec.get("type") == "array", (
        "entry branch: package.include must be declared as an array"
    )


def test_bridge_manifest_covers_entry_branch_keys() -> None:
    """The bridge manifest must carry every key the entry branch requires."""
    entry = _entry_branch(_load_schema())
    manifest = _load(BRIDGE_MANIFEST)
    missing = [key for key in entry.get("required", []) if key not in manifest]
    assert not missing, f"bridge manifest missing entry-branch keys: {missing}"
    for key in ("backend", "frontend"):
        value = manifest.get("entry", {}).get(key)
        assert isinstance(value, str) and value.strip(), (
            f"bridge manifest entry.{key} must be a non-empty string"
        )
    assert str(manifest["requires"]["qwenpaw"]).strip(), (
        "bridge manifest requires.qwenpaw must be non-empty"
    )
    assert isinstance(manifest.get("package", {}).get("include"), list), (
        "bridge manifest package.include must be an array"
    )


def test_bridge_manifest_validates_against_entry_schema() -> None:
    """Full JSON-Schema validation of the bridge manifest against the entry
    branch — only when jsonschema happens to be installed (no new dependency)."""
    jsonschema = pytest.importorskip("jsonschema")
    entry = _entry_branch(_load_schema())
    jsonschema.validate(instance=_load(BRIDGE_MANIFEST), schema=entry)


@pytest.mark.parametrize("path", _manifest_paths(), ids=lambda path: path.parent.name)
def test_manifest_matches_exactly_one_schema_form(path: Path) -> None:
    jsonschema = pytest.importorskip("jsonschema")
    jsonschema.Draft202012Validator(_load_schema()).validate(_load(path))


def test_teamharness_manifest_still_teamharness_form() -> None:
    """Golden: the TeamHarness manifest keeps its prompts/skills/mcp form."""
    manifest = _load(TEAMHARNESS_MANIFEST)
    assert "entry" not in manifest, "teamharness must stay entry-free"
    for surface in ("prompts", "skills", "mcp"):
        assert surface in manifest, f"teamharness must keep its {surface} surface"


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
