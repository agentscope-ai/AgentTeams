from __future__ import annotations

import json
from pathlib import Path

import pytest

from qwenpaw_site_packages_gate import (
    EXPECTED_QWENPAW_VERSION,
    apply_matrix_overlay,
    assert_upstream_manifest,
    load_manifest,
    write_checksums,
)


ROOT = Path(__file__).resolve().parents[1]
MANIFEST = ROOT / "scripts" / "qwenpaw_upstream_manifest.json"


def test_manifest_pins_qwenpaw_version() -> None:
    manifest = load_manifest(MANIFEST)
    assert manifest["qwenpaw_version"] == EXPECTED_QWENPAW_VERSION


def test_manifest_lists_patch_targets() -> None:
    manifest = load_manifest(MANIFEST)
    files = manifest["files"]
    assert "qwenpaw/app/channels/matrix/channel.py" in files
    assert "qwenpaw/app/multi_agent_manager.py" in files
    assert "qwenpaw/app/workspace/service_factories.py" in files


def test_assert_upstream_manifest_accepts_matching_markers(tmp_path: Path) -> None:
    site_packages = tmp_path / "site-packages"
    matrix_dir = site_packages / "qwenpaw" / "app" / "channels" / "matrix"
    matrix_dir.mkdir(parents=True)
    (matrix_dir / "channel.py").write_text(
        "class MatrixChannel:\n    async def _sync_loop(self):\n        pass\n",
        encoding="utf-8",
    )
    manager_dir = site_packages / "qwenpaw" / "app"
    manager_dir.mkdir(parents=True, exist_ok=True)
    (manager_dir / "multi_agent_manager.py").write_text(
        "instance = Workspace(\n    workspace_dir=agent_ref.workspace_dir,\n)\n",
        encoding="utf-8",
    )
    workspace_dir = site_packages / "qwenpaw" / "app" / "workspace"
    workspace_dir.mkdir(parents=True)
    (workspace_dir / "service_factories.py").write_text(
        "mcp.init_from_config_background(ws._config.mcp)\n",
        encoding="utf-8",
    )

    assert_upstream_manifest(site_packages, MANIFEST)


def test_assert_upstream_manifest_rejects_missing_marker(tmp_path: Path) -> None:
    site_packages = tmp_path / "site-packages"
    matrix_dir = site_packages / "qwenpaw" / "app" / "channels" / "matrix"
    matrix_dir.mkdir(parents=True)
    (matrix_dir / "channel.py").write_text("class MatrixChannel:\n    pass\n", encoding="utf-8")

    with pytest.raises(RuntimeError, match="missing markers"):
        assert_upstream_manifest(site_packages, MANIFEST)


def test_apply_matrix_overlay_replaces_upstream_channel(tmp_path: Path) -> None:
    site_packages = tmp_path / "site-packages"
    matrix_dir = site_packages / "qwenpaw" / "app" / "channels" / "matrix"
    matrix_dir.mkdir(parents=True)
    upstream = matrix_dir / "channel.py"
    upstream.write_text("upstream\n", encoding="utf-8")

    overlay_dir = tmp_path / "overlay"
    overlay_dir.mkdir()
    overlay_text = "overlay channel\n"
    (overlay_dir / "channel.py").write_text(overlay_text, encoding="utf-8")

    apply_matrix_overlay(overlay_dir, site_packages)
    assert upstream.read_text(encoding="utf-8") == overlay_text


def test_write_checksums_updates_manifest(tmp_path: Path) -> None:
    site_packages = tmp_path / "site-packages"
    matrix_dir = site_packages / "qwenpaw" / "app" / "channels" / "matrix"
    matrix_dir.mkdir(parents=True)
    (matrix_dir / "channel.py").write_text("class MatrixChannel\n", encoding="utf-8")

    manifest_path = tmp_path / "manifest.json"
    manifest_path.write_text(
        json.dumps(
            {
                "qwenpaw_version": EXPECTED_QWENPAW_VERSION,
                "files": {
                    "qwenpaw/app/channels/matrix/channel.py": {
                        "required_markers": ["class MatrixChannel"],
                    },
                },
            },
        ),
        encoding="utf-8",
    )

    write_checksums(site_packages, manifest_path)
    updated = json.loads(manifest_path.read_text(encoding="utf-8"))
    assert updated["files"]["qwenpaw/app/channels/matrix/channel.py"]["sha256"]
