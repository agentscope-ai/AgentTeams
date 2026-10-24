#!/usr/bin/env python3
"""Upgrade pre-flight checker for the QwenPaw runtime.

Thin CLI wrapper around the importable checker core
(``qwenpaw_worker.upgrade.checker``). Behaviour, arguments, and exit codes
are unchanged from the original standalone script:

- exit 0 = PASS (every check PASS/WARN/SKIP)
- exit 1 = FAIL (at least one check FAIL)
- exit 2 = usage / environment error

Usage:
    python3 check_qwenpaw_upgrade.py --target 2.3.0 \
        [--qwenpaw-dir /path/to/QwenPaw] [--wheel x.whl | --wheelhouse dir | --download] \
        [--check-deps] [--in-place] [--json]

The core logic (gate parsing, pin collection, patch applicability, deps
dry-run) lives in ``qwenpaw_worker/upgrade/checker.py`` so the in-image
orchestrator and the agentteams-upgrade plugin can import it directly.
This wrapper only handles: argument parsing, wheel resolution
(--wheel/--wheelhouse/--download), in-place pin demotion, and output.

Checks:

* C1 version gate - the expected version pinned in the worker wrapper
                    (``require_version(...)`` in ``worker.py``).
* C2 image pins   - ``ARG QWENPAW_PIP_SPEC`` defaults in ``Dockerfile`` and
                    ``../manager/Dockerfile.qwenpaw``, plus the ``qwenpaw``
                    dependency in ``pyproject.toml``.
* C3 compat patch - whether the build-time Driver-reload patch still applies
                    to the target version's ``qwenpaw/drivers/manager.py``
                    (extracted from a wheel).
* C4 dependencies - optional ``uv pip install --dry-run`` against the target
                    version (requires network; opt-in via --check-deps).

The checker never modifies the repository; it only reads files and (with
--download / --check-deps) performs network reads.
"""
from __future__ import annotations

import argparse
import json
import sys
import tempfile
from dataclasses import asdict
from pathlib import Path
from urllib.error import URLError
from urllib.request import Request, urlopen

# Import the checker core. The repo src/ is prepended so this script works
# from a bare checkout (host/dev) as well as from inside the image, where
# the overlayed qwenpaw_worker package already contains the upgrade/ subpackage.
_SRC = str(Path(__file__).resolve().parents[1] / "src")
if _SRC not in sys.path:
    sys.path.insert(0, _SRC)
from qwenpaw_worker.upgrade import checker as _checker  # noqa: E402

from packaging.version import InvalidVersion, Version  # noqa: E402

# Re-exported for the --download branch below (and for tests that patch
# ``check_qwenpaw_upgrade._http_json``); single shared implementation.
PYPI_JSON_URL = _checker.PYPI_JSON_URL
GATE_SOURCE_REL = _checker.GATE_SOURCE_REL
DOCKERFILE_REL = _checker.DOCKERFILE_REL
_http_json = _checker._http_json


def _resolve_wheel(target: str, args: argparse.Namespace, temp_dir: Path):
    """Resolve the wheel needed for the patch-applicability check.

    Returns (path, note, error). error=True fails the gate; a missing wheel
    without an explicit source is reported as NOT_RUN (offline-safe).
    """
    if args.wheel:
        path = Path(args.wheel)
        if path.is_file():
            return path, f"--wheel {path}", False
        return None, f"--wheel {path} not found", True
    if args.wheelhouse:
        house = Path(args.wheelhouse)
        matches = sorted(house.glob(f"qwenpaw-{target}-*.whl")) if house.is_dir() else []
        if matches:
            return matches[-1], f"--wheelhouse {matches[-1]}", False
        return None, f"no qwenpaw-{target}-*.whl in {house}", True
    if args.download:
        try:
            info = _http_json(PYPI_JSON_URL.format(version=target), timeout=120)
            wheel_url = None
            wheel_filename = None
            for item in info.get("urls", []):
                if item.get("packagetype") == "bdist_wheel":
                    wheel_url = item.get("url")
                    wheel_filename = item.get("filename")
            if not wheel_url or not wheel_filename:
                return None, f"no wheel published for qwenpaw {target}", True
            dest = temp_dir / wheel_filename
            req = Request(wheel_url, headers={"User-Agent": "agentteams-upgrade-check"})
            with urlopen(req, timeout=600) as resp, open(dest, "wb") as fh:
                while True:
                    chunk = resp.read(1024 * 1024)
                    if not chunk:
                        break
                    fh.write(chunk)
            return dest, f"--download {wheel_filename}", False
        except (OSError, URLError, ValueError) as exc:
            return None, f"--download failed: {exc}", True
    return None, "no wheel source (--wheel/--wheelhouse/--download); patch check skipped", False


def _render(checks: list, overall: str) -> str:
    width = 100
    lines = ["QwenPaw upgrade pre-flight", "=" * width, f"{'ID':<6}{'RESULT':<10}evidence / advice"]
    for check in checks:
        lines.append(f"{check.id:<6}{check.status:<10}{check.evidence}")
        if check.advice:
            lines.append(f"{'':<6}{'':<10}advice: {check.advice}")
    lines.append("=" * width)
    lines.append(f"OVERALL: {overall}")
    return "\n".join(lines)


def parse_args(argv=None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Check whether the QwenPaw runtime can be upgraded.")
    parser.add_argument("--target", required=True, help="Target qwenpaw version, e.g. 2.3.0")
    parser.add_argument(
        "--qwenpaw-dir",
        default=str(Path(__file__).resolve().parents[1]),
        help="Path to the QwenPaw repository checkout (default: sibling of this script)",
    )
    wheel = parser.add_mutually_exclusive_group()
    wheel.add_argument("--wheel", help="Path to a qwenpaw wheel for the target version")
    wheel.add_argument("--wheelhouse", help="Directory to search for qwenpaw-<target>-*.whl")
    wheel.add_argument("--download", action="store_true", help="Download the wheel from PyPI for this check")
    parser.add_argument("--check-deps", action="store_true", help="Also dry-run install the target into a throwaway venv (slow)")
    parser.add_argument("--in-place", action="store_true",
                        help="Image pin drift is WARN, not FAIL (in-place upgrade does not rebuild the image)")
    parser.add_argument("--json", action="store_true", help="Emit JSON")
    return parser.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv)
    qwenpaw_dir = Path(args.qwenpaw_dir)
    # --qwenpaw-dir points at the QwenPaw repo checkout when it carries the
    # repo layout; otherwise (the in-image / agentteams worker context) the
    # C1-C3 gates resolve against the installed qwenpaw_worker package
    # (run_preflight selects that with qwenpaw_dir=None).
    has_repo_layout = (qwenpaw_dir / GATE_SOURCE_REL).is_file() or (
        qwenpaw_dir / DOCKERFILE_REL
    ).is_file()
    repo_dir = qwenpaw_dir if has_repo_layout else None
    if repo_dir is None:
        print(
            f"note: --qwenpaw-dir {qwenpaw_dir} is not a QwenPaw repo checkout; "
            f"using the installed qwenpaw_worker package (in-image context)",
            file=sys.stderr,
        )
    try:
        Version(args.target)
    except InvalidVersion:
        print(f"error: invalid --target version: {args.target}", file=sys.stderr)
        return 2

    with tempfile.TemporaryDirectory(prefix="qwenpaw-upgrade-check-") as temp_dir:
        wheel, wheel_note, wheel_error = _resolve_wheel(args.target, args, Path(temp_dir))
        checks, meta = _checker.run_preflight(
            target=args.target,
            qwenpaw_dir=repo_dir,
            wheel=wheel,
            wheel_note=wheel_note,
            wheel_error=wheel_error,
            check_deps_flag=args.check_deps,
            in_place=args.in_place,
        )

    if args.json:
        payload = {
            "target": args.target,
            "qwenpaw_dir": str(qwenpaw_dir),
            "overall": meta["overall"],
            "exit_code": meta["exit_code"],
            "checks": [asdict(check) for check in checks],
        }
        print(json.dumps(payload, indent=2))
    else:
        print(_render(checks, meta["overall"]))
    return meta["exit_code"]


if __name__ == "__main__":
    sys.exit(main())
