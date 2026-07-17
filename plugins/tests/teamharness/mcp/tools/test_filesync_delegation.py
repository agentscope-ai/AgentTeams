"""Direct tests for TeamHarness MCP filesync module."""

from __future__ import annotations

import json
import os
import stat
import sys
import tempfile
from pathlib import Path

MCP_DIR = Path(__file__).resolve().parents[4] / "teamharness" / "mcp"
sys.path.insert(0, str(MCP_DIR))

import _bootstrap  # noqa: F401
from tools.filesync import filesync  # noqa: E402


def _write_fake_mc(bin_dir: Path, log_path: Path) -> None:
    if os.name == "nt":
        script = f"""@echo off
echo %*>> "{log_path}"
echo ENV MC_HOST_agentteams=%MC_HOST_agentteams%>> "{log_path}"
echo %* | findstr /C:"agentteams/agentteams-storage" >nul
if %ERRORLEVEL%==0 (
  if "%MC_HOST_agentteams%"=="" (
    echo missing MC_HOST_agentteams 1>&2
    exit /b 3
  )
)
echo %* | findstr /C:"tasks/denied" >nul
if %ERRORLEVEL%==0 (
  echo mc.bin: ^<ERROR^> Unable to list comparison retrying.. Access Denied. 1>&2
  exit /b 0
)
echo %1 | findstr /C:"ls" >nul
if %ERRORLEVEL%==0 (
  echo 2026-06-03 12:00:00      42 projects/demo/plan.md
)
exit /b 0
"""
        mc_bin = bin_dir / "mc.cmd"
    else:
        script = f"""#!/usr/bin/env bash
printf '%s\\n' "$*" >> "{log_path}"
printf 'ENV MC_HOST_agentteams=%s\\n' "${{MC_HOST_agentteams:-}}" >> "{log_path}"
case "$*" in
  *"agentteams/agentteams-storage"*)
    if [ -z "${{MC_HOST_agentteams:-}}" ]; then
      echo "missing MC_HOST_agentteams" >&2
      exit 3
    fi
    ;;
esac
case "$*" in
  *"tasks/denied"*)
    echo "mc.bin: <ERROR> Unable to list comparison retrying.. Access Denied." >&2
    exit 0
    ;;
esac
if [ "$1" = "ls" ]; then
  echo "2026-06-03 12:00:00      42 projects/demo/plan.md"
fi
exit 0
"""
        mc_bin = bin_dir / "mc"
    mc_bin.write_text(script, encoding="utf-8")
    if os.name != "nt":
        mc_bin.chmod(stat.S_IRWXU)


def test_filesync_push_pull_list_via_filesync_module() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        workspace = root / "workspace"
        bin_dir = root / "bin"
        log_path = root / "mc.log"
        bin_dir.mkdir()
        _write_fake_mc(bin_dir, log_path)

        common = {
            "workspaceDir": str(workspace),
            "storage": {
                "sharedPrefix": "mock/shared",
                "globalSharedPrefix": "mock/global-shared",
            },
        }

        env = {
            "PATH": f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}",
            "AGENTTEAMS_FS_ENDPOINT": "https://oss.example.test",
            "AGENTTEAMS_FS_ACCESS_KEY": "access-key",
            "AGENTTEAMS_FS_SECRET_KEY": "secret-key",
        }
        previous = {key: os.environ.get(key) for key in env}
        os.environ.update(env)
        try:
            dry = filesync({**common, "action": "pull", "path": "shared/projects/demo", "dryRun": True})
            assert dry.get("ok")
            assert dry.get("command")[2] == "mock/shared/projects/demo/"

            result_path = workspace / "shared/tasks/t-001/result.md"
            result_path.parent.mkdir(parents=True, exist_ok=True)
            result_path.write_text("# Result\n", encoding="utf-8")
            pushed = filesync(
                {**common, "action": "push", "path": "shared/tasks/t-001", "exclude": ["*.tmp"]}
            )
            assert pushed.get("ok")

            listed = filesync({**common, "action": "list", "path": "shared/projects/demo"})
            assert listed.get("ok")
            assert listed.get("entries")

            denied_dir = workspace / "shared/tasks/denied"
            denied_dir.mkdir(parents=True, exist_ok=True)
            (denied_dir / "result.md").write_text("# Denied\n", encoding="utf-8")
            denied = filesync({**common, "action": "push", "path": "shared/tasks/denied"})
            assert denied.get("ok") is False
            assert "Access Denied" in denied.get("error", "")
        finally:
            for key, value in previous.items():
                if value is None:
                    os.environ.pop(key, None)
                else:
                    os.environ[key] = value

        commands = [line.strip() for line in log_path.read_text(encoding="utf-8").splitlines()]
        assert any("mirror" in line and "t-001" in line for line in commands)
