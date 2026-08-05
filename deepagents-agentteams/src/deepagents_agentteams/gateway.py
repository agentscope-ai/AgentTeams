"""Higress model and MCP client configuration."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from typing import TYPE_CHECKING, Any, TypeVar
from urllib.parse import urlsplit, urlunsplit

from deepagents_agentteams.config import ConfigError, ModelConfig

if TYPE_CHECKING:
    from langchain_openai import ChatOpenAI

T = TypeVar("T")


def build_higress_model(
    config: ModelConfig,
    *,
    model_factory: Callable[..., T] | None = None,
) -> T | ChatOpenAI:
    """Build an OpenAI-compatible chat model routed through Higress."""
    if model_factory is None:
        from langchain_openai import ChatOpenAI

        model_factory = ChatOpenAI
    kwargs: dict[str, Any] = {
        "model": config.name,
        "base_url": canonical_openai_base_url(config.gateway_url),
        "api_key": config.gateway_key,
        "use_responses_api": False,
    }
    return model_factory(**kwargs)


def canonical_openai_base_url(gateway_url: str) -> str:
    """Normalize legacy gateway origins while preserving explicit provider paths."""
    parsed = urlsplit(gateway_url)
    if parsed.scheme and parsed.netloc and not parsed.path.strip("/"):
        return urlunsplit((parsed.scheme, parsed.netloc, "/v1", parsed.query, parsed.fragment))
    return gateway_url


def build_mcp_connections(
    servers: Sequence[Mapping[str, object]],
    *,
    gateway_key: str,
) -> dict[str, dict[str, object]]:
    """Translate AgentTeams MCP declarations to LangChain MCP connections."""
    connections: dict[str, dict[str, object]] = {}
    for server in servers:
        name = server.get("name")
        url = server.get("url")
        transport = server.get("transport") or "http"
        if not isinstance(name, str) or not name:
            raise ConfigError("MCP server name must be a non-empty string")
        if not isinstance(url, str) or not url:
            raise ConfigError(f"MCP server {name} URL must be a non-empty string")
        if transport not in {"http", "sse"}:
            raise ConfigError(f"MCP server {name} transport must be http or sse")
        mapped_transport = "streamable_http" if transport == "http" else transport
        connections[name] = {
            "url": url,
            "transport": mapped_transport,
            "headers": {"Authorization": f"Bearer {gateway_key}"},
        }
    return connections
