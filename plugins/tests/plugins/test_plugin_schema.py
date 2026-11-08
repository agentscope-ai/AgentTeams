"""Real Draft 2020-12 schema validation for plugin manifests.

validate-plugin.rb used to only parse the schema JSON file, so its success
proved nothing about schema compatibility. These tests run the actual
validation (plugins/scripts/validate_manifest.py — the same CLI the Ruby
validator shells out to, sharing the single form rule) against the real
manifests and mutated copies, so schema/manifest drift turns the suite red.
"""

from __future__ import annotations

import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

yaml = pytest.importorskip("yaml")

REPO_ROOT = Path(__file__).resolve().parents[3]
SCHEMAS_DIR = REPO_ROOT / "plugins" / "schemas"
VALIDATOR = REPO_ROOT / "plugins" / "scripts" / "validate_manifest.py"
MATRIX_MANIFEST = REPO_ROOT / "plugins" / "agentteams-matrix" / "plugin.yaml"
TEAMHARNESS_MANIFEST = REPO_ROOT / "plugins" / "agentteams-teamharness" / "plugin.yaml"
WORKERFLOW_MANIFEST = REPO_ROOT / "plugins" / "workerflow" / "plugin.yaml"


def _validate(manifest_path: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(VALIDATOR), str(manifest_path), str(SCHEMAS_DIR)],
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )


def _load_manifest(path: Path) -> dict:
    with path.open(encoding="utf-8") as fh:
        manifest = yaml.safe_load(fh)
    assert isinstance(manifest, dict), f"{path} must parse to a mapping"
    return manifest


def _validate_mutated(tmp_path: Path, manifest: dict) -> subprocess.CompletedProcess:
    mutated = tmp_path / "plugin.yaml"
    with mutated.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(manifest, fh)
    return _validate(mutated)


def test_matrix_bundle_manifest_passes_real_schema_validation() -> None:
    result = _validate(MATRIX_MANIFEST)
    assert result.returncode == 0, f"stdout: {result.stdout}\nstderr: {result.stderr}"
    assert "bundle.schema.json" in result.stdout


def test_teamharness_manifest_stays_valid() -> None:
    # Surface keys present but bundle form by the shared rule
    # (adapters + requires) -> bundle.schema.json, where prompts/skills/mcp
    # are optional.
    result = _validate(TEAMHARNESS_MANIFEST)
    assert result.returncode == 0, f"stdout: {result.stdout}\nstderr: {result.stderr}"
    assert "bundle.schema.json" in result.stdout


def test_matrix_without_adapters_fails_real_schema_validation(tmp_path: Path) -> None:
    manifest = _load_manifest(MATRIX_MANIFEST)
    del manifest["adapters"]
    result = _validate_mutated(tmp_path, manifest)
    assert result.returncode != 0
    combined = result.stdout + result.stderr
    # No adapters -> not bundle form -> the original contract applies.
    assert "plugin.schema.json" in combined, combined
    assert "prompts" in combined, combined


def test_teamharness_without_prompts_still_passes(tmp_path: Path) -> None:
    manifest = _load_manifest(TEAMHARNESS_MANIFEST)
    del manifest["prompts"]
    result = _validate_mutated(tmp_path, manifest)
    assert result.returncode == 0, f"stdout: {result.stdout}\nstderr: {result.stderr}"
    assert "bundle.schema.json" in result.stdout


def test_non_bundle_without_prompts_fails(tmp_path: Path) -> None:
    # workerflow has no `requires`, so it stays on the original
    # plugin.schema.json contract where prompts is mandatory.
    manifest = _load_manifest(WORKERFLOW_MANIFEST)
    assert "requires" not in manifest, "fixture premise: workerflow must be non-bundle form"
    del manifest["prompts"]
    result = _validate_mutated(tmp_path, manifest)
    assert result.returncode != 0
    combined = result.stdout + result.stderr
    assert "plugin.schema.json" in combined, combined
    assert "prompts" in combined, combined


def test_form_rule_selects_the_expected_schema() -> None:
    spec = importlib.util.spec_from_file_location("validate_manifest", VALIDATOR)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    assert module.schema_file_for(_load_manifest(MATRIX_MANIFEST)) == "bundle.schema.json"
    assert module.schema_file_for(_load_manifest(TEAMHARNESS_MANIFEST)) == "bundle.schema.json"
    assert module.schema_file_for(_load_manifest(WORKERFLOW_MANIFEST)) == "plugin.schema.json"
