import importlib.util
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]
PATCH_SCRIPT = ROOT / "scripts" / "patch-qwenpaw-defer-mcp-startup.py"


def _load_patch_script():
    spec = importlib.util.spec_from_file_location(
        "_patch_qwenpaw_defer_mcp_startup",
        PATCH_SCRIPT,
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def test_defer_mcp_startup_patch_is_applied_during_image_build() -> None:
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")

    assert "patch-qwenpaw-defer-mcp-startup.py" in dockerfile
    assert "/opt/venv/qwenpaw/bin/python /tmp/patch-qwenpaw-defer-mcp-startup.py" in dockerfile
    assert dockerfile.index("install-builtin-qwenpaw-plugins.py") < dockerfile.index(
        "patch-qwenpaw-defer-mcp-startup.py",
    )


def test_defer_mcp_startup_patch_enables_native_workspace_flag() -> None:
    module = _load_patch_script()
    source = """
        instance = Workspace(
            agent_id=agent_id,
            workspace_dir=agent_ref.workspace_dir,
        )

        new_instance = Workspace(
            agent_id=agent_id,
            workspace_dir=agent_ref.workspace_dir,
        )
"""

    patched = module.patch_source(source)

    assert patched.count("defer_mcp_startup=True") == 2
    assert patched == module.patch_source(patched)


def test_defer_mcp_startup_patch_preserves_mcp_connect_timeout() -> None:
    module = _load_patch_script()
    source = """
            if getattr(ws, "defer_mcp_startup", False):
                mcp.init_from_config_background(ws._config.mcp)
                logger.debug(
                    f"MCP initialization deferred for agent: {ws.agent_id}",
                )
"""

    patched = module.patch_service_factories_source(source)

    assert "mcp.init_from_config_background(" in patched
    assert "timeout=60.0" in patched
    assert patched == module.patch_service_factories_source(patched)


def test_defer_mcp_startup_patch_fails_when_qwenpaw_shape_changes() -> None:
    module = _load_patch_script()

    with pytest.raises(RuntimeError, match="shape changed"):
        module.patch_source("instance = Workspace(agent_id=agent_id)\n")


def test_defer_mcp_startup_patch_fails_when_service_factory_changes() -> None:
    module = _load_patch_script()

    with pytest.raises(RuntimeError, match="shape changed"):
        module.patch_service_factories_source("mcp.init_from_config(config)\n")
