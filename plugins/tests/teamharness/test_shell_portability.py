"""System-Bash (bash 3.2) portability tests for the TeamHarness lifecycle shell scripts.

The bundle is the portable distribution, so the package entrypoints and every
per-runtime adapter script must run on the system shell — on macOS that is
bash 3.2.57. ``bash -n`` is necessary but not sufficient: it cannot detect a
runtime builtin incompatibility such as ``declare -A`` (which exits 2 under
3.2 before any adapter runs). This module pins the contract two ways:

1. A static denylist scan of every lifecycle script (``scripts/*.sh`` plus
   ``adapters/*/{install,uninstall}.sh``) for the bash-4-only constructs, and
   of the ``#!/usr/bin/env bash`` shebang line.
2. A real-interpreter run: when a bash 3.2 interpreter is resolvable
   (``TEAMHARNESS_BASH32`` path, or ``bash3.2`` / ``bash-3.2`` on PATH — on
   macOS the system ``/bin/bash`` is 3.2.57, so macOS CI exercises this
   automatically), ``install.sh --runtime openclaw --dry-run`` and
   ``uninstall.sh --runtime openclaw --dry-run`` execute under it against a
   throwaway HOME and PATH stub (the style of test_package_lifecycle.py);
   otherwise the cases skip. The whole lifecycle suite is additionally
   parametrized over the same interpreter in test_package_lifecycle.py.
"""

from __future__ import annotations

import os
import re
import subprocess
from pathlib import Path

import pytest

from test_package_lifecycle import (
    BASH32_SKIP_MESSAGE,
    BUNDLE,
    INSTALL,
    REPO_ROOT,
    UNINSTALL,
    _clean_env,
    bash32_interpreter,
)

SCRIPTS = BUNDLE / "scripts"
ADAPTERS = BUNDLE / "adapters"
SHEBANG = "#!/usr/bin/env bash"

# The bash-4-only constructs the lifecycle scripts must not contain. Each
# entry is (human-readable name, line-level pattern); the failure message
# names file, line and construct so a reviewer sees *why* it failed.
DENYLIST = (
    (
        "associative arrays (declare/local/typeset -A)",
        re.compile(r"\b(?:declare|local|typeset)\s+-[A-Za-z]*A[A-Za-z]*\b"),
    ),
    (
        "mapfile/readarray",
        re.compile(r"\b(?:mapfile|readarray)\b"),
    ),
    (
        "case-modifying expansions (${var^^} ${var,,} ${var^} ${var,})",
        re.compile(r"\$\{[A-Za-z_][A-Za-z0-9_]*[\^,]"),
    ),
    (
        "[[ -v ]]",
        re.compile(r"\[\[\s+-v(?=\s|\])"),
    ),
    (
        "wait -n",
        re.compile(r"\bwait\s+-n\b"),
    ),
    (
        "&>>",
        re.compile(r"&>>"),
    ),
    (
        "|&",
        re.compile(r"\|\&"),
    ),
    (
        "coproc",
        re.compile(r"\bcoproc\b"),
    ),
    (
        "namerefs (declare/local/typeset -n)",
        re.compile(r"\b(?:declare|local|typeset)\s+-[A-Za-z]*n[A-Za-z]*\b"),
    ),
    (
        "shopt globstar",
        re.compile(r"\bshopt\b.*\bglobstar\b"),
    ),
    (
        "printf %( format",
        re.compile(r"\bprintf\b.*'%\("),
    ),
)


def lifecycle_scripts() -> list[Path]:
    """The portable shell surface: the package entrypoints plus every
    per-runtime adapter install/uninstall script."""
    scripts = sorted(SCRIPTS.glob("*.sh"))
    adapter_scripts = []
    for adapter in sorted(ADAPTERS.iterdir()):
        if not adapter.is_dir():
            continue
        for name in ("install.sh", "uninstall.sh"):
            path = adapter / name
            if path.is_file():
                adapter_scripts.append(path)
    return scripts + adapter_scripts


def test_lifecycle_script_inventory_is_complete():
    rels = {p.relative_to(BUNDLE).as_posix() for p in lifecycle_scripts()}
    expected = {
        "scripts/install.sh",
        "scripts/uninstall.sh",
    }
    for runtime in ("qwenpaw", "openclaw", "hermes"):
        expected.add(f"adapters/{runtime}/install.sh")
        expected.add(f"adapters/{runtime}/uninstall.sh")
    assert rels == expected


def test_lifecycle_scripts_use_env_bash_shebang():
    problems = []
    for script in lifecycle_scripts():
        lines = script.read_text(encoding="utf-8").splitlines()
        if not lines or lines[0] != SHEBANG:
            problems.append(
                f"{script.relative_to(REPO_ROOT).as_posix()}:1: expected shebang "
                f"{SHEBANG!r}, got {lines[0] if lines else '<empty file>'!r}"
            )
    assert not problems, "\n".join(problems)


def test_lifecycle_scripts_contain_no_bash4_only_constructs():
    violations = []
    for script in lifecycle_scripts():
        lines = script.read_text(encoding="utf-8").splitlines()
        for lineno, line in enumerate(lines, 1):
            for name, pattern in DENYLIST:
                if pattern.search(line):
                    violations.append(
                        f"{script.relative_to(REPO_ROOT).as_posix()}:{lineno}: "
                        f"bash-4-only construct {name}: {line.strip()}"
                    )
    assert not violations, "\n".join(violations)


def _bash32_env(tmp_path: Path, bash32: str) -> dict:
    """The lifecycle env with a PATH overlay that puts the 3.2 interpreter
    first as ``bash``, so the entrypoints' own ``bash <adapter>`` invocations
    also run under 3.2 (as on macOS, where /bin/bash is the only bash)."""
    env = _clean_env(tmp_path, stubs=("openclaw",))
    bin32 = tmp_path / "bin32"
    bin32.mkdir()
    os.symlink(os.path.abspath(bash32), str(bin32 / "bash"))
    env["PATH"] = f"{bin32}:{env['PATH']}"
    return env


def test_install_openclaw_dry_run_under_real_bash32(tmp_path):
    bash32 = bash32_interpreter()
    if bash32 is None:
        pytest.skip(BASH32_SKIP_MESSAGE)
    env = _bash32_env(tmp_path, bash32)
    result = subprocess.run(
        [bash32, str(INSTALL), "--runtime", "openclaw", "--dry-run"],
        capture_output=True,
        text=True,
        env=env,
        timeout=300,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def test_uninstall_openclaw_dry_run_under_real_bash32(tmp_path):
    bash32 = bash32_interpreter()
    if bash32 is None:
        pytest.skip(BASH32_SKIP_MESSAGE)
    env = _bash32_env(tmp_path, bash32)
    result = subprocess.run(
        [bash32, str(UNINSTALL), "--runtime", "openclaw", "--dry-run"],
        capture_output=True,
        text=True,
        env=env,
        timeout=300,
    )
    assert result.returncode == 0, result.stdout + result.stderr
