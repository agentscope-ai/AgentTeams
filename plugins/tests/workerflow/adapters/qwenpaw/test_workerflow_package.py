import json
import os
import re
import subprocess
import zipfile
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[5]
MANIFEST = REPO_ROOT / "plugins" / "workerflow" / "plugin.yaml"
BUILD_SCRIPT = (
    REPO_ROOT
    / "plugins"
    / "workerflow"
    / "adapters"
    / "qwenpaw"
    / "scripts"
    / "build-qwenpaw-plugin.rb"
)


def _manifest_version() -> str:
    match = re.search(r"^  version:\s*(\S+)\s*$", MANIFEST.read_text(encoding="utf-8"), re.MULTILINE)
    assert match is not None
    return match.group(1)


def test_build_qwenpaw_native_workerflow_plugin_package(tmp_path: Path) -> None:
    result = subprocess.run(
        ["ruby", str(BUILD_SCRIPT), str(MANIFEST)],
        cwd=REPO_ROOT,
        env={**os.environ, "OUT_DIR": str(tmp_path)},
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=True,
    )

    zip_path = Path(result.stdout.strip().splitlines()[-1])
    assert zip_path.is_file()
    assert (tmp_path / "workerflow-qwenpaw.zip").is_file()

    version = _manifest_version()
    root = f"workerflow-qwenpaw-{version}"
    with zipfile.ZipFile(zip_path) as archive:
        names = set(archive.namelist())
        assert f"{root}/plugin.json" in names
        assert f"{root}/plugin.py" in names
        assert f"{root}/workerflow/plugin.yaml" in names
        assert f"{root}/workerflow/prompts/team/WORKERFLOW.md" in names
        assert f"{root}/workerflow/prompts/agent/worker.md" in names
        assert f"{root}/workerflow/skills/agent/worker-internal-workflow/SKILL.md" in names
        assert f"{root}/workerflow/mcp/server.py" in names

        skill = archive.read(
            f"{root}/workerflow/skills/agent/worker-internal-workflow/SKILL.md"
        ).decode("utf-8")
        manifest = json.loads(archive.read(f"{root}/plugin.json"))

    assert skill.startswith("---\n")
    assert "name: workerflow-internal-workflow" in skill
    assert manifest["id"] == "workerflow"
    assert manifest["version"] == version
    assert manifest["entry"]["backend"] == "plugin.py"
    assert "workerflow-mcp" in manifest["meta"]["features"]
