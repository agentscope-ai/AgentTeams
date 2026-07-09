#!/usr/bin/env python3
"""Enable QwenPaw's native deferred MCP startup for app workspaces."""

from __future__ import annotations

from pathlib import Path
import sys


MULTI_AGENT_MANAGER_PATCHES = (
    (
        """        instance = Workspace(
            agent_id=agent_id,
            workspace_dir=agent_ref.workspace_dir,
        )
""",
        """        instance = Workspace(
            agent_id=agent_id,
            workspace_dir=agent_ref.workspace_dir,
            defer_mcp_startup=True,
        )
""",
    ),
    (
        """        new_instance = Workspace(
            agent_id=agent_id,
            workspace_dir=agent_ref.workspace_dir,
        )
""",
        """        new_instance = Workspace(
            agent_id=agent_id,
            workspace_dir=agent_ref.workspace_dir,
            defer_mcp_startup=True,
        )
""",
    ),
)


SERVICE_FACTORIES_PATCHES = (
    (
        """                mcp.init_from_config_background(ws._config.mcp)
""",
        """                mcp.init_from_config_background(
                    ws._config.mcp,
                    timeout=60.0,
                )
""",
    ),
)


def _apply_patches(source: str, patches, component: str) -> str:
    patched = source
    for before, after in patches:
        if after in patched:
            continue
        if before not in patched:
            raise RuntimeError(
                f"QwenPaw {component} shape changed; "
                "cannot enable deferred MCP startup safely",
            )
        patched = patched.replace(before, after, 1)
    return patched


def patch_multi_agent_manager_source(source: str) -> str:
    return _apply_patches(
        source,
        MULTI_AGENT_MANAGER_PATCHES,
        "MultiAgentManager",
    )


def patch_service_factories_source(source: str) -> str:
    return _apply_patches(
        source,
        SERVICE_FACTORIES_PATCHES,
        "workspace service factories",
    )


def patch_source(source: str) -> str:
    return patch_multi_agent_manager_source(source)


def _patch_file(target: Path, patcher) -> None:
    source = target.read_text(encoding="utf-8")
    patched = patcher(source)
    if patched != source:
        target.write_text(patched, encoding="utf-8")


def main() -> int:
    app_dir = (
        Path(sys.prefix)
        / "lib"
        / "python3.11"
        / "site-packages"
        / "qwenpaw"
        / "app"
    )
    _patch_file(app_dir / "multi_agent_manager.py", patch_source)
    _patch_file(
        app_dir / "workspace" / "service_factories.py",
        patch_service_factories_source,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
