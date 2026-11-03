# Cross-copy name binding for the teamharness MCP package (W1-a2, issue #1451 P6)
from __future__ import annotations

from typing import Any

"""server.py may be loaded several times into distinct module objects (the
stdio entry, ``mcp.server``, and each test harness), and the tests
monkeypatch a handful of call sites on whichever copy they drive.  Those
call sites therefore must not bind to a fixed module global: they go
through :func:`proxy`, which resolves the name at call time against the
server copy currently bound via :func:`bind` (honouring whatever the test
installed on that copy), falling back to the object registered with
:func:`define` and finally to the registered fragment modules.

Only the test-patched call sites (plus the server-owned ``_role`` and
``_transition_task``) use this indirection; every other cross-module
reference is a plain import.
"""

_current: dict[str, Any] | None = None
_defined: dict[str, Any] = {}
_modules: tuple[Any, ...] = ()


def bind(globals_map: dict[str, Any]) -> None:
    """Point name resolution at the server copy whose code is running."""
    global _current
    _current = globals_map


def define(name: str, obj: Any) -> None:
    """Register the canonical object for a resolved name."""
    _defined[name] = obj


def register_modules(modules: tuple[Any, ...]) -> None:
    global _modules
    _modules = tuple(modules)


def resolve(name: str) -> Any:
    if _current is not None:
        value = _current.get(name)
        if value is not None and getattr(value, "_refs_proxy_name", None) != name:
            return value
    if name in _defined:
        return _defined[name]
    for module in _modules:
        value = module.__dict__.get(name)
        # Skip a same-name proxy: registered modules hold refs proxies for
        # their cross-module call sites (e.g. taskflow's flow names), and
        # returning one would recurse forever.
        if value is not None and getattr(value, "_refs_proxy_name", None) != name:
            return value
    raise AttributeError(f"teamharness refs cannot resolve {name!r}")


class _RefsProxy:
    """Call-time indirection for one name in the bound server namespace."""

    _refs_proxy_name: str

    def __init__(self, name: str) -> None:
        self._refs_proxy_name = name
        self.__name__ = name
        self.__qualname__ = name
        self.__doc__ = f"refs proxy for {name!r}; see common.refs"

    def __call__(self, *args: Any, **kwargs: Any) -> Any:
        return resolve(self._refs_proxy_name)(*args, **kwargs)

    def __getattr__(self, attr: str) -> Any:
        # Attribute access on a proxy (e.g. MATRIX_ROOM_RE.fullmatch) is
        # forwarded to the object resolved at access time.
        return getattr(resolve(self._refs_proxy_name), attr)


def proxy(name: str) -> Any:
    return _RefsProxy(name)
