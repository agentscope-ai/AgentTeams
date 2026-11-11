"""Worker isolation bundle tests (MX-8 / M7): whole-bundle replacement of
the worker shim + gitconfig, with a fail-closed integrity self-check on
the isolation directory (foreign or missing file -> halt, no silent
repair). Drives shared/lib/worker-isolation.sh through bash.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
ISOLATION_LIB = REPO_ROOT / "shared" / "lib" / "worker-isolation.sh"
ENTRYPOINT = REPO_ROOT / "worker" / "scripts" / "worker-entrypoint.sh"
MARKER = "# Written by agentteams worker entrypoint on every start; edits are lost."

BASH = shutil.which("bash")
pytestmark = pytest.mark.skipif(
    BASH is None, reason="bash not available in this environment"
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _iso_env(tmp_path) -> dict:
    env = dict(os.environ)
    env["AGENTTEAMS_ISOLATION_DIR"] = str(tmp_path / "iso")
    return env


def _workspace(tmp_path) -> Path:
    ws = tmp_path / "ws"
    (ws / "skills" / "file-sync" / "scripts").mkdir(parents=True)
    return ws


def _run_bash(script: str, env: dict) -> subprocess.CompletedProcess:
    return subprocess.run(
        [BASH, "-c", script], capture_output=True, text=True, env=env, timeout=60
    )


def _setup_script(workspace: Path) -> str:
    return (
        'set -u\n'
        f'source "{ISOLATION_LIB}"\n'
        f'worker_isolation_setup "{workspace}"\n'
    )


def _setup(tmp_path):
    env = _iso_env(tmp_path)
    ws = _workspace(tmp_path)
    proc = _run_bash(_setup_script(ws), env)
    assert proc.returncode == 0, f"setup failed: {proc.stdout}{proc.stderr}"
    return env, ws


def _setup_again(tmp_path, env, ws) -> subprocess.CompletedProcess:
    return _run_bash(_setup_script(ws), env)


def _iso(tmp_path, env) -> Path:
    return Path(env["AGENTTEAMS_ISOLATION_DIR"])


# ---------------------------------------------------------------------------
# Fresh write + idempotency
# ---------------------------------------------------------------------------


def test_fresh_dir_writes_bundle(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    shim = iso / "bin" / "agentteams-sync"
    gitconfig = iso / "gitconfig"

    assert shim.is_file()
    lines = shim.read_text().splitlines()
    assert lines[0] == "#!/bin/bash"  # shebang must stay on line 1
    assert lines[1] == MARKER
    assert lines[2] == (
        f'exec /bin/sh "{ws}/skills/file-sync/scripts/agentteams-sync.sh" "$@"'
    )

    assert gitconfig.is_file()
    glines = gitconfig.read_text().splitlines()
    assert glines[0] == MARKER
    assert "[safe]" in glines
    assert "\tdirectory = *" in glines
    # No identity: the worker does not commit (git is delegated to the
    # Manager) — a commit fails closed instead of misattributing.
    assert "user" not in "\n".join(glines).replace("username", "")


def test_setup_is_idempotent(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    before = {
        "bin/agentteams-sync": (iso / "bin" / "agentteams-sync").read_bytes(),
        "gitconfig": (iso / "gitconfig").read_bytes(),
    }
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode == 0, f"second run failed: {proc.stdout}{proc.stderr}"
    assert (iso / "bin" / "agentteams-sync").read_bytes() == before["bin/agentteams-sync"]
    assert (iso / "gitconfig").read_bytes() == before["gitconfig"]
    # Whole-bundle rewrite: exactly the expected entries, no leftovers.
    assert sorted(p.name for p in iso.iterdir()) == ["bin", "gitconfig"]
    assert sorted(p.name for p in (iso / "bin").iterdir()) == ["agentteams-sync"]


# ---------------------------------------------------------------------------
# Fail-closed self-check
# ---------------------------------------------------------------------------


def test_foreign_file_in_bin_halts(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    (iso / "bin" / "evil").write_text("echo pwned")
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode != 0
    out = proc.stdout + proc.stderr
    assert "bin/evil" in out  # the offending path is named
    assert "foreign" in out
    # No silent repair: the foreign file is left in place.
    assert (iso / "bin" / "evil").is_file()


def test_foreign_top_level_file_halts(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    (iso / "notes.txt").write_text("foreign")
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode != 0
    assert "notes.txt" in proc.stdout + proc.stderr


def test_missing_expected_file_halts(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    (iso / "gitconfig").unlink()
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode != 0
    out = proc.stdout + proc.stderr
    assert "gitconfig" in out and "missing" in out


def test_missing_shim_halts_without_repair(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    shim = iso / "bin" / "agentteams-sync"
    shim.unlink()
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode != 0
    assert "bin/agentteams-sync" in proc.stdout + proc.stderr
    assert "missing" in proc.stdout + proc.stderr
    assert not shim.exists()


def test_foreign_content_halts(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    shim = iso / "bin" / "agentteams-sync"
    # Shebang kept, marker replaced by foreign content.
    shim.write_text("#!/bin/bash\n# attacker\nexec /bin/true\n")
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode != 0
    out = proc.stdout + proc.stderr
    assert "bin/agentteams-sync" in out
    assert "marker" in out


def test_symlinked_bundle_file_halts(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    target = tmp_path / "outside-gitconfig"
    target.write_text(MARKER + "\n")
    (iso / "gitconfig").unlink()
    (iso / "gitconfig").symlink_to(target)
    proc = _setup_again(tmp_path, env, ws)
    assert proc.returncode != 0
    assert "gitconfig" in proc.stdout + proc.stderr


# ---------------------------------------------------------------------------
# Environment isolation
# ---------------------------------------------------------------------------


def test_env_scrubs_git_config_and_exports_bundle(tmp_path):
    env, ws = _setup(tmp_path)
    iso = _iso(tmp_path, env)
    env.update(
        {
            "GIT_CONFIG_COUNT": "2",
            "GIT_CONFIG_KEY_0": "user.name",
            "GIT_CONFIG_VALUE_0": "Evil",
            "GIT_CONFIG_KEY_1": "safe.directory",
            "GIT_CONFIG_VALUE_1": "*",
            "GIT_AUTHOR_NAME": "Evil",
            "GIT_AUTHOR_EMAIL": "evil@example.com",
            "GIT_COMMITTER_NAME": "Evil",
            "GIT_COMMITTER_EMAIL": "evil@example.com",
            "SSH_ASKPASS": "/bin/evil",
        }
    )
    script = (
        'set -u\n'
        f'source "{ISOLATION_LIB}"\n'
        "worker_isolation_env\n"
        'echo "GLOBAL=${GIT_CONFIG_GLOBAL}"\n'
        'echo "NOSYSTEM=${GIT_CONFIG_NOSYSTEM}"\n'
        'if [ -n "${GIT_CONFIG_COUNT:-}" ]; then echo "COUNT=left"; fi\n'
        'if [ -n "${GIT_CONFIG_KEY_0:-}" ]; then echo "KEY0=left"; fi\n'
        'if [ -n "${GIT_AUTHOR_NAME:-}" ]; then echo "AUTHOR=left"; fi\n'
        'if [ -n "${GIT_COMMITTER_EMAIL:-}" ]; then echo "COMMITTER=left"; fi\n'
        'if [ -n "${SSH_ASKPASS:-}" ]; then echo "SSHASK=left"; fi\n'
        "echo OK\n"
    )
    proc = _run_bash(script, env)
    assert proc.returncode == 0, f"{proc.stdout}{proc.stderr}"
    lines = proc.stdout.splitlines()
    assert f"GLOBAL={iso}/gitconfig" in lines
    assert "NOSYSTEM=1" in lines
    for left in ("COUNT=left", "KEY0=left", "AUTHOR=left", "COMMITTER=left", "SSHASK=left"):
        assert left not in lines, f"{left} survived the scrub: {lines}"


# ---------------------------------------------------------------------------
# Entrypoint wiring (static guard)
# ---------------------------------------------------------------------------


def test_scripts_parse():
    for path in (ISOLATION_LIB, ENTRYPOINT):
        proc = subprocess.run(
            [BASH, "-n", str(path)], capture_output=True, text=True, timeout=60
        )
        assert proc.returncode == 0, f"{path} does not parse: {proc.stderr}"


def test_entrypoint_uses_isolation_bundle():
    text = ENTRYPOINT.read_text()
    assert "source /opt/agentteams/scripts/lib/worker-isolation.sh" in text
    assert 'worker_isolation_setup "${WORKSPACE}"' in text
    assert "worker_isolation_env" in text
    assert "ln -sfn \"$(worker_isolation_dir)/bin/agentteams-sync\" /usr/local/bin/agentteams-sync" in text
    # The old incremental printf overwrite is gone.
    assert "printf '#!/bin/bash" not in text
