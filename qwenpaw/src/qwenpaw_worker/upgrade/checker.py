"""Pre-flight compatibility checks (C1-C4) for a QwenPaw runtime upgrade.

Core of ``qwenpaw/scripts/check_qwenpaw_upgrade.py`` — the CLI is now a thin
wrapper over this module so the agentteams-upgrade plugin and the worker
entrypoint can reuse the same gates without re-parsing the script.

Checks:

* C1 version gate - the expected version pinned in the worker wrapper
                    (``require_version(...)`` in ``src/qwenpaw_worker/worker.py``).
* C2 image pins   - ``ARG QWENPAW_PIP_SPEC`` defaults in ``Dockerfile`` and
                    ``../manager/Dockerfile.qwenpaw``, plus the ``qwenpaw``
                    dependency in ``pyproject.toml``.
* C3 compat patch - whether the build-time Driver-reload patch still applies to
                    the target version's ``qwenpaw/drivers/manager.py``
                    (extracted from a wheel).
* C4 dependencies - optional ``uv pip install --dry-run`` against the target
                    version (requires network; opt-in). When explicitly
                    requested, an unrunnable dry-run (missing tool, timeout,
                    network failure) is upgrade-blocking (FAIL).

Two execution contexts:

* **repo context** (``qwenpaw_dir`` given, or resolvable next to this package):
  all four checks run against the repository sources, exactly like the CLI.
* **in-image context** (``qwenpaw_dir=None`` and no repo checkout next to the
  installed package): C1 resolves the installed ``worker.py`` via
  ``qwenpaw_worker.__file__``; C2 reports SKIPPED (the pin files are build-time
  inputs that the runtime image does not carry); C3 resolves the patch script
  from the repo (dev) or ``/opt/agentteams/scripts/`` (image).

The checker never modifies anything; it only reads files and (with
``download_wheel`` / ``check_deps``) performs network reads.
"""
from __future__ import annotations

import json
import re
import shutil
import subprocess
import sys
import urllib.request
import zipfile
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

PYPI_JSON_URL = "https://pypi.org/pypi/qwenpaw/{version}/json"
GATE_SOURCE_REL = "src/qwenpaw_worker/worker.py"
DOCKERFILE_REL = "Dockerfile"
MANAGER_DOCKERFILE_REL = "../manager/Dockerfile.qwenpaw"
PYPROJECT_REL = "pyproject.toml"
PATCH_SCRIPT_REL = "scripts/patch-qwenpaw-driver-policy-reload.py"
PINS_TEST_REL = "tests/test_runtime_dependencies.py"

# Image-side location of the build-time patch script (kept in the image by the
# worker Dockerfile so in-place upgrades can re-apply it after a package swap).
PATCH_SCRIPT_IMAGE = "/opt/agentteams/scripts/patch-qwenpaw-driver-policy-reload.py"

# First line of the replacement block inserted by the Driver-reload patch.
PATCH_NEW_MARKER = "# Policy may change while the replacement handler initializes."

STATUS_PASS = "PASS"
STATUS_FAIL = "FAIL"
STATUS_WARN = "WARN"
STATUS_SKIP = "SKIP"
STATUS_NOT_RUN = "NOT_RUN"
STATUS_ERROR = "ERROR"


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


def _utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# --------------------------------------------------------------------------- context


def _repo_qwenpaw_dir() -> Path | None:
    """The repository ``qwenpaw/`` directory when this package is imported from
    a repo checkout (``<repo>/qwenpaw/src/qwenpaw_worker``); None in the image
    (site-packages) where the build-time pin files do not exist."""
    import qwenpaw_worker

    qwenpaw_dir = Path(qwenpaw_worker.__file__).resolve().parents[1]
    if (qwenpaw_dir / DOCKERFILE_REL).is_file() and (qwenpaw_dir / GATE_SOURCE_REL).is_file():
        return qwenpaw_dir
    return None


def _worker_source() -> tuple[str | None, str]:
    """(worker.py text, label) for the C1 gate in the current context."""
    import qwenpaw_worker

    worker_path = Path(qwenpaw_worker.__file__).resolve().parent / "worker.py"
    return _read(worker_path), "qwenpaw_worker/worker.py"


def _patch_script_path() -> Path | None:
    """The build-time patch script in the current context (repo or image)."""
    import qwenpaw_worker

    qwenpaw_dir = Path(qwenpaw_worker.__file__).resolve().parents[1]
    repo_candidate = qwenpaw_dir / PATCH_SCRIPT_REL
    if repo_candidate.is_file():
        return repo_candidate
    image_candidate = Path(PATCH_SCRIPT_IMAGE)
    if image_candidate.is_file():
        return image_candidate
    return None


# --------------------------------------------------------------------------- C1


def _version_at_least(target: str, minimum: str) -> bool:
    """True if ``target >= minimum``. Uses packaging (exact PEP440, pre-release
    aware) when available; otherwise a minimal numeric comparator that strips the
    pre-release suffix — sufficient for the min-version gate on minimal hosts that
    lack packaging (the pre-flight must not require the build toolchain)."""
    try:
        from packaging.version import Version

        return Version(target) >= Version(minimum)
    except Exception:
        pass

    def _key(v: str) -> list[int]:
        base = re.split(r"[a-zA-Z]+\d+", v, 1)[0]  # 2.2.2b4 -> 2.2.2
        parts: list[int] = []
        for piece in base.split("."):
            m = re.match(r"(\d+)", piece)
            parts.append(int(m.group(1)) if m else 0)
        return parts

    t, m = _key(target), _key(minimum)
    n = max(len(t), len(m))
    t += [0] * (n - len(t))
    m += [0] * (n - len(m))
    return t >= m


def _check_gate_source(text: str, source_label: str, target: str) -> Check:
    """C1 core: evaluate the ``require_version("...")`` gate in a worker source."""
    if text is None:
        return Check("C1", "version gate", STATUS_SKIP,
                     evidence=f"{source_label} not found")
    match = re.search(r'require_version\s*(?:\(|,)\s*"([^"]+)"\s*\)', text)
    if not match:
        return Check("C1", "version gate", STATUS_WARN,
                     evidence=f'no require_version("...") literal found in {source_label}',
                     advice="verify the gate manually - it may have been parameterized already")
    found = match.group(1)
    line_no = text[: match.start()].count("\n") + 1
    # The wrapper gate is a minimum version (require_version means ">= found"),
    # so the target passes as long as it is at or above the gate.
    ok = _version_at_least(target, found)
    if ok:
        return Check("C1", "version gate", STATUS_PASS,
                     evidence=(f'require_version("{found}") is a minimum gate; '
                               f'target {target} satisfies it ({source_label}:{line_no})'))
    return Check("C1", "version gate", STATUS_FAIL,
                 evidence=f'gate requires >= "{found}" but target is "{target}" ({source_label}:{line_no})',
                 advice=("the target is below the wrapper's minimum version; "
                         "a full version bump still touches the pins reported by C2"))


def check_gate(qwenpaw_dir: Path, target: str) -> Check:
    text = _read(qwenpaw_dir / GATE_SOURCE_REL)
    return _check_gate_source(text, GATE_SOURCE_REL, target)


def check_gate_resolved(target: str) -> Check:
    """C1 in the current context: the repo checkout when available, else the
    installed ``qwenpaw_worker`` package (in-image)."""
    qwenpaw_dir = _repo_qwenpaw_dir()
    if qwenpaw_dir is not None:
        return check_gate(qwenpaw_dir, target)
    text, label = _worker_source()
    return _check_gate_source(text, label, target)


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


def check_pins_resolved(target: str) -> Check:
    """C2 in the current context. The image pins are build-time inputs; in the
    runtime image they do not exist, so C2 is SKIPPED there (in-place semantics:
    the image keeps its pin, no rebuild happens)."""
    qwenpaw_dir = _repo_qwenpaw_dir()
    if qwenpaw_dir is not None:
        return check_pins(qwenpaw_dir, target)
    return Check("C2", "image pins", STATUS_SKIP,
                 evidence="repo pin files not present in image (in-place: informational)")


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


def _check_patch_core(patch_text: str | None, patch_label: str, target: str,
                      wheel: Path | None, wheel_note: str, wheel_error: bool) -> Check:
    """C3 core: evaluate the build-time patch contract against the wheel."""
    if patch_text is None:
        return Check("C3", "compat patch", STATUS_SKIP,
                     evidence=f"{patch_label} not found")
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

    # Locate reload_driver first so an upstream-integrated fix can be detected
    # before consulting the build-time patch's version guard. The patch slices
    # manager.py between two method signatures and raises ValueError when
    # either is missing — there is no whole-file fallback.
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

    # If the replacement block is already integrated upstream, the build-time
    # patch is obsolete for this target regardless of its version guard.
    if new_block is not None and new_block in method:
        return Check("C3", "compat patch", STATUS_PASS,
                     evidence=f"replacement already present (patch integrated upstream). {note}")

    # The patch is needed for this target, so its version guard must match:
    # the build-time script refuses to run for any other qwenpaw version, so a
    # stale guard breaks the build regardless of what the target looks like.
    if guard is not None and guard != target:
        return Check("C3", "compat patch", STATUS_FAIL,
                     evidence=(f'patch guard expects "{guard}" but target is "{target}" — '
                               f"the patch raises for every other version and cannot run as checked; "
                               f"wheel {wheel.name}"),
                     advice=(f"update the guard in {PATCH_SCRIPT_REL} when bumping to {target} and "
                             "re-verify the old/new patterns; retire the patch script if the "
                             "replacement block is already integrated upstream"))
    if PATCH_NEW_MARKER in method:
        if new_block is not None:
            return Check("C3", "compat patch", STATUS_FAIL,
                         evidence=(f"marker present but the full replacement is not inside reload_driver - "
                                   f"patch partially applied or upstream block changed. {note}"),
                         advice="rebase/refresh the Driver-reload patch for the target version")
        return Check("C3", "compat patch", STATUS_WARN,
                     evidence=(f"replacement marker present in {wheel.name} but the full replacement "
                               f"block could not be parsed from {patch_label} - marker-level check "
                               f"only. {note}"),
                     advice="review the patch manually; the whole-block verification is unavailable")
    if old_line is None:
        return Check("C3", "compat patch", STATUS_WARN,
                     evidence=f"could not parse the old pattern from {patch_label}; {note}",
                     advice="review patch-qwenpaw-driver-policy-reload.py manually")
    count = method.count(old_line)
    if count == 1:
        if new_block is None:
            return Check("C3", "compat patch", STATUS_WARN,
                         evidence=(f"target pattern present once in reload_driver() but the full "
                                   f"replacement block could not be parsed from {patch_label} - "
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


def check_patch(qwenpaw_dir: Path, target: str, wheel: Path | None,
                wheel_note: str, wheel_error: bool = False) -> Check:
    patch_text = _read(qwenpaw_dir / PATCH_SCRIPT_REL)
    return _check_patch_core(patch_text, PATCH_SCRIPT_REL, target, wheel, wheel_note, wheel_error)


def check_patch_resolved(target: str, wheel: Path | None,
                         wheel_note: str, wheel_error: bool = False) -> Check:
    """C3 in the current context (repo checkout or image-side patch script)."""
    path = _patch_script_path()
    patch_text = _read(path) if path is not None else None
    label = str(path) if path is not None else PATCH_SCRIPT_REL
    return _check_patch_core(patch_text, label, target, wheel, wheel_note, wheel_error)


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
        # --check-deps is explicitly opt-in: once requested, an unrunnable
        # dry-run is a failed required stage, not a warning (maintainer gate
        # detail, AgentTeams#1341, 2026-10-09).
        return Check("C4", "dependencies", STATUS_FAIL,
                     evidence=(f"explicitly requested dry-run could not run: "
                               f"{exc}"),
                     advice=("resolve the tooling/network failure and re-run; "
                             "do not upgrade while a requested pre-flight "
                             "stage is unverifiable"))
    if proc.returncode == 0:
        return Check("C4", "dependencies", STATUS_PASS,
                     evidence=f"{runner} dry-run resolved {target}: {_tail(proc.stdout)}")
    return Check("C4", "dependencies", STATUS_FAIL,
                 evidence=f"{runner} dry-run failed: {_tail(proc.stderr or proc.stdout)}",
                 advice="resolve the dependency conflict before bumping")


# --------------------------------------------------------------------------- aggregation


def run_preflight(target: str, qwenpaw_dir: Path | None = None,
                  wheel: Path | None = None, wheel_note: str = "",
                  wheel_error: bool = False, check_deps_flag: bool = False,
                  in_place: bool = False) -> tuple[list[Check], dict]:
    """Run C1-C4 and return ``(checks, meta)`` with ``meta = {target, overall,
    exit_code}`` (overall: FAIL when any check FAILs, else PASS; exit_code:
    1 on FAIL, 0 otherwise). ``qwenpaw_dir=None`` selects the in-image
    context (resolved via the installed ``qwenpaw_worker`` package)."""
    if qwenpaw_dir is not None:
        c1 = check_gate(qwenpaw_dir, target)
        c2 = check_pins(qwenpaw_dir, target)
        c3 = check_patch(qwenpaw_dir, target, wheel, wheel_note, wheel_error)
    else:
        c1 = check_gate_resolved(target)
        c2 = check_pins_resolved(target)
        c3 = check_patch_resolved(target, wheel, wheel_note, wheel_error)

    checks: list[Check] = [c1, c2, c3]
    if check_deps_flag:
        checks.append(check_deps(target))
    else:
        checks.append(Check("C4", "dependencies", STATUS_SKIP,
                            evidence="not requested (--check-deps)"))

    if in_place:
        # In-place runtime upgrades do not rebuild the worker image, so the C2
        # image pins are informational (the image keeps the previous pin); only
        # a full image rebuild requires them to match the target.
        for c in checks:
            if c.id == "C2" and c.status == STATUS_FAIL:
                c.status = STATUS_WARN
                c.evidence = c.evidence + " [in-place: informational, image not rebuilt]"
                c.advice = ("in-place upgrade keeps the current image; bump the pins above only "
                            "for a full image rebuild")

    overall = STATUS_FAIL if any(check.status == STATUS_FAIL for check in checks) else STATUS_PASS
    exit_code = 1 if overall == STATUS_FAIL else 0
    meta = {"target": target, "overall": overall, "exit_code": exit_code}
    return checks, meta


# --------------------------------------------------------------------------- wheel download


def download_wheel(target: str, wheelhouse: Path, index_url: str | None = None,
                   timeout: float = 300.0) -> Path:
    """Download the target wheel into ``wheelhouse`` (cache: an existing
    ``qwenpaw-<target>-*.whl`` is reused). Raises ``OSError`` on network or
    publication failure — callers translate that into their error reporting."""
    wheelhouse = Path(wheelhouse)
    wheelhouse.mkdir(parents=True, exist_ok=True)
    candidates = sorted(wheelhouse.glob(f"qwenpaw-{target}-*.whl"))
    if candidates:
        return candidates[-1]
    if index_url:
        url = f"{index_url.rstrip('/')}/qwenpaw/{target}/json"
    else:
        url = PYPI_JSON_URL.format(version=target)
    data = _http_json(url, timeout=timeout)
    wheels = [item for item in (data.get("urls") or []) if item.get("packagetype") == "bdist_wheel"]
    pick = next((item for item in wheels if item["filename"].endswith("py3-none-any.whl")),
                wheels[0] if wheels else None)
    if pick is None:
        raise OSError(f"no wheel published for {target}")
    dest = wheelhouse / pick["filename"]
    req = urllib.request.Request(pick["url"], headers={"User-Agent": "qwenpaw-upgrade-preflight"})
    with urllib.request.urlopen(req, timeout=timeout) as resp, open(dest, "wb") as handle:
        shutil.copyfileobj(resp, handle)
    return dest


# --------------------------------------------------------------------------- /upgrade/check


def _gate_dict(check: Check) -> dict:
    item: dict = {"status": check.status, "evidence": check.evidence}
    if check.advice:
        item["advice"] = check.advice
    return item


def run_check(verify: bool = False, include_prerelease: bool = False,
              index_url: str | None = None) -> dict:
    """``GET /upgrade/check`` implementation: current version + PyPI candidates
    + per-candidate gates (C1/C2 always; C3 only with ``verify=True``, using a
    persistent wheel cache under ``$QWENPAW_WORKING_DIR/upgrade/wheelhouse``).

    Network problems never raise: they come back in ``errors[]`` with
    ``candidates: []`` (the endpoint reports 200, per the design)."""
    import os

    from qwenpaw_worker.upgrade import versions

    index_json = index_url or versions.PYPI_JSON_URL_DEFAULT
    errors: list[str] = []
    current, current_source = versions.current_version()
    payload: dict = {
        "current": current,
        "current_source": current_source,
        "checked_at": _utc_now(),
        "index": index_json,
        "candidates": [],
        "errors": errors,
    }

    try:
        releases = versions.fetch_releases(index_json)
    except versions.UpgradeIndexError as exc:
        errors.append(f"index unreachable: {exc}")
        return payload
    except Exception as exc:  # noqa: BLE001 - report, do not 5xx
        errors.append(f"index error: {exc}")
        return payload

    try:
        candidate_versions = versions.list_candidate_versions(current, releases, include_prerelease)
    except Exception as exc:  # noqa: BLE001
        errors.append(f"candidate resolution failed: {exc}")
        return payload

    wheelhouse = Path(os.environ.get("QWENPAW_WORKING_DIR", ".")) / "upgrade" / "wheelhouse"
    recommended: str | None = None
    for version in candidate_versions:
        gates: dict[str, dict] = {
            "C1": _gate_dict(check_gate_resolved(version)),
            "C2": _gate_dict(check_pins_resolved(version)),
            "C3": {"status": STATUS_NOT_RUN,
                   "evidence": "pass verify=true to download wheel and check compat patch"},
            "C4": {"status": STATUS_SKIP, "evidence": "opt-in (check-deps)"},
        }
        if verify:
            try:
                wheel = download_wheel(version, wheelhouse, index_url=None)
                check = check_patch_resolved(version, wheel, f"wheel {wheel.name}", False)
            except Exception as exc:  # noqa: BLE001 - per-version C3=ERROR
                check = Check("C3", "compat patch", STATUS_ERROR,
                              evidence=f"wheel download failed: {exc}")
            gates["C3"] = _gate_dict(check)
        statuses = {g["status"] for g in gates.values()}
        upgradeable = not (STATUS_FAIL in statuses or STATUS_ERROR in statuses)
        if upgradeable:
            recommended = version  # ascending PEP440 order: the last one wins
        payload["candidates"].append({
            "version": version,
            "prerelease": versions.is_prerelease(version),
            "published_at": versions.release_published_at(releases.get(version) or []),
            "gates": gates,
            "upgradeable": upgradeable,
            "recommended": False,
        })
    for item in payload["candidates"]:
        if item["version"] == recommended:
            item["recommended"] = True
    return payload
