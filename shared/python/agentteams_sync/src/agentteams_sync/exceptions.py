"""Sync-layer exceptions shared across worker runtimes."""


class BridgeRuntimeError(RuntimeError):
    """Runtime-to-standard bridge failed before storage persistence."""
