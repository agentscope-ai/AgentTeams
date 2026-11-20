import json
import os
from pathlib import Path
import subprocess

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/publish-harnesses.yml").read_text())
SCRIPT = WORKFLOW["jobs"]["prepare"]["steps"][0]["run"]


@pytest.mark.parametrize("runtime,architecture,publish,success,count", [
    ("all", "all", "false", True, 12),
    ("codex", "amd64", "false", True, 1),
    ("codex", "amd64", "true", False, 0),
    ("invalid", "all", "false", False, 0),
])
def test_actual_workflow_selection(tmp_path, runtime, architecture, publish, success, count):
    output = tmp_path / "output"
    result = subprocess.run(["bash", "-e", "-c", SCRIPT], capture_output=True, text=True,
        env={**os.environ, "VERSION": "v1.2.8", "RUNTIME": runtime,
             "ARCHITECTURE": architecture, "PUBLISH": publish,
             "GITHUB_SHA": "a" * 40, "GITHUB_OUTPUT": str(output)})
    assert (result.returncode == 0) == success, result.stderr
    if success:
        values = dict(line.split("=", 1) for line in output.read_text().splitlines())
        assert len(json.loads(values["runtimes"])) * len(json.loads(values["architectures"])) == count
        assert "include" not in WORKFLOW["jobs"]["harness-platform"]["strategy"]["matrix"]


@pytest.mark.parametrize("runtime_changed", [False, True])
def test_controller_reuse_fetches_source_and_rejects_runtime_drift(tmp_path, runtime_changed):
    def git(cwd, *args):
        return subprocess.check_output(["git", "-C", str(cwd), *args], text=True).strip()

    remote = tmp_path / "remote.git"
    subprocess.run(["git", "init", "--bare", str(remote)], check=True, capture_output=True)
    source = tmp_path / "source"
    source.mkdir()
    git(source, "init", "-b", "main")
    git(source, "config", "user.email", "test@example.com")
    git(source, "config", "user.name", "Test")
    (source / "agentteams-controller").mkdir()
    (source / "agentteams-controller/main.go").write_text("original runtime")
    git(source, "add", ".")
    git(source, "commit", "-m", "base")
    base = git(source, "rev-parse", "HEAD")
    (source / "source-marker").write_text("image build")
    git(source, "add", ".")
    git(source, "commit", "-m", "image source outside main history")
    revision = git(source, "rev-parse", "HEAD")
    git(source, "push", str(remote), "HEAD:refs/heads/build-source")
    git(source, "checkout", "--detach", base)
    (source / "main-marker").write_text("release")
    if runtime_changed:
        (source / "agentteams-controller/main.go").write_text("changed runtime")
    git(source, "add", ".")
    git(source, "commit", "-m", "release source")
    git(source, "push", str(remote), "HEAD:refs/heads/main")
    checkout = tmp_path / "checkout"
    subprocess.run(["git", "clone", "--depth=1", "--branch=main", remote.as_uri(), str(checkout)],
                   check=True, capture_output=True)
    assert subprocess.run(["git", "-C", str(checkout), "cat-file", "-e", revision],
                          capture_output=True).returncode != 0
    binaries = tmp_path / "bin"
    binaries.mkdir()
    docker = binaries / "docker"
    docker.write_text('#!/bin/sh\nif [ "$1" = inspect ]; then echo "$TEST_REVISION"; fi\n')
    docker.chmod(0o755)
    step = next(s for s in WORKFLOW["jobs"]["controller-platform"]["steps"]
                if s.get("name") == "Build shared CLI and builtin assets on native runner")
    result = subprocess.run(["bash", "-e", "-c", step["run"]], cwd=checkout,
        capture_output=True, text=True, env={**os.environ, "PATH": str(binaries)+os.pathsep+os.environ["PATH"],
            "TEST_REVISION": revision, "REGISTRY": "higress-registry.cn-hangzhou.cr.aliyuncs.com",
            "CANDIDATE": "test", "ARCH": "arm64", "REUSE_CONTROLLER":
            "higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-controller@sha256:"+"a"*64})
    assert (result.returncode == 0) == (not runtime_changed), result.stderr
