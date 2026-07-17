# -*- coding: utf-8 -*-
"""
AgentTeams Matrix channel overlay for CoPaw.

This module replaces CoPaw's built-in matrix channel with AgentTeams-specific
enhancements (E2EE, history buffering, mention handling) until they are
merged upstream.

Package layout (Phase 7):
- ``channel.py`` — unified MatrixChannel (Manager + Worker)
- ``relations.py`` — thread/replay helpers (no ``copaw_worker`` imports)
- ``outbound_policy.py`` — Team Leader / NO_REPLY send filtering
- ``paths.py`` — ``runtime_root()`` for runtime.yaml lookup
- ``config.py`` — vendored CoPaw config overlay (**frozen**, X7.4)
"""
from .channel import MatrixChannel

__all__ = ["MatrixChannel"]
