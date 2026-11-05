"""Hermes directory-plugin entrypoint for the AgentTeams Matrix adapter."""

from .adapter import register

__all__ = ["register"]
