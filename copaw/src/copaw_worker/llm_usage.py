"""Track LLM HTTP calls routed through configured Higress gateway base URLs."""

from __future__ import annotations

import logging
import threading
from typing import Any, Iterable
from urllib.parse import urlparse

logger = logging.getLogger(__name__)

_LLM_PATH_MARKERS = (
    "/chat/completions",
    "/completions",
    "/embeddings",
    "/audio/transcriptions",
    "/images/generations",
)

_HOOK_INSTALLED = False
_tracker = None


class LLMUsageTracker:
    """Thread-safe counter for gateway LLM HTTP calls since the last report."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._since_last_report = 0
        self._gateway_prefixes: tuple[str, ...] = ()

    def configure_gateway_prefixes(self, prefixes: Iterable[str]) -> None:
        normalized = tuple(sorted({str(prefix).rstrip("/") for prefix in prefixes if prefix}))
        with self._lock:
            self._gateway_prefixes = normalized

    def note_request(self, method: str, url: str) -> None:
        if method.upper() != "POST":
            return
        if not _is_llm_gateway_call(url, self._gateway_prefixes):
            return
        with self._lock:
            self._since_last_report += 1

    def take_for_report(self) -> int:
        with self._lock:
            count = self._since_last_report
            self._since_last_report = 0
            return count

    def restore_after_failed_report(self, count: int) -> None:
        if count <= 0:
            return
        with self._lock:
            self._since_last_report += count


def get_llm_usage_tracker() -> LLMUsageTracker:
    global _tracker
    if _tracker is None:
        _tracker = LLMUsageTracker()
    return _tracker


def gateway_prefixes_from_openclaw(openclaw_cfg: dict[str, Any]) -> list[str]:
    providers = openclaw_cfg.get("models", {}).get("providers", {})
    if not isinstance(providers, dict):
        return []
    prefixes: list[str] = []
    for provider_cfg in providers.values():
        if not isinstance(provider_cfg, dict):
            continue
        base_url = str(provider_cfg.get("baseUrl") or "").strip()
        if base_url:
            prefixes.append(base_url.rstrip("/"))
    return prefixes


def configure_llm_usage_from_openclaw(openclaw_cfg: dict[str, Any]) -> None:
    prefixes = gateway_prefixes_from_openclaw(openclaw_cfg)
    get_llm_usage_tracker().configure_gateway_prefixes(prefixes)
    logger.info(
        "configured LLM usage tracker with %s gateway prefix(es)",
        len(prefixes),
    )


def install_llm_usage_hooks() -> None:
    """Patch httpx clients used by CoPaw provider calls."""
    global _HOOK_INSTALLED
    if _HOOK_INSTALLED:
        return

    import httpx

    tracker = get_llm_usage_tracker()

    original_async_request = httpx.AsyncClient.request
    original_sync_request = httpx.Client.request

    async def async_request(self, method: str, url: str, *args: Any, **kwargs: Any):
        tracker.note_request(method, str(url))
        return await original_async_request(self, method, url, *args, **kwargs)

    def sync_request(self, method: str, url: str, *args: Any, **kwargs: Any):
        tracker.note_request(method, str(url))
        return original_sync_request(self, method, url, *args, **kwargs)

    httpx.AsyncClient.request = async_request  # type: ignore[method-assign]
    httpx.Client.request = sync_request  # type: ignore[method-assign]

    _HOOK_INSTALLED = True
    logger.info("Installed CoPaw LLM usage httpx hooks")


def _is_llm_gateway_call(url: str, gateway_prefixes: tuple[str, ...]) -> bool:
    if not gateway_prefixes:
        return False
    text = str(url or "").strip()
    if not text:
        return False
    parsed = urlparse(text)
    path = parsed.path or ""
    if not any(marker in path for marker in _LLM_PATH_MARKERS):
        return False
    normalized = text.rstrip("/")
    for prefix in gateway_prefixes:
        if normalized.startswith(prefix):
            return True
        prefix_parsed = urlparse(prefix)
        if prefix_parsed.netloc and parsed.netloc == prefix_parsed.netloc:
            prefix_path = prefix_parsed.path.rstrip("/")
            if prefix_path and path.startswith(prefix_path):
                return True
    return False
