"""Drift test for the P0 compatibility copies shipped inside bundles.

Per ``plugins/agentteams-teamharness/DEDUPE.md`` the ``mcp/``, ``skills/``,
and ``contracts/`` trees in the teamharness bundle are whole-tree copies of
the live implementation under ``plugins/teamharness``. Per
``plugins/agentteams-matrix/DEDUPE.md`` the matrix bundle's QwenPaw adapter
(the ``agentteams_matrix`` package plus the ``plugin.py`` entry point under
``adapters/qwenpaw``) is a copy of the live implementation under
``plugins/agentteams-matrix-channel``. Re-sync is a byte-for-byte copy in
the same commit as the upstream change; hand edits of individual copy files
are not allowed. Any drift — a missing file, an extra file, or a single
differing byte — fails here with the first drifted file.

One sanctioned exception: since P9/PR1 M3 (#1468) the matrix bundle's
``channel.py`` is bundle-owned code (it delegates mention parsing, the
per-room history buffer and session-id derivation to the bundle's
``core/`` package instead of carrying its own copy), so its byte-compare
is exempted for the ``agentteams_matrix`` pair — the file's presence on
both sides is still checked.

Pure stdlib: no qwenpaw, ruby, or third-party dependencies.
"""

from pathlib import Path

PLUGINS_ROOT = Path(__file__).resolve().parents[3]
BUNDLE_DIR = PLUGINS_ROOT / "agentteams-teamharness"
LEGACY_DIR = PLUGINS_ROOT / "teamharness"
MATRIX_BUNDLE_DIR = PLUGINS_ROOT / "agentteams-matrix" / "adapters" / "qwenpaw"
MATRIX_LEGACY_DIR = PLUGINS_ROOT / "agentteams-matrix-channel"

DRIFT_DIRS = ("contracts", "mcp", "skills")

# (label, bundle path, legacy path, kind). ``kind`` is ``"dir"`` for a
# whole-tree copy and ``"file"`` for a single-file copy. The teamharness
# entries are derived from DRIFT_DIRS; the matrix entries pin the QwenPaw
# adapter copy.
COPY_PAIRS = tuple(
    (name, BUNDLE_DIR / name, LEGACY_DIR / name, "dir") for name in DRIFT_DIRS
) + (
    (
        "agentteams_matrix",
        MATRIX_BUNDLE_DIR / "agentteams_matrix",
        MATRIX_LEGACY_DIR / "agentteams_matrix",
        "dir",
    ),
    (
        "plugin.py",
        MATRIX_BUNDLE_DIR / "plugin.py",
        MATRIX_LEGACY_DIR / "plugin.py",
        "file",
    ),
)

# Build artifacts; never part of a sync.
EXCLUDED_DIRS = {"__pycache__", ".git"}
EXCLUDED_NAMES = {".DS_Store"}


def _copy_files(base: Path) -> dict[str, Path]:
    files = {}
    for path in sorted(base.rglob("*")):
        if not path.is_file():
            continue
        rel_parts = path.relative_to(base).parts
        if any(part in EXCLUDED_DIRS for part in rel_parts):
            continue
        if path.name in EXCLUDED_NAMES or path.name.endswith(".pyc"):
            continue
        files[path.relative_to(base).as_posix()] = path
    return files


def _assert_tree_copies_match(
    label: str,
    bundle_base: Path,
    legacy_base: Path,
    byte_exempt: tuple[str, ...] = (),
) -> None:
    assert bundle_base.is_dir(), f"missing bundle copy directory: {bundle_base}"
    assert legacy_base.is_dir(), f"missing legacy source directory: {legacy_base}"

    bundle_files = _copy_files(bundle_base)
    legacy_files = _copy_files(legacy_base)

    missing_in_bundle = sorted(set(legacy_files) - set(bundle_files))
    if missing_in_bundle:
        raise AssertionError(
            f"drift in {label}: first missing file (present in the legacy "
            f"source, absent from the bundle copy) is {label}/"
            f"{missing_in_bundle[0]}"
        )

    extra_in_bundle = sorted(set(bundle_files) - set(legacy_files))
    if extra_in_bundle:
        raise AssertionError(
            f"drift in {label}: first unexpected file in the bundle copy "
            f"(absent from the legacy source) is {label}/"
            f"{extra_in_bundle[0]}"
        )

    for rel in sorted(bundle_files):
        if rel in byte_exempt:
            # Bundle-owned file (present on both sides, checked above)
            # that is no longer a byte copy — see the module docstring.
            continue
        bundle_path = bundle_files[rel]
        legacy_path = legacy_files[rel]
        if bundle_path.read_bytes() != legacy_path.read_bytes():
            raise AssertionError(
                f"drift in {label}: first drifted file is {label}/{rel} "
                f"({bundle_path} differs from {legacy_path})"
            )


def _assert_file_copies_match(label: str, bundle_file: Path, legacy_file: Path) -> None:
    assert bundle_file.is_file(), f"missing bundle copy file: {bundle_file}"
    assert legacy_file.is_file(), f"missing legacy source file: {legacy_file}"

    if bundle_file.read_bytes() != legacy_file.read_bytes():
        raise AssertionError(
            f"drift in {label}: first drifted file is {label} "
            f"({bundle_file} differs from {legacy_file})"
        )


def test_compatibility_copies_match_legacy():
    # P9/PR1 M3 (#1468): the matrix bundle's channel.py delegates to the
    # bundle's core/ package and is bundle-owned, not a byte copy.
    byte_exempt = {
        "agentteams_matrix": ("channel.py",),
    }
    for label, bundle, legacy, kind in COPY_PAIRS:
        if kind == "dir":
            _assert_tree_copies_match(
                label, bundle, legacy, byte_exempt=byte_exempt.get(label, ())
            )
        else:
            _assert_file_copies_match(label, bundle, legacy)
