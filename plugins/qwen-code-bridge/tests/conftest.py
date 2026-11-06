# -*- coding: utf-8 -*-
"""Make the plugin root (``qwen_code_bridge`` package) importable."""

import os
import sys
import urllib.parse
import urllib.request
from pathlib import Path

import pytest

_PLUGIN_ROOT = str(Path(__file__).resolve().parent.parent)
if _PLUGIN_ROOT not in sys.path:
    sys.path.insert(0, _PLUGIN_ROOT)

from qwen_code_bridge import state  # noqa: E402

#: Suite-level escape hatch; must stay unset in normal runs.
_ALLOW_NET_ENV = "QWEN_CODE_BRIDGE_TEST_ALLOW_NET"
#: Hosts tests may still reach: servers the suite starts for itself.
_LOOPBACK_HOSTS = {"127.0.0.1", "localhost", "::1"}


@pytest.fixture(autouse=True)
def _isolate_state_dir(tmp_path, monkeypatch):
    """Suite guard: every test resolves plugin state under ``tmp_path``.

    Guards against the 19:43 wake leak: during deny-wake development a
    mid-version test wrote a real wake into the live queue, the live
    plugin delivered it to the fake session
    ``matrix:!wake42:matrix.example``, and QwenPaw opened a chat and ran
    a full agent turn for it.  The legacy override
    (``QWENPAW_CODING_CLI_STATE_DIR``) is pointed at a per-test directory
    so any state/watchdog/wake-queue write lands in ``tmp_path`` instead
    of the live tree.  The primary override is pinned to the same
    directory only when it is preset externally (otherwise it would
    shadow tests that isolate themselves via the legacy name); a dirty
    outer environment still cannot reach the live tree.  ``monkeypatch``
    restores the original environment after each test.
    """
    guard = str(tmp_path / "state-guard")
    monkeypatch.setenv(state.LEGACY_STATE_DIR_ENV, guard)
    if state.STATE_DIR_ENV in os.environ:
        monkeypatch.setenv(state.STATE_DIR_ENV, guard)
    yield


#: Fallback when the live app port cannot be resolved; the app default.
_LIVE_APP_FALLBACK_PORT = 8088


@pytest.fixture(autouse=True)
def _block_real_http(monkeypatch):
    """Suite guard: no test may send HTTP to the live QwenPaw app.

    Guards against the 19:43 wake leak: that incident started when a
    mid-version test let a real ``urlopen`` POST a wake straight to the
    live QwenPaw app at ``127.0.0.1:8088``; the live plugin delivered it
    to the fake session ``matrix:!wake42:matrix.example``, and QwenPaw
    opened a chat and ran a full agent turn for it.  The leak went
    through loopback, so "loopback is fine" was never safe:

    * a non-loopback target always raises;
    * a loopback target on the live app's own port also raises —
      loopback is only allowed for ports the tests start for
      themselves (the wake-queue drain and web-proxy tests bind
      ``127.0.0.1:0``, a random free port, and must poll their own
      server); the live app's port is refused by default.

    The live port is resolved with the same logic the plugin uses
    (``endpoints.app_base()``) at fixture setup — before any test can
    monkeypatch the environment — and falls back to ``8088`` if it
    cannot be resolved.  A test may still install its own fake first
    (its ``monkeypatch.setattr`` takes effect after this fixture), and
    the suite may be run with ``QWEN_CODE_BRIDGE_TEST_ALLOW_NET=1``.
    """
    if os.environ.get(_ALLOW_NET_ENV) == "1":
        yield
        return

    try:
        from qwen_code_bridge import endpoints

        live_port = (
            urllib.parse.urlparse(endpoints.app_base()).port
            or _LIVE_APP_FALLBACK_PORT
        )
    except Exception:
        live_port = _LIVE_APP_FALLBACK_PORT

    real_urlopen = urllib.request.urlopen

    def _guarded(url, *args, **kwargs):
        target = url.full_url if isinstance(url, urllib.request.Request) else url
        parsed = urllib.parse.urlparse(str(target))
        if parsed.hostname not in _LOOPBACK_HOSTS:
            raise RuntimeError("blocked real HTTP in tests")
        if parsed.port == live_port:
            raise RuntimeError(
                "blocked real HTTP in tests: "
                f"target is the live QwenPaw app on port {live_port}"
            )
        return real_urlopen(url, *args, **kwargs)

    monkeypatch.setattr(urllib.request, "urlopen", _guarded)
    yield
