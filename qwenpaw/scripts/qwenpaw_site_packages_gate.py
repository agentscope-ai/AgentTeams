#!/usr/bin/env python3
"""Version-gate and checksum helpers for QwenPaw site-packages surgery (Phase 12 Q12.4).

Fail the image build when the installed ``qwenpaw`` package drifts from the pinned
version or when upstream files we patch/overlay no longer match expected markers.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import shutil
import sys
from pathlib import Path
from typing import Any


EXPECTED_QWENPAW_VERSION = "1.1.11"
DEFAULT_MANIFEST = Path(__file__).with_name("qwenpaw_upstream_manifest.json")


def _site_packages_from_prefix(prefix: Path) -> Path:
    return prefix / "lib" / "python3.11" / "site-packages"


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def load_manifest(path: Path) -> dict[str, Any]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise RuntimeError(f"invalid manifest shape: {path}")
    return data


def assert_pinned_qwenpaw_version(*, expected: str = EXPECTED_QWENPAW_VERSION) -> str:
    try:
        installed = importlib.metadata.version("qwenpaw")
    except importlib.metadata.PackageNotFoundError as exc:
        raise RuntimeError("qwenpaw is not installed in the active environment") from exc
    if installed != expected:
        raise RuntimeError(
            f"qwenpaw version mismatch: expected {expected}, got {installed}",
        )
    return installed


def _assert_file_markers(
    site_packages: Path,
    rel_path: str,
    markers: list[str],
) -> None:
    target = site_packages / rel_path
    if not target.is_file():
        raise RuntimeError(f"expected upstream file missing: {target}")
    source = target.read_text(encoding="utf-8")
    missing = [marker for marker in markers if marker not in source]
    if missing:
        raise RuntimeError(
            f"upstream shape changed for {rel_path}; missing markers: {missing}",
        )


def assert_upstream_manifest(
    site_packages: Path,
    manifest_path: Path,
) -> None:
    manifest = load_manifest(manifest_path)
    expected_version = str(manifest.get("qwenpaw_version") or "").strip()
    if expected_version and expected_version != EXPECTED_QWENPAW_VERSION:
        raise RuntimeError(
            "manifest qwenpaw_version does not match gate constant "
            f"({expected_version} != {EXPECTED_QWENPAW_VERSION})",
        )

    files = manifest.get("files")
    if not isinstance(files, dict):
        raise RuntimeError("manifest.files must be an object")

    for rel_path, spec in files.items():
        if not isinstance(spec, dict):
            raise RuntimeError(f"invalid manifest entry for {rel_path}")
        markers = spec.get("required_markers") or []
        if markers:
            _assert_file_markers(site_packages, rel_path, list(markers))
        expected_sha = str(spec.get("sha256") or "").strip()
        if expected_sha:
            target = site_packages / rel_path
            actual = _sha256_file(target)
            if actual != expected_sha:
                raise RuntimeError(
                    f"checksum mismatch for {rel_path}: expected {expected_sha}, got {actual}",
                )


def write_checksums(site_packages: Path, manifest_path: Path) -> None:
    manifest = load_manifest(manifest_path)
    files = manifest.setdefault("files", {})
    if not isinstance(files, dict):
        raise RuntimeError("manifest.files must be an object")
    for rel_path, spec in files.items():
        if not isinstance(spec, dict):
            continue
        target = site_packages / rel_path
        if target.is_file():
            spec["sha256"] = _sha256_file(target)
    manifest_path.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def apply_matrix_overlay(overlay_dir: Path, site_packages: Path) -> None:
    source = overlay_dir / "channel.py"
    target = site_packages / "qwenpaw" / "app" / "channels" / "matrix" / "channel.py"
    if not source.is_file():
        raise FileNotFoundError(f"matrix overlay missing: {source}")
    if not target.parent.is_dir():
        raise FileNotFoundError(f"qwenpaw matrix channel dir missing: {target.parent}")
    shutil.copy2(source, target)
    pycache = target.parent / "__pycache__"
    if pycache.is_dir():
        shutil.rmtree(pycache)


def apply_worker_overlay(worker_src: Path, site_packages: Path) -> None:
    source_dir = worker_src / "qwenpaw_worker"
    target_dir = site_packages / "qwenpaw_worker"
    if not source_dir.is_dir():
        raise FileNotFoundError(f"qwenpaw_worker overlay missing: {source_dir}")
    if not target_dir.is_dir():
        raise FileNotFoundError(f"installed qwenpaw_worker missing: {target_dir}")
    for child in source_dir.iterdir():
        destination = target_dir / child.name
        if child.is_dir():
            if destination.exists():
                shutil.rmtree(destination)
            shutil.copytree(child, destination)
        else:
            shutil.copy2(child, destination)


def cmd_verify(args: argparse.Namespace) -> int:
    site_packages = Path(args.site_packages)
    assert_pinned_qwenpaw_version()
    assert_upstream_manifest(site_packages, Path(args.manifest))
    print(f"qwenpaw {EXPECTED_QWENPAW_VERSION} upstream manifest verified")
    return 0


def cmd_write_checksums(args: argparse.Namespace) -> int:
    site_packages = Path(args.site_packages)
    manifest_path = Path(args.manifest)
    assert_pinned_qwenpaw_version()
    write_checksums(site_packages, manifest_path)
    print(f"updated checksums in {manifest_path}")
    return 0


def cmd_apply_matrix_overlay(args: argparse.Namespace) -> int:
    site_packages = Path(args.site_packages)
    overlay_dir = Path(args.overlay_dir)
    assert_pinned_qwenpaw_version()
    assert_upstream_manifest(site_packages, Path(args.manifest))
    apply_matrix_overlay(overlay_dir, site_packages)
    print(f"applied matrix overlay from {overlay_dir}")
    return 0


def cmd_apply_worker_overlay(args: argparse.Namespace) -> int:
    site_packages = Path(args.site_packages)
    worker_src = Path(args.worker_src)
    assert_pinned_qwenpaw_version()
    apply_worker_overlay(worker_src, site_packages)
    print(f"applied qwenpaw_worker overlay from {worker_src}")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--site-packages",
        default=str(_site_packages_from_prefix(Path(sys.prefix))),
        help="site-packages directory for the qwenpaw venv",
    )
    parser.add_argument(
        "--manifest",
        default=str(DEFAULT_MANIFEST),
        help="upstream manifest JSON path",
    )

    sub = parser.add_subparsers(dest="command", required=True)

    verify = sub.add_parser("verify", help="assert pinned version and upstream markers/checksums")
    verify.set_defaults(func=cmd_verify)

    write = sub.add_parser(
        "write-checksums",
        help="refresh sha256 entries in the manifest (run inside pinned qwenpaw image)",
    )
    write.set_defaults(func=cmd_write_checksums)

    matrix = sub.add_parser("apply-matrix-overlay", help="copy AgentTeams matrix channel overlay")
    matrix.add_argument("overlay_dir", help="directory containing channel.py")
    matrix.set_defaults(func=cmd_apply_matrix_overlay)

    worker = sub.add_parser("apply-worker-overlay", help="copy qwenpaw_worker package overlay")
    worker.add_argument("worker_src", help="directory containing qwenpaw_worker/")
    worker.set_defaults(func=cmd_apply_worker_overlay)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main())
