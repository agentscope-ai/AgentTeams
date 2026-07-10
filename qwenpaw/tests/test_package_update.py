import json
import tarfile
import os
import subprocess
import threading
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import shutil
import sys
import types
from urllib.parse import parse_qs, urlparse

from qwenpaw_worker.update import AgentPackageManager, MemberRuntimeConfig, _strip_json_line_comments


def _package(
    tmp_path: Path,
    version: str,
    *,
    materials: bool = False,
    include_teams: bool = False,
    mcp_servers=None,
    mcp_json=None,
) -> Path:
    source_dir = tmp_path / f"src-{version}"
    config_dir = source_dir / "config"
    skill_dir = source_dir / "skills" / "code-review"
    config_dir.mkdir(parents=True)
    skill_dir.mkdir(parents=True)
    (source_dir / "manifest.json").write_text('{"version":"1.0"}\n', encoding="utf-8")
    if mcp_json is None:
        mcp_json = {"mcpServers": mcp_servers or {}}
    if isinstance(mcp_json, str):
        mcp_json_text = mcp_json if mcp_json.endswith("\n") else f"{mcp_json}\n"
    else:
        mcp_json_text = json.dumps(mcp_json, ensure_ascii=False) + "\n"
    (source_dir / "mcp.json").write_text(mcp_json_text, encoding="utf-8")
    (config_dir / "AGENTS.md").write_text(f"agent package {version}\n", encoding="utf-8")
    (config_dir / "SOUL.md").write_text(f"soul {version}\n", encoding="utf-8")
    (config_dir / "MEMORY.md").write_text(f"memory {version}\n", encoding="utf-8")
    if include_teams:
        (config_dir / "TEAMS.md").write_text(f"teams package {version}\n", encoding="utf-8")
    if materials:
        (config_dir / "BOOTSTRAP.md").write_text(f"bootstrap {version}\n", encoding="utf-8")
        materials_dir = config_dir / "materials"
        materials_dir.mkdir()
        (materials_dir / "custom.md").write_text(f"custom docs {version}\n", encoding="utf-8")
        crons_dir = config_dir / "crons"
        crons_dir.mkdir()
        (crons_dir / "daily.md").write_text(f"cron {version}\n", encoding="utf-8")
        runtime_config_dir = config_dir / "config"
        runtime_config_dir.mkdir()
        (runtime_config_dir / "settings.yaml").write_text(f"settings: {version}\n", encoding="utf-8")
        (runtime_config_dir / "credagent.json").write_text(f'{{"version":"{version}"}}\n', encoding="utf-8")
        (runtime_config_dir / "mcporter.json").write_text(f'{{"package":"{version}"}}\n', encoding="utf-8")
        bootstrap_dir = config_dir / "bootstrap"
        bootstrap_dir.mkdir()
        (bootstrap_dir / "seed.md").write_text(f"config bootstrap {version}\n", encoding="utf-8")
    (skill_dir / "SKILL.md").write_text(f"skill {version}\n", encoding="utf-8")
    package_path = tmp_path / f"dev-worker-{version}.tar.gz"
    with tarfile.open(package_path, "w:gz") as archive:
        archive.add(source_dir, arcname=".")
    return package_path


def _empty_package(tmp_path: Path, version: str) -> Path:
    source_dir = tmp_path / f"empty-src-{version}"
    source_dir.mkdir()
    (source_dir / "manifest.json").write_text('{"version":"1.0"}\n', encoding="utf-8")
    package_path = tmp_path / f"empty-dev-worker-{version}.tar.gz"
    with tarfile.open(package_path, "w:gz") as archive:
        archive.add(source_dir, arcname=".")
    return package_path


def _runtime_config(tmp_path: Path, package_path: Path, version: str) -> MemberRuntimeConfig:
    path = tmp_path / f"runtime-{version}.yaml"
    path.write_text(
        f"""
kind: MemberRuntimeConfig
metadata:
  generation: {version}
member:
  name: worker-a
  runtime: qwenpaw
desired:
  agentPackage:
    ref: file://{package_path}
    name: dev-worker
    version: {version}
    digest: sha256:{version}
""",
        encoding="utf-8",
    )
    return MemberRuntimeConfig.load(path)


def _runtime_config_ref(tmp_path: Path, ref: str, version: str) -> MemberRuntimeConfig:
    path = tmp_path / f"runtime-ref-{version}.yaml"
    path.write_text(
        f"""
kind: MemberRuntimeConfig
metadata:
  generation: {version}
member:
  name: worker-a
  runtime: qwenpaw
desired:
  agentPackage:
    ref: {ref}
    name: dev-worker
    version: {version}
    digest: sha256:{version}
""",
        encoding="utf-8",
    )
    return MemberRuntimeConfig.load(path)


def _runtime_config_without_package(tmp_path: Path) -> MemberRuntimeConfig:
    path = tmp_path / "runtime-without-package.yaml"
    path.write_text(
        """
kind: MemberRuntimeConfig
metadata:
  generation: no-package
member:
  name: worker-a
  runtime: qwenpaw
desired: {}
""",
        encoding="utf-8",
    )
    return MemberRuntimeConfig.load(path)


def _install_fake_qwenpaw_mcp_config(monkeypatch):
    saved: dict[str, object] = {}
    agent_config = types.SimpleNamespace(mcp=None)

    class MCPConfig:
        def __init__(self) -> None:
            self.clients = {}

    class MCPClientConfig:
        def __init__(self, **kwargs) -> None:
            self.__dict__.update(kwargs)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.MCPConfig = MCPConfig
    config_module.MCPClientConfig = MCPClientConfig
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)
    return saved, agent_config


def test_agent_package_manager_applies_changed_package_without_restart_signal(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")
    package_v2 = _package(tmp_path, "2")

    applied_v1 = manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    assert applied_v1 == manager.current_dir
    assert (manager.current_dir / "config" / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "SOUL.md").read_text(encoding="utf-8") == "soul 1\n"
    assert (workspace_dir / "MEMORY.md").read_text(encoding="utf-8") == "memory 1\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill 1\n"

    same = manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    assert same == manager.current_dir
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"

    applied_v2 = manager.apply(_runtime_config(tmp_path, package_v2, "2"))
    assert applied_v2 == manager.current_dir
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 2\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill 2\n"
    assert manager.marker_path.read_text(encoding="utf-8").splitlines() == [
        f"file://{package_v2}",
        "dev-worker",
        "2",
        "sha256:2",
    ]


def test_agent_package_manager_fetches_oss_package_from_storage_prefix(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    storage_prefix = tmp_path / "agentteams" / "agentteams-storage"
    remote_package = storage_prefix / "agent" / "worker-a" / "packages" / "dev-worker-1.tar.gz"
    remote_package.parent.mkdir(parents=True)
    shutil.copy2(_package(tmp_path, "1"), remote_package)

    fake_bin = tmp_path / "bin"
    fake_bin.mkdir()
    mc = fake_bin / "mc"
    mc.write_text(
        """#!/bin/sh
set -eu
if [ "$1" != "cp" ]; then
  echo "unexpected mc command: $*" >&2
  exit 2
fi
cp "$2" "$3"
""",
        encoding="utf-8",
    )
    mc.chmod(0o755)
    monkeypatch.setenv("PATH", f"{fake_bin}:{os.environ.get('PATH', '')}")
    monkeypatch.setenv("AGENTTEAMS_STORAGE_PREFIX", str(storage_prefix))

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    applied = manager.apply(
        _runtime_config_ref(
            tmp_path,
            "oss://agent/worker-a/packages/dev-worker-1.tar.gz",
            "1",
        )
    )

    assert applied == manager.current_dir
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill 1\n"


def test_agent_package_manager_copies_package_materials_to_default_workspace(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    (workspace_dir / "config").mkdir(parents=True)
    (workspace_dir / "config" / "mcporter.json").write_text('{"runtime":true}\n', encoding="utf-8")
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1", materials=True)
    package_v2 = _package(tmp_path, "2", materials=True)

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    assert (manager.current_dir / "config" / "BOOTSTRAP.md").read_text(encoding="utf-8") == "bootstrap 1\n"
    assert (workspace_dir / "BOOTSTRAP.md").read_text(encoding="utf-8") == "bootstrap 1\n"
    assert (workspace_dir / "materials" / "custom.md").read_text(encoding="utf-8") == "custom docs 1\n"
    assert (workspace_dir / "crons" / "daily.md").read_text(encoding="utf-8") == "cron 1\n"
    assert (workspace_dir / "config" / "settings.yaml").read_text(encoding="utf-8") == "settings: 1\n"
    assert (workspace_dir / "config" / "credagent.json").read_text(encoding="utf-8") == '{"version":"1"}\n'
    assert (workspace_dir / "bootstrap" / "seed.md").read_text(encoding="utf-8") == "config bootstrap 1\n"
    assert (workspace_dir / "config" / "mcporter.json").read_text(encoding="utf-8") == '{"runtime":true}\n'
    assert not (workspace_dir / "manifest.json").exists()
    assert not (workspace_dir / "mcp.json").exists()

    manager.apply(_runtime_config(tmp_path, package_v2, "2"))

    assert (workspace_dir / "BOOTSTRAP.md").read_text(encoding="utf-8") == "bootstrap 2\n"
    assert (workspace_dir / "materials" / "custom.md").read_text(encoding="utf-8") == "custom docs 2\n"
    assert (workspace_dir / "crons" / "daily.md").read_text(encoding="utf-8") == "cron 2\n"
    assert (workspace_dir / "config" / "settings.yaml").read_text(encoding="utf-8") == "settings: 2\n"
    assert (workspace_dir / "config" / "credagent.json").read_text(encoding="utf-8") == '{"version":"2"}\n'
    assert (workspace_dir / "bootstrap" / "seed.md").read_text(encoding="utf-8") == "config bootstrap 2\n"
    assert (workspace_dir / "config" / "mcporter.json").read_text(encoding="utf-8") == '{"runtime":true}\n'


def test_agent_package_manager_embeds_root_mcp_json_into_qwenpaw_agent_config(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    workspace_dir.mkdir()
    (workspace_dir / "agent.json").write_text('{"existing":true}\n', encoding="utf-8")
    saved: dict[str, object] = {}
    agent_config = types.SimpleNamespace(mcp=None)

    class MCPConfig:
        def __init__(self) -> None:
            self.clients = {}

    class MCPClientConfig:
        def __init__(self, **kwargs) -> None:
            self.__dict__.update(kwargs)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.MCPConfig = MCPConfig
    config_module.MCPClientConfig = MCPClientConfig
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(
        tmp_path,
        "1",
        mcp_servers={
            "package-docs": {
                "url": "https://package.example.com/mcp",
                "transport": "http",
                "description": "Package docs",
            }
        },
    )
    package_v2 = _package(tmp_path, "2")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    assert not (workspace_dir / "mcp.json").exists()
    client = saved["default"].mcp.clients["package-docs"]
    assert client.name == "package-docs"
    assert client.url == "https://package.example.com/mcp"
    assert client.transport == "http"
    assert client.description == "Package docs"

    manager.apply(_runtime_config(tmp_path, package_v2, "2"))

    assert "package-docs" not in saved["default"].mcp.clients
    assert (workspace_dir / "agent.json").read_text(encoding="utf-8") == '{"existing":true}\n'


def test_agent_package_mcp_json_expands_agent_workspace_placeholders(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    workspace_dir.mkdir()
    saved: dict[str, object] = {}
    agent_config = types.SimpleNamespace(mcp=None)

    class MCPConfig:
        def __init__(self) -> None:
            self.clients = {}

    class MCPClientConfig:
        def __init__(self, **kwargs) -> None:
            self.__dict__.update(kwargs)

    config_module = types.ModuleType("qwenpaw.config.config")
    config_module.MCPConfig = MCPConfig
    config_module.MCPClientConfig = MCPClientConfig
    config_module.load_agent_config = lambda _agent_id: agent_config
    config_module.save_agent_config = lambda agent_id, value: saved.update({agent_id: value})

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config", types.ModuleType("qwenpaw.config"))
    monkeypatch.setitem(sys.modules, "qwenpaw.config.config", config_module)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(
        tmp_path,
        "1",
        mcp_servers={
            "workspace-tools": {
                "command": "python",
                "args": [
                    "{AGENT_WORKSPACE}/server.py",
                    "${AGENT_WORKSPACE}/data",
                ],
                "cwd": "{AGENT_WORKSPACE}",
                "env": {
                    "AGENT_WORKSPACE": "/tmp/wrong-workspace",
                    "CUSTOM_DIR": "${AGENT_WORKSPACE}/custom",
                },
            }
        },
    )

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    client = saved["default"].mcp.clients["workspace-tools"]
    assert client.args == [
        str(workspace_dir / "server.py"),
        str(workspace_dir / "data"),
    ]
    assert client.cwd == str(workspace_dir)
    assert client.env["AGENT_WORKSPACE"] == str(workspace_dir)
    assert client.env["CUSTOM_DIR"] == str(workspace_dir / "custom")


def test_agent_package_manager_accepts_mcporter_style_mcp_json(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    workspace_dir.mkdir()
    saved, _agent_config = _install_fake_qwenpaw_mcp_config(monkeypatch)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_path = _package(
        tmp_path,
        "mcporter",
        mcp_json={
            "mcpServers": {
                "remote-docs": {
                    "description": "Remote docs",
                    "baseUrl": "https://docs.example.com/mcp",
                    "transport": "http",
                    "headers": {"Authorization": "$env:DOCS_TOKEN"},
                },
                "local-tools": {
                    "description": "Local tools",
                    "command": "npx",
                    "args": ["-y", "local-tools-mcp"],
                    "env": {"TOKEN": "$env:LOCAL_TOOLS_TOKEN"},
                },
            }
        },
    )

    manager.apply(_runtime_config(tmp_path, package_path, "mcporter"))

    clients = saved["default"].mcp.clients
    remote = clients["remote-docs"]
    assert remote.name == "remote-docs"
    assert remote.url == "https://docs.example.com/mcp"
    assert remote.transport == "http"
    assert remote.headers == {"Authorization": "$env:DOCS_TOKEN"}

    local = clients["local-tools"]
    assert local.name == "local-tools"
    assert local.command == "npx"
    assert local.args == ["-y", "local-tools-mcp"]
    assert local.env == {
        "TOKEN": "$env:LOCAL_TOOLS_TOKEN",
        "AGENT_WORKSPACE": str(workspace_dir),
    }


def test_agent_package_manager_accepts_line_comments_in_mcp_json(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    workspace_dir.mkdir()
    saved, _agent_config = _install_fake_qwenpaw_mcp_config(monkeypatch)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_path = _package(
        tmp_path,
        "commented-mcp",
        mcp_json="""
{
  "mcpServers": {
    // "disabled-docs": {"url": "https://disabled.example.com/mcp"},
    "remote-docs": {
      "url": "https://docs.example.com/mcp", // keep URL schemes intact
      "transport": "http"
    }
  }
}
""",
    )

    manager.apply(_runtime_config(tmp_path, package_path, "commented-mcp"))

    clients = saved["default"].mcp.clients
    assert "disabled-docs" not in clients
    assert clients["remote-docs"].url == "https://docs.example.com/mcp"
    assert clients["remote-docs"].transport == "http"


def test_agent_package_manager_accepts_commented_mcp_json_shapes(
    tmp_path: Path,
    monkeypatch,
) -> None:
    cases = {
        "mcp-list": (
            """
{
  // list shape is accepted for older packages
  "mcpServers": [
    {"name": "list-docs", "url": "https://list.example.com/mcp", "transport": "sse"} // trailing comment
  ]
}
""",
            "list-docs",
            "https://list.example.com/mcp",
        ),
        "clients": (
            """
{
  "clients": {
    // legacy clients shape
    "legacy-docs": {"url": "https://legacy.example.com/mcp"}
  }
}
""",
            "legacy-docs",
            "https://legacy.example.com/mcp",
        ),
        "nested": (
            """
{
  "mcp": {
    "clients": {
      "nested-docs": {"url": "https://nested.example.com/mcp"} // nested legacy shape
    }
  }
}
""",
            "nested-docs",
            "https://nested.example.com/mcp",
        ),
    }

    for version, (mcp_json, client_name, url) in cases.items():
        case_dir = tmp_path / version
        workspace_dir = case_dir / "workspace"
        workspace_dir.mkdir(parents=True)
        saved, _agent_config = _install_fake_qwenpaw_mcp_config(monkeypatch)
        manager = AgentPackageManager(case_dir / "packages", workspace_dir=workspace_dir)
        package_path = _package(case_dir, version, mcp_json=mcp_json)

        manager.apply(_runtime_config(case_dir, package_path, version))

        client = saved["default"].mcp.clients[client_name]
        assert client.name == client_name
        assert client.url == url


def test_agent_package_mcp_json_comment_stripper_preserves_string_slashes_and_escapes() -> None:
    raw = json.loads(
        _strip_json_line_comments(
            """
{
  "mcpServers": {
    "docs": {
      "url": "https://docs.example.com/a//b",
      "args": ["--literal=//not-comment", "quoted \\\" // still string"]
    } // trailing comment
  }
}
"""
        )
    )

    docs = raw["mcpServers"]["docs"]
    assert docs["url"] == "https://docs.example.com/a//b"
    assert docs["args"] == ["--literal=//not-comment", 'quoted " // still string']


def test_agent_package_manager_reads_legacy_mcp_json_shapes(
    tmp_path: Path,
    monkeypatch,
) -> None:
    cases = {
        "clients": {"clients": {"docs": {"url": "https://clients.example.com/mcp"}}},
        "nested": {"mcp": {"clients": {"docs": {"url": "https://nested.example.com/mcp"}}}},
        "top-level": {"docs": {"url": "https://top-level.example.com/mcp"}},
    }

    for version, mcp_json in cases.items():
        case_dir = tmp_path / version
        workspace_dir = case_dir / "workspace"
        workspace_dir.mkdir(parents=True)
        saved, _agent_config = _install_fake_qwenpaw_mcp_config(monkeypatch)
        manager = AgentPackageManager(case_dir / "packages", workspace_dir=workspace_dir)
        package_path = _package(case_dir, version, mcp_json=mcp_json)

        manager.apply(_runtime_config(case_dir, package_path, version))

        client = saved["default"].mcp.clients["docs"]
        assert client.name == "docs"
        assert client.url == mcp_json.get("docs", {"url": f"https://{version}.example.com/mcp"})["url"]


def test_agent_package_mcp_json_does_not_overwrite_mcporter_config(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    (workspace_dir / "config").mkdir(parents=True)
    mcporter_path = workspace_dir / "config" / "mcporter.json"
    mcporter_path.write_text('{"mcpServers":{"runtime":{"url":"https://runtime.example.com/mcp"}}}\n', encoding="utf-8")
    saved, _agent_config = _install_fake_qwenpaw_mcp_config(monkeypatch)
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_path = _package(
        tmp_path,
        "package-mcp",
        mcp_json={
            "mcpServers": {
                "package-docs": {
                    "baseUrl": "https://package.example.com/mcp",
                    "transport": "http",
                }
            }
        },
    )

    manager.apply(_runtime_config(tmp_path, package_path, "package-mcp"))

    assert saved["default"].mcp.clients["package-docs"].url == "https://package.example.com/mcp"
    assert mcporter_path.read_text(encoding="utf-8") == '{"mcpServers":{"runtime":{"url":"https://runtime.example.com/mcp"}}}\n'
    assert not (workspace_dir / "mcp.json").exists()


def test_agent_package_update_removes_materials_missing_from_new_package(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1", materials=True)
    package_v2 = _package(tmp_path, "2")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    assert (workspace_dir / "BOOTSTRAP.md").exists()
    assert (workspace_dir / "materials" / "custom.md").exists()
    assert (workspace_dir / "crons" / "daily.md").exists()
    assert (workspace_dir / "bootstrap" / "seed.md").exists()

    manager.apply(_runtime_config(tmp_path, package_v2, "2"))

    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 2\n"
    assert not (workspace_dir / "BOOTSTRAP.md").exists()
    assert not (workspace_dir / "materials").exists()
    assert not (workspace_dir / "crons").exists()
    assert not (workspace_dir / "config").exists()


def test_agent_package_manager_fetches_nacos_agentspec_package(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    requests: list[tuple[str, dict[str, list[str]]]] = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urlparse(self.path)
            query = parse_qs(parsed.query)
            requests.append((parsed.path, query))
            if parsed.path != "/nacos/v3/client/ai/agentspecs":
                self.send_response(404)
                self.end_headers()
                return
            body = {
                "code": 0,
                "message": "success",
                "data": {
                    "namespaceId": query["namespaceId"][0],
                    "name": query["name"][0],
                    "description": "demo",
                    "content": '{"version":"1.0"}',
                    "resource": {
                        "agents": {
                            "name": "AGENTS.md",
                            "type": "config",
                            "content": "agent package nacos\n",
                        },
                        "soul": {
                            "name": "SOUL.md",
                            "type": "config",
                            "content": "soul nacos\n",
                        },
                        "skill": {
                            "name": "code-review/SKILL.md",
                            "type": "skills",
                            "content": "skill nacos\n",
                        },
                    },
                },
            }
            data = json.dumps(body).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *_args):
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
        applied = manager.apply(
            _runtime_config_ref(
                tmp_path,
                f"nacos://{server.server_address[0]}:{server.server_address[1]}/public/dev-worker/1.0.0",
                "1",
            )
        )
    finally:
        server.shutdown()
        thread.join(timeout=5)

    assert applied == manager.current_dir
    assert requests == [
        (
            "/nacos/v3/client/ai/agentspecs",
            {"namespaceId": ["public"], "name": ["dev-worker"], "version": ["1.0.0"]},
        )
    ]
    assert (manager.current_dir / "manifest.json").read_text(encoding="utf-8").strip() == '{\n  "version": "1.0"\n}'
    versioned_manifest = (
        tmp_path
        / "packages"
        / "downloads"
        / "nacos"
        / "public"
        / "dev-worker"
        / "version-1.0.0"
        / "dev-worker"
        / "manifest.json"
    )
    assert versioned_manifest.is_file()
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package nacos\n"
    assert (workspace_dir / "SOUL.md").read_text(encoding="utf-8") == "soul nacos\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill nacos\n"


def test_agent_package_manager_fetches_nacos_agentspec_with_user_password(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    seen_auth = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            if self.path != "/nacos/v3/auth/user/login":
                self.send_response(404)
                self.end_headers()
                return
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            data = json.dumps({"accessToken": "nacos-token", "tokenTtl": 3600}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            seen_auth.append(self.headers.get("Authorization"))
            body = {
                "code": 0,
                "message": "success",
                "data": {
                    "namespaceId": "public",
                    "name": "dev-worker",
                    "content": '{"version":"1.0"}',
                    "resource": {
                        "agents": {
                            "name": "AGENTS.md",
                            "type": "config",
                            "content": "agent package nacos auth\n",
                        },
                    },
                },
            }
            data = json.dumps(body).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *_args):
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
        manager.apply(
            _runtime_config_ref(
                tmp_path,
                f"nacos://user:pass@{server.server_address[0]}:{server.server_address[1]}/public/dev-worker",
                "1",
            )
        )
    finally:
        server.shutdown()
        thread.join(timeout=5)

    assert seen_auth == ["Bearer nacos-token"]
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package nacos auth\n"


def test_agent_package_manager_fetches_nacos_agentspec_with_sts_agentteams_url_query(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    seen_run = []

    def fake_run(command, check, capture_output, text):
        seen_run.append(
            {
                "command": command,
                "check": check,
                "capture_output": capture_output,
                "text": text,
            }
        )
        output_dir = Path(command[command.index("-o") + 1])
        spec_name = command[command.index("agentspec-get") + 1]
        package_dir = output_dir / spec_name
        config_dir = package_dir / "config"
        config_dir.mkdir(parents=True)
        (package_dir / "manifest.json").write_text('{"version":"1.2.0"}\n', encoding="utf-8")
        (config_dir / "AGENTS.md").write_text("agent package nacos sts\n", encoding="utf-8")
        return subprocess.CompletedProcess(command, 0, stdout="", stderr="")

    monkeypatch.setattr("qwenpaw_worker.update.subprocess.run", fake_run)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    monkeypatch.setattr(
        manager,
        "_fetch_controller_sts",
        lambda: {
            "access_key_id": "sts-ak",
            "access_key_secret": "sts-sk",
            "security_token": "sts-token",
        },
    )
    manager.apply(
        _runtime_config_ref(
            tmp_path,
            "nacos://ai-registry-poc.mse.cn-hangzhou.aliyuncs.com:80/"
            "95d1a6bb-a3aa-4af0-936e-8447c9156f3b/github-manager-rocketmq"
            "?version=0.0.2&authType=sts-agentteams",
            "1",
        )
    )

    assert seen_run == [
        {
            "command": [
                "nacos-cli",
                "--host",
                "ai-registry-poc.mse.cn-hangzhou.aliyuncs.com",
                "--port",
                "80",
                "--namespace",
                "95d1a6bb-a3aa-4af0-936e-8447c9156f3b",
                "--auth-type",
                "sts-agentteams",
                "--access-key",
                "sts-ak",
                "--secret-key",
                "sts-sk",
                "--security-token",
                "sts-token",
                "agentspec-get",
                "github-manager-rocketmq",
                "-o",
                str(
                    tmp_path
                    / "packages"
                    / "downloads"
                    / "nacos"
                    / "95d1a6bb-a3aa-4af0-936e-8447c9156f3b"
                    / "github-manager-rocketmq"
                    / "version-0.0.2"
                ),
                "--version",
                "0.0.2",
            ],
            "check": True,
            "capture_output": True,
            "text": True,
        }
    ]
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package nacos sts\n"


def test_agent_package_manager_versions_nacos_download_dirs(tmp_path: Path) -> None:
    manager = AgentPackageManager(tmp_path / "packages")

    version_1 = manager._nacos_download_output_dir("public", "dev-worker", "1.0.0", "")
    version_2 = manager._nacos_download_output_dir("public", "dev-worker", "2.0.0", "")

    assert version_1 != version_2
    assert version_1.parts[-2:] == ("dev-worker", "version-1.0.0")
    assert version_2.parts[-2:] == ("dev-worker", "version-2.0.0")


def test_agent_package_manager_keeps_workspace_when_desired_package_is_empty(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    applied = manager.apply(_runtime_config_without_package(tmp_path))

    assert applied is None
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill 1\n"
    assert manager.marker_path.read_text(encoding="utf-8").splitlines() == [
        f"file://{package_v1}",
        "dev-worker",
        "1",
        "sha256:1",
    ]


def test_agent_package_update_overwrites_runtime_edits_to_package_seed(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")
    package_v2 = _package(tmp_path, "2")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    (workspace_dir / "AGENTS.md").write_text("agent runtime edit\n", encoding="utf-8")
    (workspace_dir / "skills" / "code-review" / "SKILL.md").write_text("agent skill edit\n", encoding="utf-8")

    manager.apply(_runtime_config(tmp_path, package_v2, "2"))

    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 2\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill 2\n"


def test_agent_package_update_does_not_overwrite_runtime_teams_md(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1", include_teams=True)
    teams_md = workspace_dir / "TEAMS.md"

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    teams_md.write_text("runtime managed teams\n", encoding="utf-8")
    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    assert teams_md.read_text(encoding="utf-8") == "runtime managed teams\n"


def test_agent_package_update_does_not_delete_runtime_teams_md_from_old_package(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1", include_teams=True)
    package_v2 = _empty_package(tmp_path, "2")
    teams_md = workspace_dir / "TEAMS.md"

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    teams_md.write_text("runtime managed teams\n", encoding="utf-8")
    manager.apply(_runtime_config(tmp_path, package_v2, "2"))

    assert teams_md.read_text(encoding="utf-8") == "runtime managed teams\n"


def test_agent_package_update_clears_prompt_files_missing_from_new_package(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")
    package_v2 = _empty_package(tmp_path, "2")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))
    manager.apply(_runtime_config(tmp_path, package_v2, "2"))

    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == ""
    assert (workspace_dir / "SOUL.md").read_text(encoding="utf-8") == ""
    assert not (workspace_dir / "MEMORY.md").exists()
    assert not (workspace_dir / "skills" / "code-review").exists()
    assert not (workspace_dir / "skill.json").exists()


def test_agent_package_manager_creates_empty_prompt_files_when_package_has_none(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package = _empty_package(tmp_path, "empty")

    manager.apply(_runtime_config(tmp_path, package, "empty"))

    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == ""
    assert (workspace_dir / "SOUL.md").read_text(encoding="utf-8") == ""


def test_agent_package_manager_reconciles_qwenpaw_workspace_skill_manifest(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    calls: list[Path] = []
    registry_module = types.ModuleType("qwenpaw.agents.skill_system.registry")
    registry_module.reconcile_workspace_manifest = lambda path: calls.append(path)

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents", types.ModuleType("qwenpaw.agents"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system", types.ModuleType("qwenpaw.agents.skill_system"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system.registry", registry_module)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    assert calls == [workspace_dir]


def test_agent_package_manager_enables_package_skills_in_workspace_manifest(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"

    def reconcile_workspace_manifest(path):
        manifest = path / "skill.json"
        manifest.write_text(
            json.dumps(
                {
                    "schema_version": "workspace-skill-manifest.v1",
                    "skills": {"code-review": {"enabled": False}},
                }
            ),
            encoding="utf-8",
        )

    registry_module = types.ModuleType("qwenpaw.agents.skill_system.registry")
    registry_module.reconcile_workspace_manifest = reconcile_workspace_manifest

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents", types.ModuleType("qwenpaw.agents"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system", types.ModuleType("qwenpaw.agents.skill_system"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system.registry", registry_module)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    manifest = json.loads((workspace_dir / "skill.json").read_text(encoding="utf-8"))
    assert manifest["skills"]["code-review"]["enabled"] is True


def test_agent_package_manager_rolls_back_workspace_and_current_when_workspace_apply_fails(
    tmp_path: Path,
    monkeypatch,
) -> None:
    workspace_dir = tmp_path / "workspace"
    registry_module = types.ModuleType("qwenpaw.agents.skill_system.registry")
    calls = 0

    def reconcile_or_fail(_path):
        nonlocal calls
        calls += 1
        if calls == 2:
            raise RuntimeError("skill manifest failed")

    registry_module.reconcile_workspace_manifest = reconcile_or_fail

    monkeypatch.setitem(sys.modules, "qwenpaw", types.ModuleType("qwenpaw"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents", types.ModuleType("qwenpaw.agents"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system", types.ModuleType("qwenpaw.agents.skill_system"))
    monkeypatch.setitem(sys.modules, "qwenpaw.agents.skill_system.registry", registry_module)

    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")
    package_v2 = _package(tmp_path, "2")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    try:
        manager.apply(_runtime_config(tmp_path, package_v2, "2"))
    except RuntimeError as exc:
        assert str(exc) == "skill manifest failed"
    else:
        raise AssertionError("expected workspace apply failure")

    assert (manager.current_dir / "config" / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "SOUL.md").read_text(encoding="utf-8") == "soul 1\n"
    assert (workspace_dir / "MEMORY.md").read_text(encoding="utf-8") == "memory 1\n"
    assert (workspace_dir / "skills" / "code-review" / "SKILL.md").read_text(encoding="utf-8") == "skill 1\n"
    assert manager.marker_path.read_text(encoding="utf-8").splitlines() == [
        f"file://{package_v1}",
        "dev-worker",
        "1",
        "sha256:1",
    ]


def test_agent_package_manager_keeps_previous_version_when_new_package_is_invalid(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")
    broken_package = tmp_path / "broken-package.txt"
    broken_package.write_text("not an agent package archive\n", encoding="utf-8")

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    try:
        manager.apply(_runtime_config(tmp_path, broken_package, "2"))
    except RuntimeError as exc:
        assert "unsupported agent package format" in str(exc)
    else:
        raise AssertionError("expected invalid package failure")

    assert (manager.current_dir / "config" / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert manager.marker_path.read_text(encoding="utf-8").splitlines() == [
        f"file://{package_v1}",
        "dev-worker",
        "1",
        "sha256:1",
    ]


def test_agent_package_manager_keeps_previous_version_when_mcp_json_is_invalid(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    package_v1 = _package(tmp_path, "1")
    package_v2 = _package(
        tmp_path,
        "2",
        mcp_json='{"mcpServers":{"docs":{"url":"https://docs.example.com/mcp",}}}\n',
    )

    manager.apply(_runtime_config(tmp_path, package_v1, "1"))

    try:
        manager.apply(_runtime_config(tmp_path, package_v2, "2"))
    except RuntimeError as exc:
        assert "agent package mcp.json is invalid JSON" in str(exc)
    else:
        raise AssertionError("expected invalid mcp.json failure")

    assert (manager.current_dir / "config" / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert (workspace_dir / "AGENTS.md").read_text(encoding="utf-8") == "agent package 1\n"
    assert manager.marker_path.read_text(encoding="utf-8").splitlines() == [
        f"file://{package_v1}",
        "dev-worker",
        "1",
        "sha256:1",
    ]


def test_agent_package_manager_rejects_archive_path_traversal(tmp_path: Path) -> None:
    workspace_dir = tmp_path / "workspace"
    manager = AgentPackageManager(tmp_path / "packages", workspace_dir=workspace_dir)
    zip_path = tmp_path / "evil.zip"
    with zipfile.ZipFile(zip_path, "w") as archive:
        archive.writestr("../staging-evil/evil.txt", "escape\n")

    try:
        manager.apply(_runtime_config(tmp_path, zip_path, "1"))
    except RuntimeError as exc:
        assert "unsafe agent package path" in str(exc)
    else:
        raise AssertionError("expected unsafe archive path failure")

    assert not (tmp_path / "packages" / "staging-evil").exists()
