"""#11: _resolve_context_window must tolerate a bad contextWindow value.

Kept in a standalone module (rather than test_bridge.py, which is a pre-existing
uncollectable file that imports symbols no longer present in bridge.py) so these
tests actually run. They import only the helper under test.
"""

from copaw_worker.bridge import _resolve_context_window


def _cfg(context_window):
    return {
        "models": {
            "providers": {
                "gw": {"models": [{"id": "m1", "contextWindow": context_window}]}
            }
        }
    }


def test_non_numeric_context_window_returns_none():
    # A malformed provider entry (e.g. "128k") must not crash config bridging.
    assert _resolve_context_window(_cfg("not-a-number")) is None


def test_null_context_window_returns_none():
    assert _resolve_context_window(_cfg(None)) is None


def test_valid_context_window_parses():
    assert _resolve_context_window(_cfg(128000)) == 128000
    assert _resolve_context_window(_cfg("64000")) == 64000


def test_missing_context_window_returns_none():
    cfg = {"models": {"providers": {"gw": {"models": [{"id": "m1"}]}}}}
    assert _resolve_context_window(cfg) is None
