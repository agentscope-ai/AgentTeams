"""Package lifecycle entrypoint tests for the agentteams-teamharness bundle.

``scripts/install.sh`` and ``scripts/uninstall.sh`` are the documented default
installation path; these tests exercise them as black boxes against a
throwaway HOME and PATH-stubbed runtime binaries — no live runtime, no
network, no credentials (the style of test_bundle_adapters.py).

The seven entrypoint-level cases from the #1459 P1 fix: openclaw-only
install/uninstall, the qwenpaw success path, multi-runtime coexistence, the
no-runtime error, failure isolation, artifact-driven uninstall after the
runtime binary disappears, and --dry-run writing nothing. A final test runs
the ``bash -n`` syntax gate on both entrypoints (the container's shell
approval blocks direct ``bash`` invocations; the entrypoints are otherwise
exercised end-to-end by the cases above).

Interpreter contract: the entrypoints must run on the system shell (bash
3.2 on macOS). Every case is parametrized over the interpreter: the default
``bash`` plus a real bash 3.2 when one is resolvable via
``TEAMHARNESS_BASH32`` (a path) or ``bash3.2`` / ``bash-3.2`` on PATH, so a
macOS run (system ``/bin/bash`` is 3.2.57) or a Linux run with
``TEAMHARNESS_BASH32`` covers all seven cases on 3.2.
``test_shell_portability.py`` additionally pins the 3.2 contract with a
static denylist scan over every lifecycle script.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
BUNDLE = REPO_ROOT / "plugins" / "agentteams-teamharness"
INSTALL = BUNDLE / "scripts" / "install.sh"
UNINSTALL = BUNDLE / "scripts" / "uninstall.sh"
PLUGIN_ID = "agentteams-teamharness"
RUNTIME_BINARIES = ("qwenpaw", "openclaw", "hermes")
SKILL_COUNT = 10

BASH32_SKIP_MESSAGE = (
    "set TEAMHARNESS_BASH32 or install bash3.2; macOS system bash is the target"
)


def bash32_interpreter() -> str | None:
    """Resolve a real bash 3.2 interpreter, or None when none is available.

    Resolution order: the ``TEAMHARNESS_BASH32`` environment variable (an
    executable path; a set-but-invalid value is a configuration error, not
    "not found"), then ``bash3.2`` / ``bash-3.2`` on PATH. On macOS the
    system ``/bin/bash`` is 3.2.57, so macOS runs exercise the 3.2 path
    automatically.
    """
    env_path = os.environ.get("TEAMHARNESS_BASH32", "").strip()
    if env_path:
        if os.path.isfile(env_path) and os.access(env_path, os.X_OK):
            return env_path
        pytest.fail(f"TEAMHARNESS_BASH32={env_path!r} is not an executable file")
    for name in ("bash3.2", "bash-3.2"):
        found = shutil.which(name)
        if found:
            return found
    return None


def interpreter_candidates() -> list[tuple[str, str]]:
    """(interpreter, id) pairs for the lifecycle suite: the default ``bash``
    plus a real bash 3.2 when one is resolvable, so a macOS run (or a Linux
    run with TEAMHARNESS_BASH32) covers every case on 3.2."""
    candidates = [("bash", "bash")]
    bash32 = bash32_interpreter()
    if bash32:
        candidates.append((bash32, "bash32"))
    return candidates


INTERPRETERS = interpreter_candidates()
INTERPRETER_NAMES = [interpreter for interpreter, _ in INTERPRETERS]
INTERPRETER_IDS = [name for _, name in INTERPRETERS]

# Variables the adapters read; the entrypoints must see a controlled env.
SCRUBBED_VARS = (
    "OPENCLAW_CONFIG",
    "OPENCLAW_CONFIG_PATH",
    "OPENCLAW_WORKSPACE",
    "HERMES_HOME",
    "HERMES_PLUGIN_HOME",
    "QWENPAW_WORKING_DIR",
    "TEAMHARNESS_INSTALL_LOG",
    "AGENTTEAMS_BUNDLE_DIR",
    "AGENTTEAMS_PLUGIN_DIR",
    "TH_OC_ADAPTER_DIR",
    "TH_OC_BUNDLE_DIR",
    "TH_OC_CONFIG",
    "TH_OC_DRY_RUN",
    "TH_HM_BUNDLE_DIR",
    "TH_HM_CONFIG",
    "TH_HM_PLUGIN_HOME",
    "TH_HM_DRY_RUN",
)


def _clean_env(tmp_path: Path, stubs: tuple = (), extra: dict | None = None) -> dict:
    """Controlled env for a package run: temp HOME, controlled runtime PATH.

    PATH is a single overlay bin directory: symlinks to every program of the
    process PATH (first hit wins, as in PATH semantics) except the real
    qwenpaw/openclaw/hermes binaries, plus the requested stub executables.
    Dropping whole directories would also hide the python3 the adapters need
    (a runtime binary can share a directory with it), so the overlay is
    per-program. ``python3`` is a wrapper around the interpreter running the
    tests: a symlink would not do, because CPython finds a venv by its
    launch path, so the wrapper execs the venv python and the adapters'
    inline python (e.g. ``import yaml``) resolves to the same packages the
    tests use. Calling the helper again for the same tmp_path (a later phase
    of one test) refreshes the stubs: earlier stub files are removed, so a
    phase that "loses" a binary really loses it.
    """
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir(exist_ok=True)

    # Stub files are regular files named after a runtime binary; anything
    # from an earlier phase of this test that is not requested now is stale.
    for existing in bin_dir.iterdir():
        if (
            existing.is_file()
            and not existing.is_symlink()
            and existing.name in RUNTIME_BINARIES
            and existing.name not in stubs
        ):
            existing.unlink()

    for name in stubs:
        stub = bin_dir / name
        stub.write_text("#!/bin/sh\nexit 0\n")
        stub.chmod(stub.stat().st_mode | 0o111)

    linked = set(stubs)
    for existing in bin_dir.iterdir():
        linked.add(existing.name)
    for d in os.environ.get("PATH", "").split(os.pathsep):
        if not d:
            continue
        try:
            entries = sorted(os.listdir(d))
        except OSError:
            continue
        for name in entries:
            if name in RUNTIME_BINARIES or name in linked:
                continue
            src = os.path.join(d, name)
            if not os.path.isfile(src):
                continue
            os.symlink(src, bin_dir / name)
            linked.add(name)

    # Point python3 at the interpreter running the tests via a wrapper
    # script (the first PATH hit may be a system python without the
    # packages the adapters import; a symlink would drop a venv because
    # CPython resolves venvs by launch path).
    python3_shim = bin_dir / "python3"
    shim_text = '#!/bin/sh\nexec "%s" "$@"\n' % sys.executable
    if python3_shim.is_symlink() or not python3_shim.is_file() \
            or python3_shim.read_text(encoding="utf-8") != shim_text:
        if python3_shim.is_symlink() or python3_shim.exists():
            python3_shim.unlink()
        python3_shim.write_text(shim_text, encoding="utf-8")
    python3_shim.chmod(python3_shim.stat().st_mode | 0o111)

    env = {
        key: value
        for key, value in os.environ.items()
        if key not in SCRUBBED_VARS and key not in ("PATH", "HOME")
    }
    env["HOME"] = str(tmp_path / "home")
    env["PATH"] = str(bin_dir)
    if extra:
        env.update(extra)
    return env


def run_entrypoint(
    script: Path, *args: str, env: dict, bash_bin: str = "bash"
) -> subprocess.CompletedProcess:
    return subprocess.run(
        [bash_bin, str(script), *args],
        capture_output=True,
        text=True,
        env=env,
        timeout=300,
    )


def _qwenpaw_marker(home: Path) -> Path:
    return home / ".qwenpaw" / ".agentteams" / "teamharness" / "installed.json"


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_openclaw_only_install_and_uninstall(tmp_path, bash_bin):
    home = tmp_path / "home"
    config = home / ".openclaw" / "openclaw.json"
    env = _clean_env(tmp_path, extra={"OPENCLAW_CONFIG": str(config)})

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: installed" in result.stdout
    assert "qwenpaw: skipped (not detected)" in result.stdout
    assert "hermes: skipped (not detected)" in result.stdout

    data = json.loads(config.read_text(encoding="utf-8"))
    assert "teamharness" in data["mcp"]["servers"]
    staged_root = home / ".openclaw" / "plugins" / PLUGIN_ID
    assert staged_root.is_dir()
    manifest = json.loads((staged_root / "openclaw.plugin.json").read_text(encoding="utf-8"))
    assert len(manifest["skills"]) == SKILL_COUNT

    result = run_entrypoint(UNINSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: uninstalled" in result.stdout
    data = json.loads(config.read_text(encoding="utf-8"))
    assert "teamharness" not in data.get("mcp", {}).get("servers", {})
    assert not staged_root.exists()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_qwenpaw_only_success_path(tmp_path, bash_bin):
    home = tmp_path / "home"
    env = _clean_env(tmp_path, stubs=("qwenpaw",))

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "qwenpaw: installed" in result.stdout
    assert "openclaw: skipped (not detected)" in result.stdout
    assert "hermes: skipped (not detected)" in result.stdout

    marker = _qwenpaw_marker(home)
    assert marker.is_file()
    state = json.loads(marker.read_text(encoding="utf-8"))
    assert state["runtime"] == "qwenpaw"

    result = run_entrypoint(UNINSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "qwenpaw: uninstalled" in result.stdout
    assert not marker.exists()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_coexistence_installs_and_uninstalls_every_runtime(tmp_path, bash_bin):
    home = tmp_path / "home"
    oc_config = home / ".openclaw" / "openclaw.json"
    hermes_home = home / ".hermes"
    env = _clean_env(
        tmp_path,
        stubs=("qwenpaw",),
        extra={"OPENCLAW_CONFIG": str(oc_config), "HERMES_HOME": str(hermes_home)},
    )

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    for runtime in RUNTIME_BINARIES:
        assert f"{runtime}: installed" in result.stdout

    marker = _qwenpaw_marker(home)
    assert marker.is_file()
    assert "teamharness" in json.loads(oc_config.read_text(encoding="utf-8"))["mcp"]["servers"]
    staged_root = home / ".openclaw" / "plugins" / PLUGIN_ID
    assert staged_root.is_dir()
    hermes_config = yaml.safe_load((hermes_home / "config.yaml").read_text(encoding="utf-8"))
    assert "teamharness" in hermes_config["mcp_servers"]
    hermes_plugin = hermes_home / "plugins" / PLUGIN_ID
    assert hermes_plugin.is_dir()

    result = run_entrypoint(UNINSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    for runtime in RUNTIME_BINARIES:
        assert f"{runtime}: uninstalled" in result.stdout

    assert not marker.exists()
    data = json.loads(oc_config.read_text(encoding="utf-8"))
    assert "teamharness" not in data.get("mcp", {}).get("servers", {})
    assert not staged_root.exists()
    hermes_config = yaml.safe_load((hermes_home / "config.yaml").read_text(encoding="utf-8"))
    assert "teamharness" not in hermes_config.get("mcp_servers", {})
    assert not hermes_plugin.exists()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_no_runtime_detected_exits_one_and_lists_runtimes(tmp_path, bash_bin):
    env = _clean_env(tmp_path)

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 1
    combined = result.stdout + result.stderr
    for runtime in RUNTIME_BINARIES:
        assert runtime in combined
        assert f"{runtime}: skipped (not detected)" in result.stdout


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_openclaw_failure_isolates_from_qwenpaw(tmp_path, bash_bin):
    home = tmp_path / "home"
    # The "config file" is a directory: the adapter can stage the plugin but
    # cannot write the config, so its run fails.
    bad_config = home / ".openclaw" / "openclaw.json"
    bad_config.mkdir(parents=True)
    env = _clean_env(
        tmp_path, stubs=("qwenpaw",), extra={"OPENCLAW_CONFIG": str(bad_config)}
    )

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode != 0
    assert "qwenpaw: installed" in result.stdout
    assert "openclaw: failed (exit" in result.stdout
    assert "hermes: skipped (not detected)" in result.stdout
    # the failure summary names the failing runtime
    assert "openclaw" in result.stdout + result.stderr

    marker = _qwenpaw_marker(home)
    assert marker.is_file()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_uninstall_removes_residue_after_binary_disappears(tmp_path, bash_bin):
    home = tmp_path / "home"
    # Install while the runtime is visible (binary on PATH + env); the
    # config sits at the temp-HOME default path.
    env_install = _clean_env(
        tmp_path,
        stubs=("openclaw",),
        extra={"OPENCLAW_CONFIG": str(home / ".openclaw" / "openclaw.json")},
    )
    result = run_entrypoint(INSTALL, env=env_install, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr

    # The binary disappears and the env is unset; only the artifacts remain.
    env_uninstall = _clean_env(tmp_path)
    assert "OPENCLAW_CONFIG" not in env_uninstall
    result = run_entrypoint(UNINSTALL, env=env_uninstall, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: uninstalled" in result.stdout

    config = home / ".openclaw" / "openclaw.json"
    data = json.loads(config.read_text(encoding="utf-8"))
    assert "teamharness" not in data.get("mcp", {}).get("servers", {})
    assert not (home / ".openclaw" / "plugins" / PLUGIN_ID).exists()


def _worker_home(home: Path) -> tuple[Path, Path]:
    """The Worker's layout under a temp HOME: canonical $HOME/openclaw.json
    with $HOME/.openclaw/openclaw.json a symlink to it. Returns
    (canonical, link)."""
    (home / ".openclaw").mkdir(parents=True)
    canonical = home / "openclaw.json"
    canonical.write_text(json.dumps({"agent": {"displayName": "worker"}}) + "\n")
    link = home / ".openclaw" / "openclaw.json"
    link.symlink_to(canonical)
    return canonical, link


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_worker_layout_default_install_honours_effective_config(tmp_path, bash_bin):
    home = tmp_path / "home"
    canonical, link = _worker_home(home)
    env = _clean_env(tmp_path, extra={"OPENCLAW_CONFIG_PATH": str(canonical)})
    assert "OPENCLAW_CONFIG" not in env

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: installed" in result.stdout
    assert "qwenpaw: skipped (not detected)" in result.stdout
    assert "hermes: skipped (not detected)" in result.stdout

    # The canonical file the Worker actually reads gained the entry...
    canonical_data = json.loads(canonical.read_text(encoding="utf-8"))
    assert "teamharness" in canonical_data["mcp"]["servers"]
    # ...the default-path symlink survived and reads the same entry, and
    # the staged root resolves under the canonical config directory.
    assert link.is_symlink(), "install must not replace the config symlink"
    via_link = json.loads(link.read_text(encoding="utf-8"))
    assert (
        via_link["mcp"]["servers"]["teamharness"]
        == canonical_data["mcp"]["servers"]["teamharness"]
    )
    staged_root = canonical.parent / "plugins" / PLUGIN_ID
    assert staged_root.is_dir()
    assert str(staged_root) in canonical_data["plugins"]["load"]["paths"]


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_worker_layout_dry_run_writes_nothing_and_keeps_symlink(tmp_path, bash_bin):
    home = tmp_path / "home"
    canonical, link = _worker_home(home)
    before = canonical.read_bytes()
    env = _clean_env(tmp_path, extra={"OPENCLAW_CONFIG_PATH": str(canonical)})

    result = run_entrypoint(
        INSTALL, "--runtime", "openclaw", "--dry-run", env=env, bash_bin=bash_bin
    )
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: installed (dry-run)" in result.stdout

    assert canonical.read_bytes() == before
    assert link.is_symlink()
    assert not (canonical.parent / "plugins").exists()
    assert not (link.parent / "plugins").exists()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_worker_layout_default_uninstall_preserves_symlink(tmp_path, bash_bin):
    home = tmp_path / "home"
    canonical, link = _worker_home(home)
    env = _clean_env(tmp_path, extra={"OPENCLAW_CONFIG_PATH": str(canonical)})

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "teamharness" in json.loads(canonical.read_text(encoding="utf-8"))["mcp"]["servers"]
    staged_root = canonical.parent / "plugins" / PLUGIN_ID
    assert staged_root.is_dir()

    result = run_entrypoint(UNINSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: uninstalled" in result.stdout

    # The canonical side lost exactly this adapter's residue; the foreign
    # key and the symlink survive.
    data = json.loads(canonical.read_text(encoding="utf-8"))
    assert "teamharness" not in data.get("mcp", {}).get("servers", {})
    assert "agentteams-teamharness" not in data.get("plugins", {}).get("entries", {})
    assert data["agent"] == {"displayName": "worker"}
    assert not staged_root.exists()
    assert link.is_symlink()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_uninstall_finds_staged_root_next_to_dangling_config_link(tmp_path, bash_bin):
    """Artifact-driven detection with no runtime variables: the config
    symlink survives with its target gone (dangling) — the staged root is
    found next to the link's canonical target and removed."""
    home = tmp_path / "home"
    canonical, link = _worker_home(home)
    env = _clean_env(tmp_path, extra={"OPENCLAW_CONFIG_PATH": str(canonical)})

    result = run_entrypoint(INSTALL, env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    staged_root = canonical.parent / "plugins" / PLUGIN_ID
    assert staged_root.is_dir()

    # The canonical file is gone; only the dangling link and the staged
    # root remain, and every runtime variable is unset.
    canonical.unlink()
    env_uninstall = _clean_env(tmp_path)
    assert "OPENCLAW_CONFIG_PATH" not in env_uninstall
    assert "OPENCLAW_CONFIG" not in env_uninstall

    result = run_entrypoint(UNINSTALL, env=env_uninstall, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert "openclaw: uninstalled" in result.stdout
    assert not staged_root.exists()
    assert link.is_symlink(), "uninstall must not remove the config symlink"


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_dry_run_forwards_to_every_adapter_and_writes_nothing(tmp_path, bash_bin):
    home = tmp_path / "home"
    oc_config = home / ".openclaw" / "openclaw.json"
    hermes_home = home / ".hermes"
    log_file = tmp_path / "logs" / "teamharness-install.jsonl"
    env = _clean_env(
        tmp_path,
        stubs=("qwenpaw",),
        extra={
            "OPENCLAW_CONFIG": str(oc_config),
            "HERMES_HOME": str(hermes_home),
            "TEAMHARNESS_INSTALL_LOG": str(log_file),
        },
    )

    result = run_entrypoint(INSTALL, "--dry-run", env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    for runtime in RUNTIME_BINARIES:
        assert f"{runtime}: installed (dry-run)" in result.stdout

    # Nothing was written: no config, no staged roots, no marker, no log.
    assert not (home / ".openclaw").exists()
    assert not (hermes_home / "config.yaml").exists()
    assert not (hermes_home / "plugins").exists()
    assert not (home / ".qwenpaw").exists()
    assert not log_file.exists()

    result = run_entrypoint(UNINSTALL, "--dry-run", env=env, bash_bin=bash_bin)
    assert result.returncode == 0, result.stdout + result.stderr
    assert not (home / ".openclaw").exists()
    assert not (home / ".qwenpaw").exists()
    assert not log_file.exists()


@pytest.mark.parametrize("bash_bin", INTERPRETER_NAMES, ids=INTERPRETER_IDS)
def test_entrypoints_pass_bash_syntax_check(bash_bin):
    for script in (INSTALL, UNINSTALL):
        result = subprocess.run(
            [bash_bin, "-n", str(script)],
            capture_output=True,
            text=True,
            timeout=60,
        )
        assert result.returncode == 0, f"bash -n {script.name}: {result.stderr}"
