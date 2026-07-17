"""AgentTeams Matrix overlay path resolution."""

from __future__ import annotations

import os
from pathlib import Path


def runtime_root() -> Path:
    """Return the AgentTeams L2 worker root (parent of ``.copaw``)."""
    configured = os.environ.get("COPAW_WORKING_DIR")
    if configured:
        path = Path(configured).expanduser().resolve()
        if path.name == "default" and path.parent.name == "workspaces":
            copaw_dir = path.parent.parent
            if copaw_dir.name == ".copaw":
                return copaw_dir.parent
        if path.name == ".copaw":
            return path.parent
        return path.parent

    cwd = Path.cwd().resolve()
    if cwd.name == "default" and cwd.parent.name == "workspaces":
        copaw_dir = cwd.parent.parent
        if copaw_dir.name == ".copaw":
            return copaw_dir.parent
    return cwd
