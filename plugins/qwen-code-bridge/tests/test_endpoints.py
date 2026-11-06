# -*- coding: utf-8 -*-
"""Endpoint resolution: never rely on a hardcoded host/port."""

from __future__ import annotations

from qwen_code_bridge import endpoints


def test_explicit_wins(monkeypatch) -> None:
    monkeypatch.setenv(endpoints.APP_URL_ENV, "http://env:1")
    assert endpoints.app_base("http://explicit:9") == "http://explicit:9"


def test_env_url(monkeypatch) -> None:
    monkeypatch.setenv(endpoints.APP_URL_ENV, "http://env:1/")
    assert endpoints.app_base() == "http://env:1"


def test_request_base(monkeypatch) -> None:
    monkeypatch.delenv(endpoints.APP_URL_ENV, raising=False)

    class Req:
        base_url = "http://host.docker.internal:18088/"

    assert endpoints.app_base(None, Req()) == "http://host.docker.internal:18088"


def test_port_env_and_fallback(monkeypatch) -> None:
    monkeypatch.delenv(endpoints.APP_URL_ENV, raising=False)
    monkeypatch.setenv(endpoints.APP_PORT_ENV, "19000")
    assert endpoints.app_base() == "http://127.0.0.1:19000"
    monkeypatch.delenv(endpoints.APP_PORT_ENV, raising=False)
    base = endpoints.app_base()
    assert base.startswith("http://127.0.0.1:")
