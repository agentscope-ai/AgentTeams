#!/usr/bin/env python3
"""Pre-flight compatibility check for upgrading the QwenPaw runtime of a worker image.

Companion tooling for the in-place upgrade discussion in agentscope-ai/AgentTeams#1341.

Answers one question *before* a runtime bump is attempted: "what breaks or must
change if the worker's QwenPaw runtime is moved to <target version>?"

Checks (offline when --wheel / --wheelhouse is provided):

* C1 version gate - the expected version pinned in the worker wrapper
                    (``require_version(...)`` in ``src/qwenpaw_worker/worker.py``).
* C2 image pins   - ``ARG QWENPAW_PIP_SPEC`` defaults in ``Dockerfile`` and
                    ``../manager/Dockerfile.qwenpaw``, plus the ``qwenpaw``
                    dependency in ``pyproject.toml``.
* C3 compat patch - whether the build-time Driver-reload patch still applies to
                    the target version's ``qwenpaw/drivers/manager.py``
                    (extracted from a wheel).
* C4 dependencies - optional ``uv pip install --dry-run`` against the target
                    version (requires network; opt-in via --check-deps).

Exit codes: 0 = no FAILs, 1 = at least one FAIL, 2 = usage/environment error.

The checker never modifies the repository; it only reads files and (with
--download / --check-deps) performs network reads.
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import zipfile
from dataclasses import dataclass
from pathlib import Path

PYPI_JSON_URL = "https://pypi.org/pypi/qwenpaw/{version}/json"
GATE_SOURCE_REL = "src/qwenpaw_worker/worker.py"
DOCKERFILE_REL = "Dockerfile"
MANAGER_DOCKERFILE_REL = "../manager/Dockerfile.qwenpaw"
PYPROJECT_REL = "pyproject.toml"
PATCH_SCRIPT_REL = "scripts/patch-qwenpaw-driver-policy-reload.py"
PINS_TEST_REL = "tests/test_runtime_dependencies.py"

# First line of the replacement block inserted by the Driver-reload patch.
PATCH_NEW_MARKER = "# Policy may change while the replacement handler initializes."

STATUS_PASS = "PASS"
STATUS_FAIL = "FAIL"
STATUS_WARN = "WARN"
STATUS_SKIP = "SKIP"


@dataclass
class Check:
    id: str
    name: str
    status: str
    evidence: str = ""
    advice: str = ""


def _read(path: Path) -> str | None:
    try:
        return path.read_text(encoding="utf-8")
    except OSError:
        return None


def _http_json(url: str, timeout: float = 60.0):
    req = urllib.request.Request(url, headers={"User-Agent": "qwenpaw-upgrade-preflight"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.load(resp)


def _tail(text: str, lines: int = 3) -> str:
    parts = [line for line in text.strip().splitlines() if line.strip()]
    return " | ".join(parts[-lines:])[:400]


# --------------------------------------------------------------------------- C1


def check_gate(qwenpaw_dir: Path, target: str) -> Check:
    source = qwenpaw_dir / GATE_SOURCE_REL
    text = _read(source)
    if text is None:
        return Check("C1", "version gate", STATUS_SKIP,
                     evidence=f"{GATE_SOURCE_REL} not found")
    match = re.search(r'require_version\s*(?:\(|,)\s*"([^"]+)"\s*\)', text)
    if not match:
        return Check("C1", "version gate", STATUS_WARN,
                     evidence=f'no require_version("...") literal found in {GATE_SOURCE_REL}',
                     advice="verify the gate manually - it may have been parameterized already")
    found = match.group(1)
    line_no = text[: match.start()].count("\n") + 1
    if found == target:
        return Check("C1", "version gate", STATUS_PASS,
                     evidence=f'require_version("{found}") matches target ({GATE_SOURCE_REL}:{line_no})')
    return Check("C1", "version gate", STATUS_FAIL,
                 evidence=f'gate expects "{found}" but target is "{target}" ({GATE_SOURCE_REL}:{line_no})',
                 advice=("update the expected version in the wrapper, then re-run; "
                         "the full bump still touches the pins reported by C2"))


# --------------------------------------------------------------------------- C2

_PIN_DOCKERFILE = re.compile(r"ARG\s+QWENPAW_PIP_SPEC=qwenpaw==(\S+)")
_PIN_PYPROJECT = re.compile(r'"qwenpaw==([^"]+)"')


def _collect_pins(qwenpaw_dir: Path) -> list[tuple[str, Path, str | None, int | None]]:
    pins: list[tuple[str, Path, str | None, int | None]] = []
    for label, rel, pattern in (
        ("qwenpaw/Dockerfile", DOCKERFILE_REL, _PIN_DOCKERFILE),
        ("manager/Dockerfile.qwenpaw", MANAGER_DOCKERFILE_REL, _PIN_DOCKERFILE),
        ("qwenpaw/pyproject.toml", PYPROJECT_REL, _PIN_PYPROJECT),
    ):
        path = (qwenpaw_dir / rel).resolve()
        text = _read(path)
        if text is None:
            pins.append((label, path, None, None))
            continue
        match = pattern.search(text)
        if match:
            line_no = text[: match.start()].count("\n") + 1
            pins.append((label, path, match.group(1), line_no))
        else:
            pins.append((label, path, None, None))
    return pins


def check_pins(qwenpaw_dir: Path, target: str) -> Check:
    pins = _collect_pins(qwenpaw_dir)
    rows: list[str] = []
    missing = 0
    mismatch = 0
    for label, path, value, line_no in pins:
        if value is None:
            missing += 1
            rows.append(f"{label}: pin NOT FOUND")
        elif value == target:
            rows.append(f"{label}: {value} (ok, line {line_no})")
        else:
            mismatch += 1
            rows.append(f"{label}: {value} (expected {target}, line {line_no})")
    evidence = "; ".join(rows)
    test_file = qwenpaw_dir / PINS_TEST_REL
    if test_file.is_file():
        rows.append(f"{PINS_TEST_REL}: asserts the pins above")
        evidence = "; ".join(rows)
    if mismatch or missing:
        return Check("C2", "image pins", STATUS_FAIL, evidence=evidence,
                     advice=("bump every location above (and the assertions in "
                             f"{PINS_TEST_REL}) for target {target}"))
    return Check("C2", "image pins", STATUS_PASS, evidence=evidence)


# --------------------------------------------------------------------------- C3


def _decode_escaped(raw: str) -> str | None:
    try:
        return raw.encode("utf-8").decode("unicode_escape")
    except UnicodeDecodeError:
        return None


def _find_closing_paren(text: str, open_index: int) -> int:
    """Index of the ')' matching the '(' at open_index (string-aware), else -1."""
    depth = 0
    pos = open_index
    in_string = False
    while pos < len(text):
        ch = text[pos]
        if in_string:
            if ch == "\\":
                pos += 2
                continue
            if ch == '"':
                in_string = False
        elif ch == '"':
            in_string = True
        elif ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return pos
        pos += 1
    return -1


def _parse_new_block(patch_text: str) -> str | None:
    """The patch's ``new = (...)`` block: string literals concatenated, or None."""
    marker = patch_text.find("new = (")
    if marker < 0:
        return None
    open_index = patch_text.index("(", marker)
    close_index = _find_closing_paren(patch_text, open_index)
    if close_index < 0:
        return None
    segment = patch_text[open_index + 1:close_index]
    parts: list[str] = []
    for match in re.finditer(r'"((?:[^"\\]|\\.)*)"', segment):
        decoded = _decode_escaped(match.group(1))
        if decoded is None:
            return None
        parts.append(decoded)
    return "".join(parts) if parts else None


def _parse_patch_contract(patch_text: str) -> tuple[str | None, str | None, str | None]:
    guard = None
    match = re.search(r'if package\.version != "([^"]+)":', patch_text)
    if match:
        guard = match.group(1)
    old_line = None
    match = re.search(r'old = "((?:[^"\\]|\\.)*)"', patch_text)
    if match:
        raw = match.group(1)
        old_line = _decode_escaped(raw)
        if old_line is None:
            old_line = raw
    new_block = _parse_new_block(patch_text)
    return guard, old_line, new_block


def _wheel_manager_source(wheel: Path) -> tuple[str | None, str | None]:
    """Return (manager.py text, wheel Version from METADATA)."""
    manager = None
    version = None
    with zipfile.ZipFile(wheel) as zf:
        for name in zf.namelist():
            if name.endswith("qwenpaw/drivers/manager.py"):
                manager = zf.read(name).decode("utf-8", errors="replace")
            if name.endswith(".dist-info/METADATA"):
                metadata = zf.read(name).decode("utf-8", errors="replace")
                match = re.search(r"^Version:\s*(\S+)", metadata, re.MULTILINE)
                if match:
                    version = match.group(1)
    return manager, version


def _resolve_wheel(target: str, args: argparse.Namespace,
                   temp_dir: Path) -> tuple[Path | None, str, bool]:
    """Resolve the wheel to inspect.

    Returns (path, note, error). ``error`` is True when the caller explicitly
    asked for a wheel (--wheel / --wheelhouse / --download) and that request
    failed; the check must then report FAIL instead of silently skipping.
    """
    if args.wheel:
        path = Path(args.wheel).resolve()
        if path.is_file():
            return path, f"--wheel {path}", False
        return None, f"--wheel {path} not found", True
    if args.wheelhouse:
        house = Path(args.wheelhouse).resolve()
        candidates = sorted(house.glob(f"qwenpaw-{target}-*.whl"))
        if candidates:
            return candidates[-1], f"--wheelhouse {house}", False
        return None, f"no qwenpaw-{target}-*.whl in {house}", True
    if args.download:
        try:
            data = _http_json(PYPI_JSON_URL.format(version=target))
        except Exception as exc:  # noqa: BLE001 - report, do not crash
            return None, f"--download failed: {exc}", True
        wheels = [item for item in (data.get("urls") or []) if item.get("packagetype") == "bdist_wheel"]
        pick = next((item for item in wheels if item["filename"].endswith("py3-none-any.whl")),
                    wheels[0] if wheels else None)
        if pick is None:
            return None, f"no wheel published for {target}", True
        dest = temp_dir / pick["filename"]
        try:
            req = urllib.request.Request(pick["url"], headers={"User-Agent": "qwenpaw-upgrade-preflight"})
            with urllib.request.urlopen(req, timeout=300) as resp, open(dest, "wb") as handle:
                shutil.copyfileobj(resp, handle)
        except Exception as exc:  # noqa: BLE001
            return None, f"--download failed: {exc}", True
        return dest, f"--download {dest.name}", False
    return None, "no wheel provided (use --wheel/--wheelhouse/--download)", False


def check_patch(qwenpaw_dir: Path, target: str, wheel: Path | None,
                wheel_note: str, wheel_error: bool = False) -> Check:
    patch_path = qwenpaw_dir / PATCH_SCRIPT_REL
    patch_text = _read(patch_path)
    if patch_text is None:
        return Check("C3", "compat patch", STATUS_SKIP,
                     evidence=f"{PATCH_SCRIPT_REL} not found")
    guard, old_line, new_block = _parse_patch_contract(patch_text)
    if wheel is None:
        if wheel_error:
            return Check("C3", "compat patch", STATUS_FAIL,
                         evidence=f"wheel unavailable: {wheel_note}",
                         advice="fix the wheel input or provide the target wheel via --wheel/--wheelhouse")
        return Check("C3", "compat patch", STATUS_SKIP,
                     evidence=f"wheel unavailable: {wheel_note}",
                     advice="provide --wheel/--wheelhouse (or --download) to check patch applicability offline")
    try:
        manager_source, wheel_version = _wheel_manager_source(wheel)
    except zipfile.BadZipFile as exc:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=f"cannot read {wheel}: {exc}")
    if manager_source is None:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=f"{wheel.name} does not contain qwenpaw/drivers/manager.py")
    if wheel_version is not None and wheel_version != target:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=f"wheel version {wheel_version} != target {target}")

    # Build-time patch contract: the script refuses to run for any other
    # qwenpaw version, so a stale guard breaks the build regardless of what
    # the target's manager.py looks like.
    if guard is not None and guard != target:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=(f'patch guard expects "{guard}" but target is "{target}" — '
                               f"the patch raises for every other version and cannot run as checked; "
                               f"wheel {wheel.name}"),
                     advice=(f"update the guard in {PATCH_SCRIPT_REL} when bumping to {target} and "
                             "re-verify the old/new patterns; retire the patch script if the "
                             "replacement block is already integrated upstream"))

    # The patch slices manager.py between two method signatures and raises
    # ValueError when either is missing — there is no whole-file fallback.
    start = manager_source.find("    async def reload_driver(")
    if start < 0:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=(f"the patch would raise ValueError (reload_driver boundary not found) "
                               f"in {wheel.name}"),
                     advice="the target manager.py no longer matches the patch layout; review the patch")
    end = manager_source.find("    async def refresh_driver(", start)
    if end < 0:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=(f"the patch would raise ValueError (refresh_driver boundary not found) "
                               f"in {wheel.name}"),
                     advice="the target manager.py no longer matches the patch layout; review the patch")
    method = manager_source[start:end]
    note = f"wheel {wheel.name}"

    if new_block is not None and new_block in method:
        return Check("C3", "compat patch", STATUS_PASS,
                     evidence=f"replacement already present (patch integrated upstream). {note}")
    if PATCH_NEW_MARKER in method:
        if new_block is not None:
            return Check("C3", "compat patch", STATUS_FAIL,
                         evidence=(f"marker present but the full replacement is not inside reload_driver - "
                                   f"patch partially applied or upstream block changed. {note}"),
                         advice="rebase/refresh the Driver-reload patch for the target version")
        return Check("C3", "compat patch", STATUS_WARN,
                     evidence=(f"replacement marker present in {wheel.name} but the full replacement "
                               f"block could not be parsed from {PATCH_SCRIPT_REL} - marker-level check "
                               f"only. {note}"),
                     advice="review the patch manually; the whole-block verification is unavailable")
    if old_line is None:
        return Check("C3", "compat patch", STATUS_WARN,
                     evidence=f"could not parse the old pattern from {PATCH_SCRIPT_REL}; {note}",
                     advice="review patch-qwenpaw-driver-policy-reload.py manually")
    count = method.count(old_line)
    if count == 1:
        if new_block is None:
            return Check("C3", "compat patch", STATUS_WARN,
                         evidence=(f"target pattern present once in reload_driver() but the full "
                                   f"replacement block could not be parsed from {PATCH_SCRIPT_REL} - "
                                   f"applicability check only. {note}"),
                         advice="review the patch manually; the whole-block verification is unavailable")
        return Check("C3", "compat patch", STATUS_PASS,
                     evidence=f"target pattern present once in reload_driver(); patch can be applied. {note}")
    if count == 0:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=(f"target pattern occurs 0x in reload_driver(); target pattern absent - "
                               f"the patch cannot be applied. {note}"),
                     advice="rebase/refresh the Driver-reload patch for the target version")
    return Check("C3", "compat patch", STATUS_FAIL,
                 evidence=(f"target pattern occurs {count}x in reload_driver(); ambiguous - "
                           f"the patch needs review. {note}"),
                 advice="rebase/refresh the Driver-reload patch for the target version")


# --------------------------------------------------------------------------- C4


def check_deps(target: str) -> Check:
    uv = shutil.which("uv")
    if uv:
        cmd = [uv, "pip", "install", "--dry-run", f"qwenpaw=={target}"]
        runner = "uv pip"
    else:
        cmd = [sys.executable, "-m", "pip", "install", "--dry-run", f"qwenpaw=={target}"]
        runner = "pip"
    try:
        proc = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
    except (OSError, subprocess.TimeoutExpired) as exc:
        return Check("C4", "dependencies", STATUS_WARN,
                     evidence=f"could not run {runner}: {exc}")
    if proc.returncode == 0:
        return Check("C4", "dependencies", STATUS_PASS,
                     evidence=f"{runner} dry-run resolved {target}: {_tail(proc.stdout)}")
    return Check("C4", "dependencies", STATUS_FAIL,
                 evidence=f"{runner} dry-run failed: {_tail(proc.stderr or proc.stdout)}",
                 advice="resolve the dependency conflict before bumping")


# --------------------------------------------------------------------------- main


def _render(checks: list[Check], meta: dict) -> str:
    width = max(len(check.id) + len(check.name) for check in checks) + 2
    lines = [f"qwenpaw upgrade pre-flight - target {meta['target']}", ""]
    for check in checks:
        head = f"{check.id} {check.name}"
        lines.append(f"{head:<{width}} {check.status:<5} {check.evidence}")
        if check.advice:
            lines.append(f"{' ':<{width}}       -> {check.advice}")
    lines.append("")
    lines.append(f"overall: {meta['overall']} (exit {meta['exit_code']})")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="check_qwenpaw_upgrade.py",
        description="Pre-flight compatibility check for a QwenPaw runtime upgrade (AgentTeams#1341).",
    )
    parser.add_argument("--target", required=True, help="target qwenpaw version, e.g. 2.2.2")
    parser.add_argument("--qwenpaw-dir", default=None,
                        help="path to the qwenpaw/ directory of the repository (default: two levels above this script)")
    parser.add_argument("--wheel", default=None, help="path to a local qwenpaw wheel for the target version")
    parser.add_argument("--wheelhouse", default=None, help="directory that contains qwenpaw-<target>-*.whl")
    parser.add_argument("--download", action="store_true", help="download the target wheel from PyPI (network)")
    parser.add_argument("--check-deps", action="store_true",
                        help="run `uv pip install --dry-run qwenpaw==<target>` (network)")
    parser.add_argument("--json", action="store_true", help="machine-readable output")
    args = parser.parse_args(argv)

    target = str(args.target).strip()
    if not target:
        print("error: --target is empty", file=sys.stderr)
        return 2

    qwenpaw_dir = Path(args.qwenpaw_dir).resolve() if args.qwenpaw_dir else Path(__file__).resolve().parents[1]
    if not (qwenpaw_dir / GATE_SOURCE_REL).is_file() and not (qwenpaw_dir / DOCKERFILE_REL).is_file():
        print(f"error: {qwenpaw_dir} does not look like the qwenpaw/ directory", file=sys.stderr)
        return 2

    checks: list[Check] = [
        check_gate(qwenpaw_dir, target),
        check_pins(qwenpaw_dir, target),
    ]
    with tempfile.TemporaryDirectory(prefix="qwenpaw-preflight-") as temp:
        wheel, wheel_note, wheel_error = _resolve_wheel(target, args, Path(temp))
        checks.append(check_patch(qwenpaw_dir, target, wheel, wheel_note, wheel_error))
        if args.check_deps:
            checks.append(check_deps(target))
        else:
            checks.append(Check("C4", "dependencies", STATUS_SKIP,
                                evidence="not requested (--check-deps)"))

    overall = STATUS_FAIL if any(check.status == STATUS_FAIL for check in checks) else STATUS_PASS
    exit_code = 1 if overall == STATUS_FAIL else 0
    meta = {"target": target, "qwenpaw_dir": str(qwenpaw_dir), "overall": overall, "exit_code": exit_code}

    if args.json:
        payload = dict(meta)
        payload["checks"] = [check.__dict__ for check in checks]
        print(json.dumps(payload, indent=2))
    else:
        print(_render(checks, meta))
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
