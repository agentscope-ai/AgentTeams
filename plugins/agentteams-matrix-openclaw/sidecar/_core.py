"""Load the Matrix core library from the checkout the operator points at.

The core (``agentteams-matrix``) is a separate repository and is never
vendored into this plugin. ``MATRIX_CORE_DIR`` must point at a checkout
whose root contains the ``core`` package (the layout of the
``agentteams-matrix`` plugin directory). The core is imported lazily so
this package stays importable — and its config/CLI code testable —
without the core present, while a production start still fails closed
with an actionable message when the core is missing or cannot be
imported.
"""

from __future__ import annotations

import dataclasses
import os
import sys
import types

_CACHE_ATTR = "_agentteams_core_lib"


class CoreImportError(RuntimeError):
    """The Matrix core could not be loaded from MATRIX_CORE_DIR."""


@dataclasses.dataclass(frozen=True)
class CoreLib:
    """The loaded core package plus its transport module."""

    core: types.ModuleType
    transport: types.ModuleType


def core_dir() -> str | None:
    """The MATRIX_CORE_DIR value, or None when unset/empty."""
    return os.environ.get("MATRIX_CORE_DIR", "") or None


def load_core() -> CoreLib:
    """Import and return the core from MATRIX_CORE_DIR.

    Raises :class:`CoreImportError` with an actionable message when the
    variable is unset, the path does not contain the core package, or
    the import itself fails. The result is cached so the sys.path
    insertion happens once per process.
    """
    cached = globals().get(_CACHE_ATTR)
    if cached is not None:
        return cached

    directory = core_dir()
    if directory is None:
        raise CoreImportError(
            "MATRIX_CORE_DIR is not set. Point it at a checkout of the "
            "agentteams-matrix core (the directory that contains the "
            "core/ package) and retry."
        )
    if not os.path.isfile(os.path.join(directory, "core", "__init__.py")):
        raise CoreImportError(
            f"MATRIX_CORE_DIR={directory!r} does not contain the core "
            "package (core/__init__.py); point it at the "
            "agentteams-matrix plugin checkout."
        )
    if directory not in sys.path:
        sys.path.insert(0, directory)
    try:
        import core

        from core import transport as core_transport
    except Exception as exc:
        raise CoreImportError(
            f"cannot import the matrix core from MATRIX_CORE_DIR="
            f"{directory!r}: {exc!r} — the core checkout needs its own "
            "dependencies installed (matrix-nio)"
        ) from exc

    lib = CoreLib(core=core, transport=core_transport)
    globals()[_CACHE_ATTR] = lib
    return lib
