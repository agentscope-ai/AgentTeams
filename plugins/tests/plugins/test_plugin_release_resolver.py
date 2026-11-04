"""Executable release-resolver contract tests for #1455.

Exercises ``plugins/scripts/resolve-plugin-release.sh`` (tag trigger and
manual selection) and the version gate in
``plugins/scripts/validate-plugin.rb`` against minimal fixtures built under
a temp directory — no dependency on the real plugins tree.

Version policy under test (both sides must agree):
  * plain semver and pre-release semver are accepted (``0.1.0-rc.1``);
  * build metadata (``+xxx``) is rejected;
  * the tag version suffix must be exactly equal (string equality) to
    ``metadata.version``.
"""

from __future__ import annotations

import subprocess
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
RESOLVER = REPO_ROOT / "plugins" / "scripts" / "resolve-plugin-release.sh"
VALIDATOR = REPO_ROOT / "plugins" / "scripts" / "validate-plugin.rb"

# Minimal manifest for resolver tests: only metadata is read by the script.
MINIMAL_MANIFEST = """\
apiVersion: agentteams.agentteam/v1alpha1
kind: AgentTeamPlugin
metadata:
  name: {name}
  version: "{version}"
"""

# Full contract fixture for validate-plugin.rb, so a correct version lets
# the script run to its "ok:" line instead of failing on unrelated files.
FULL_MANIFEST = """\
apiVersion: agentteams.agentteam/v1alpha1
kind: AgentTeamPlugin
metadata:
  name: demo
  version: "{version}"
package:
  include:
    - plugin.yaml
    - prompts
prompts:
  team: prompts/team/TEAMS.md
  agent:
    main: prompts/agent/AGENT.md
  manager:
    main: prompts/manager/MANAGER.md
skills:
  default:
    - id: demo-skill
      path: skills/demo
mcp:
  servers:
    - id: demo-mcp
      args:
        - --port
        - "9000"
"""


def _write_manifest(root: Path, dir_name: str, name: str, version: str) -> Path:
    plugin = root / "plugins" / dir_name
    plugin.mkdir(parents=True, exist_ok=True)
    path = plugin / "plugin.yaml"
    path.write_text(MINIMAL_MANIFEST.format(name=name, version=version), encoding="utf-8")
    return path


def _build_full_plugin(root: Path, version: str) -> Path:
    plugin = root / "plugins" / "demo"
    for rel in (
        "prompts/team",
        "prompts/agent",
        "prompts/manager",
        "skills/demo",
        "scripts",
    ):
        (plugin / rel).mkdir(parents=True, exist_ok=True)
    # validate-plugin.rb derives plugins_root as manifest_dir.parent, so the
    # schema lookup is plugins/<...>/../schemas -> root/plugins/schemas.
    (root / "plugins" / "schemas").mkdir(parents=True, exist_ok=True)
    (root / "plugins" / "schemas" / "plugin.schema.json").write_text("{}", encoding="utf-8")
    (plugin / "plugin.yaml").write_text(
        FULL_MANIFEST.format(version=version), encoding="utf-8"
    )
    (plugin / "prompts" / "team" / "TEAMS.md").write_text("team\n", encoding="utf-8")
    (plugin / "prompts" / "agent" / "AGENT.md").write_text("agent\n", encoding="utf-8")
    (plugin / "prompts" / "manager" / "MANAGER.md").write_text("manager\n", encoding="utf-8")
    (plugin / "skills" / "demo" / "SKILL.md").write_text("---\nname: demo-skill\n---\n", encoding="utf-8")
    (plugin / "scripts" / "install.sh").write_text("#!/usr/bin/env bash\n", encoding="utf-8")
    (plugin / "scripts" / "uninstall.sh").write_text("#!/usr/bin/env bash\n", encoding="utf-8")
    return plugin / "plugin.yaml"


def _resolve(args: list[str], cwd: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["bash", str(RESOLVER), *args],
        cwd=str(cwd),
        capture_output=True,
        text=True,
        timeout=60,
    )


def _validate(manifest: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["ruby", str(VALIDATOR), str(manifest)],
        cwd=str(manifest.parents[1]),
        capture_output=True,
        text=True,
        timeout=60,
    )


def test_tag_version_equal_to_manifest_succeeds(tmp_path: Path) -> None:
    _write_manifest(tmp_path, "foo", "foo", "0.1.0")
    result = _resolve(["--tag", "foo-v0.1.0"], tmp_path)
    assert result.returncode == 0, result.stderr
    assert "manifest=plugins/foo/plugin.yaml" in result.stdout
    assert "name=foo" in result.stdout
    assert "version=0.1.0" in result.stdout


def test_tag_version_mismatch_fails_with_both_versions(tmp_path: Path) -> None:
    _write_manifest(tmp_path, "foo", "foo", "0.1.0")
    result = _resolve(["--tag", "foo-v9.9.9"], tmp_path)
    assert result.returncode == 1
    assert "9.9.9" in result.stderr
    assert "0.1.0" in result.stderr
    assert result.stdout == ""


def test_prerelease_exact_match_succeeds(tmp_path: Path) -> None:
    _write_manifest(tmp_path, "foo", "foo", "0.1.0-rc.1")
    result = _resolve(["--tag", "foo-v0.1.0-rc.1"], tmp_path)
    assert result.returncode == 0, result.stderr
    assert "version=0.1.0-rc.1" in result.stdout


def test_prerelease_mismatch_fails(tmp_path: Path) -> None:
    _write_manifest(tmp_path, "foo", "foo", "0.1.0-rc.1")
    result = _resolve(["--tag", "foo-v0.1.0-rc.2"], tmp_path)
    assert result.returncode == 1
    assert "0.1.0-rc.2" in result.stderr
    assert "0.1.0-rc.1" in result.stderr


def test_tag_with_build_metadata_fails(tmp_path: Path) -> None:
    _write_manifest(tmp_path, "foo", "foo", "0.1.0")
    result = _resolve(["--tag", "foo-v0.1.0+build.5"], tmp_path)
    assert result.returncode == 1
    assert "build metadata is not supported for plugin releases" in result.stderr


def test_manual_plugin_selection_succeeds(tmp_path: Path) -> None:
    _write_manifest(tmp_path, "foo", "foo", "0.1.0")
    result = _resolve(["--plugin", "plugins/foo"], tmp_path)
    assert result.returncode == 0, result.stderr
    assert "manifest=plugins/foo/plugin.yaml" in result.stdout
    assert "name=foo" in result.stdout
    assert "version=0.1.0" in result.stdout


def test_manual_plugin_selection_missing_dir_fails(tmp_path: Path) -> None:
    result = _resolve(["--plugin", "plugins/nope"], tmp_path)
    assert result.returncode == 1
    assert "ERROR" in result.stderr
    assert result.stdout == ""


def test_prefix_conflict_resolves_uniquely(tmp_path: Path) -> None:
    # foo and foo-bar share a name prefix; both at the same version, so a
    # silent wrong pick would be indistinguishable from success.
    _write_manifest(tmp_path, "foo", "foo", "1.0.0")
    _write_manifest(tmp_path, "foo-bar", "foo-bar", "1.0.0")
    result = _resolve(["--tag", "foo-bar-v1.0.0"], tmp_path)
    assert result.returncode == 0, result.stderr
    assert "manifest=plugins/foo-bar/plugin.yaml" in result.stdout
    assert "name=foo-bar" in result.stdout
    assert result.stdout.count("manifest=") == 1


def test_duplicate_manifest_names_list_candidates(tmp_path: Path) -> None:
    # Two manifests with the same metadata.name: the resolver must fail and
    # list both candidates instead of silently picking one.
    _write_manifest(tmp_path, "dup-a", "dup", "1.0.0")
    _write_manifest(tmp_path, "dup-b", "dup", "1.0.0")
    result = _resolve(["--tag", "dup-v1.0.0"], tmp_path)
    assert result.returncode == 1
    assert "plugins/dup-a/plugin.yaml" in result.stderr
    assert "plugins/dup-b/plugin.yaml" in result.stderr
    assert result.stdout == ""


def test_validator_accepts_prerelease_version(tmp_path: Path) -> None:
    manifest = _build_full_plugin(tmp_path, "0.1.0-rc.1")
    result = _validate(manifest)
    assert result.returncode == 0, result.stderr
    assert "ok: demo 0.1.0-rc.1" in result.stdout


def test_validator_rejects_build_metadata_version(tmp_path: Path) -> None:
    manifest = _build_full_plugin(tmp_path, "0.1.0+build.5")
    result = _validate(manifest)
    assert result.returncode == 1
    assert "build metadata is not supported for plugin releases" in result.stderr
