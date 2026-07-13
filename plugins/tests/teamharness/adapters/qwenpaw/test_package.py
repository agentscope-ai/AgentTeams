import json
import os
import re
import subprocess
import sys
import zipfile
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[5]
MANIFEST = REPO_ROOT / "plugins" / "teamharness" / "plugin.yaml"
BUILD_SCRIPT = (
    REPO_ROOT
    / "plugins"
    / "teamharness"
    / "adapters"
    / "qwenpaw"
    / "scripts"
    / "build-qwenpaw-plugin.rb"
)


def _manifest_version() -> str:
    match = re.search(r"^  version:\s*(\S+)\s*$", MANIFEST.read_text(encoding="utf-8"), re.MULTILINE)
    assert match is not None
    return match.group(1)


def test_build_qwenpaw_native_plugin_package(tmp_path: Path) -> None:
    result = subprocess.run(
        ["ruby", str(BUILD_SCRIPT), str(MANIFEST)],
        cwd=REPO_ROOT,
        env={
            **os.environ,
            "OUT_DIR": str(tmp_path),
            "PYTHONPYCACHEPREFIX": str(tmp_path / "pycache"),
        },
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=True,
    )

    zip_path = Path(result.stdout.strip().splitlines()[-1])
    assert zip_path.is_file()
    assert (tmp_path / "teamharness-qwenpaw.zip").is_file()

    version = _manifest_version()
    root = f"teamharness-qwenpaw-{version}"
    with zipfile.ZipFile(zip_path) as archive:
        names = set(archive.namelist())
        assert f"{root}/plugin.json" in names
        assert f"{root}/plugin.py" in names
        assert f"{root}/task_trace.py" in names
        assert f"{root}/teamharness/plugin.yaml" in names
        assert f"{root}/teamharness/trace.py" not in names
        assert f"{root}/teamharness/prompts/team/TEAMS.md" in names
        assert f"{root}/teamharness/prompts/agent/worker.md" in names
        assert f"{root}/teamharness/skills/team/communication/SKILL.md" in names
        assert f"{root}/teamharness/skills/team/roomflow/SKILL.md" in names
        assert f"{root}/teamharness/mcp/server.py" in names
        assert f"{root}/teamharness/mcp/message_tool.py" in names
        assert f"{root}/teamharness/mcp/roomflow_tool.py" in names
        assert not any(name.startswith(f"{root}/teamharness/hooks/") for name in names)
        communication_skill = archive.read(f"{root}/teamharness/skills/team/communication/SKILL.md").decode("utf-8")
        project_skill = archive.read(f"{root}/teamharness/skills/team/project-management/SKILL.md").decode("utf-8")
        assert communication_skill.startswith("---\n")
        assert "name: teamharness-communication" in communication_skill
        assert "description:" in communication_skill
        assert project_skill.startswith("---\n")
        assert "name: teamharness-project-management" in project_skill
        assert "payload` as a JSON object" in project_skill

        manifest = json.loads(archive.read(f"{root}/plugin.json"))
        extract_dir = tmp_path / "extract"
        archive.extractall(extract_dir)

    assert manifest["id"] == "teamharness"
    assert manifest["version"] == version
    assert manifest["entry"]["backend"] == "plugin.py"
    assert "periodic-sync" not in manifest["meta"]["features"]

    probe = subprocess.run(
        [
            sys.executable,
            "-c",
            (
                "import importlib.util, json; "
                f"path = {str(extract_dir / root / 'plugin.py')!r}; "
                "spec = importlib.util.spec_from_file_location('teamharness_pkg', path); "
                "mod = importlib.util.module_from_spec(spec); "
                "spec.loader.exec_module(mod); "
                "print(json.dumps(mod.install_task_trace_processor()))"
            ),
        ],
        env={
            **os.environ,
            "PYTHONDONTWRITEBYTECODE": "1",
            "QWENPAW_WORKING_DIR": str(tmp_path / ".qwenpaw"),
        },
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=True,
    )
    trace_result = json.loads(probe.stdout)
    assert trace_result.get("reason") != "task_trace.py not found"
