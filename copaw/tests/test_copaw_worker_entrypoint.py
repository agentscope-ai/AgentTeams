import os
from pathlib import Path
import subprocess


def _render_entrypoint(tmp_path: Path, *, mc_host: bool) -> Path:
    source = Path(__file__).resolve().parents[1] / "scripts" / "copaw-worker-entrypoint.sh"
    fake_root = tmp_path / "opt" / "hiclaw"
    fake_venv = tmp_path / "opt" / "venv"
    install_dir = tmp_path / "copaw-worker"
    script = (
        source.read_text()
        .replace("/opt/agentteams", str(fake_root))
        .replace("/opt/venv", str(fake_venv))
        .replace('INSTALL_DIR="/root/.copaw-worker"', f'INSTALL_DIR="{install_dir}"')
    )

    lib_dir = fake_root / "scripts" / "lib"
    lib_dir.mkdir(parents=True)
    mc_line = (
        'export "MC_HOST_${AGENTTEAMS_STORAGE_ALIAS:-agentteams}=https://ak:sk@oss.example.com"\n'
        if mc_host
        else ""
    )
    (lib_dir / "agentteams-env.sh").write_text(
        "#!/bin/sh\n"
        "AGENTTEAMS_STORAGE_ALIAS=\"${AGENTTEAMS_STORAGE_ALIAS:-agentteams}\"\n"
        "ensure_mc_credentials() {\n"
        f"{mc_line}"
        "  return 0\n"
        "}\n"
        "agentteams_mc_host_var() {\n"
        "  printf 'MC_HOST_%s' \"${AGENTTEAMS_STORAGE_ALIAS:-agentteams}\"\n"
        "}\n"
        "agentteams_mc_host_configured() {\n"
        "  var=\"$(agentteams_mc_host_var)\"\n"
        "  [ -n \"${!var:-}\" ]\n"
        "}\n"
    )

    for mode in ("lite", "standard"):
        bin_dir = fake_venv / mode / "bin"
        bin_dir.mkdir(parents=True)
        worker = bin_dir / "copaw-worker"
        worker.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$CAPTURE_FILE"\n')
        worker.chmod(0o755)

    rendered = tmp_path / "copaw-worker-entrypoint.sh"
    rendered.write_text(script)
    rendered.chmod(0o755)
    return rendered


def test_entrypoint_uses_agentteams_mc_host_and_legacy_bucket_contract(tmp_path):
    script = _render_entrypoint(tmp_path, mc_host=True)
    capture = tmp_path / "args.txt"
    env = {
        **os.environ,
        "HOME": str(tmp_path / "home"),
        "CAPTURE_FILE": str(capture),
        "AGENTTEAMS_WORKER_NAME": "alice",
        "AGENTTEAMS_STORAGE_PROVIDER": "oss",
        "AGENTTEAMS_FS_BUCKET": "custom-bucket",
    }

    result = subprocess.run([str(script)], env=env, text=True, capture_output=True)

    assert result.returncode == 0, result.stderr + result.stdout
    args = capture.read_text().splitlines()
    assert args[args.index("--fs-bucket") + 1] == "custom-bucket"


def test_entrypoint_reports_missing_agentteams_mc_host_for_oss(tmp_path):
    script = _render_entrypoint(tmp_path, mc_host=False)
    env = {
        **os.environ,
        "HOME": str(tmp_path / "home"),
        "CAPTURE_FILE": str(tmp_path / "args.txt"),
        "AGENTTEAMS_WORKER_NAME": "alice",
        "AGENTTEAMS_STORAGE_PROVIDER": "oss",
    }

    result = subprocess.run([str(script)], env=env, text=True, capture_output=True)

    assert result.returncode == 1
    assert "MC_HOST_agentteams is not configured" in result.stdout
