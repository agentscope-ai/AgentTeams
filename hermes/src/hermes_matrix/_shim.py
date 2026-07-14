"""Shim installed as ``gateway.platforms.matrix`` inside hermes-agent.

The image build renames hermes-agent's original Matrix module to
``gateway.platforms._matrix_native``.  This shim keeps the module path stable
for ``gateway.run._create_adapter`` while swapping in AgentTeams's subclassed
``MatrixAdapter``.
"""
from __future__ import annotations

from gateway.platforms import _matrix_native as _native
from gateway.platforms._matrix_native import *  # noqa: F401,F403

from hermes_matrix.adapter import MatrixAdapter

check_matrix_requirements = _native.check_matrix_requirements


def __getattr__(name: str):
    return getattr(_native, name)


def __dir__() -> list[str]:
    return sorted(set(globals()) | set(dir(_native)))
