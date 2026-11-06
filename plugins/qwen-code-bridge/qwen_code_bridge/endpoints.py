# -*- coding: utf-8 -*-
"""Endpoint resolution — never hardcode host/port when it can be observed.

Priority: explicit argument > ``QWENPAW_APP_URL`` env > the request's own
base URL > ``QWENPAW_PORT`` env > QwenPaw config ``app.port`` > 8088.
"""

from __future__ import annotations

import os

APP_URL_ENV = "QWENPAW_APP_URL"
APP_PORT_ENV = "QWENPAW_PORT"
_FALLBACK_PORT = 8088


def _config_port() -> str:
    try:
        from qwenpaw.config.utils import load_config

        config = load_config()
        app = getattr(config, "app", None)
        return str(getattr(app, "port", None) or "").strip()
    except Exception:
        return ""


def app_port() -> str:
    return (
        os.environ.get(APP_PORT_ENV, "").strip()
        or _config_port()
        or str(_FALLBACK_PORT)
    )


def app_base(explicit: str | None = None, request=None) -> str:
    """Resolve the QwenPaw app base URL for self-calls."""
    if explicit and str(explicit).strip():
        return str(explicit).strip().rstrip("/")
    env = os.environ.get(APP_URL_ENV, "").strip()
    if env:
        return env.rstrip("/")
    if request is not None:
        try:
            base = str(request.base_url).strip()
            if base:
                return base.rstrip("/")
        except Exception:
            pass
    return f"http://127.0.0.1:{app_port()}"


__all__ = ["app_base", "app_port", "APP_URL_ENV", "APP_PORT_ENV"]
