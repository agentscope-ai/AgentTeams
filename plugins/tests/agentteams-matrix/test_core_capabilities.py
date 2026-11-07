"""Runtime capability negotiation tests (MX-6, P9 sidecar).

The hello ``capabilities`` list is heterogeneous: a non-empty string
declares support, a single-key ``{name: bool}`` object declares an
explicit value (e.g. ``{"steer": false}``).  Consumers must degrade
(never raise) when a capability is absent or declared false.
"""

from __future__ import annotations

import json

import pytest

from core.interfaces import CoreConfig
from core.sidecar import (
    SidecarClient,
    SidecarProtocolError,
    SidecarServer,
    capability_supported,
    decode_line,
    make_hello,
    parse_capabilities,
)


@pytest.fixture()
def server(tmp_path):
    srv = SidecarServer(tmp_path / "sidecar.sock")
    srv.start()
    yield srv
    srv.stop()


# ---------------------------------------------------------------------------
# parse_capabilities: entry normalisation
# ---------------------------------------------------------------------------


def test_parse_string_entries_mean_supported():
    assert parse_capabilities([]) == {}
    assert parse_capabilities(["text", "media"]) == {"text": True, "media": True}


def test_parse_single_key_dict_entries_mean_explicit_value():
    assert parse_capabilities(["text", {"steer": False}]) == {
        "text": True,
        "steer": False,
    }
    assert parse_capabilities([{"steer": True}]) == {"steer": True}


@pytest.mark.parametrize(
    "entries",
    [
        [""],  # empty string entry
        [{"steer": "yes"}],  # non-bool value
        [{"steer": 1}],  # int is not bool (JSON true/false only)
        [{"": True}],  # empty dict key
        [{"a": True, "b": False}],  # multi-key dict
        [1],  # bare int
        [None],  # null entry
        ["text", [1]],  # nested list
    ],
)
def test_parse_rejects_invalid_entries(entries):
    with pytest.raises(SidecarProtocolError) as exc:
        parse_capabilities(entries)
    assert exc.value.code == "invalid_frame"


def test_parse_rejects_non_list():
    with pytest.raises(SidecarProtocolError):
        parse_capabilities("text")
    with pytest.raises(SidecarProtocolError):
        parse_capabilities({"steer": False})


# ---------------------------------------------------------------------------
# capability_supported: missing = False, never raises
# ---------------------------------------------------------------------------


def test_supported_accepts_raw_list_and_mapping():
    # Raw hello entry list.
    assert capability_supported(["text"], "text") is True
    assert capability_supported([{"steer": False}], "steer") is False
    assert capability_supported([{"steer": True}], "steer") is True
    # Already-parsed mapping.
    assert capability_supported({"steer": False}, "steer") is False
    assert capability_supported({"steer": True}, "steer") is True


def test_supported_missing_is_false_not_error():
    assert capability_supported([], "steer") is False
    assert capability_supported({"text": True}, "steer") is False
    assert capability_supported(["text"], "steer") is False


def test_supported_degrades_on_malformed_input():
    # A degradation gate, not a validator: malformed entries degrade to
    # False (malformed hellos are already rejected on the wire).
    assert capability_supported([""], "steer") is False
    assert capability_supported([1], "steer") is False


# ---------------------------------------------------------------------------
# Wire: hello frames with explicit capability values
# ---------------------------------------------------------------------------


def test_hello_with_dict_entry_roundtrips():
    frame = make_hello("worker-a", capabilities=["text", {"steer": False}])
    back = decode_line(json.dumps(frame).encode())
    assert back == frame
    assert parse_capabilities(back["capabilities"]) == {
        "text": True,
        "steer": False,
    }


def test_string_only_hello_still_valid():
    # Backward compatibility: pre-MX-6 hellos carry strings only.
    frame = make_hello("worker-a", capabilities=("text", "media"))
    assert decode_line(json.dumps(frame)) == frame


@pytest.mark.parametrize(
    "bad_entry",
    [
        "",
        {"steer": "yes"},
        {"a": True, "b": False},
        1,
    ],
)
def test_decode_rejects_hello_with_invalid_capability_entry(bad_entry):
    line = json.dumps(
        make_hello("worker-a", capabilities=["text", bad_entry])
    )
    with pytest.raises(SidecarProtocolError) as exc:
        decode_line(line)
    assert exc.value.code == "invalid_frame"


def test_live_handshake_steers_false_degrades(server):
    """Acceptance: steer declared false → consumer sees False, no error."""
    client = SidecarClient(server.socket_path)
    client.connect("worker-a", capabilities=("text", {"steer": False}))
    try:
        assert server.wait_connected(2.0)
        assert server.capabilities == ["text", {"steer": False}]
        assert server.capability_supported("steer") is False
        assert server.capability_supported("text") is True
        assert server.capability_supported("e2ee") is False
    finally:
        client.close()


def test_server_before_connect_reports_no_capability(tmp_path):
    srv = SidecarServer(tmp_path / "sidecar.sock")
    srv.start()
    try:
        assert srv.capability_supported("steer") is False
        assert srv.capability_supported("text") is False
    finally:
        srv.stop()


# ---------------------------------------------------------------------------
# Consumer degradation contract
# ---------------------------------------------------------------------------


class _SteerConsumer:
    """Simulated consumer: steer the in-flight turn when supported,
    otherwise degrade to the next turn — never raise."""

    def __init__(self, capabilities):
        self._caps = capabilities
        self.action = None

    def on_user_message(self, text: str):
        if capability_supported(self._caps, "steer"):
            self.action = ("steer", text)
        else:
            self.action = ("degrade_next_turn", text)


def test_consumer_degrades_without_error_when_false_or_missing():
    for caps in ([{"steer": False}], ["text"], [], {}):
        consumer = _SteerConsumer(caps)
        consumer.on_user_message("hello")  # must not raise
        assert consumer.action == ("degrade_next_turn", "hello")


def test_consumer_takes_original_path_when_true():
    for caps in (["steer"], {"steer": True}, [{"steer": True}]):
        consumer = _SteerConsumer(caps)
        consumer.on_user_message("hello")
        assert consumer.action == ("steer", "hello")


# ---------------------------------------------------------------------------
# CoreConfig.capabilities
# ---------------------------------------------------------------------------


def test_core_config_capabilities_default_empty(tmp_path):
    cfg = CoreConfig(state_dir=tmp_path, agent_id="worker-a")
    assert cfg.capabilities == {}


def test_core_config_capabilities_normalized(tmp_path):
    cfg = CoreConfig(
        state_dir=tmp_path, agent_id="worker-a", capabilities={"steer": False}
    )
    assert cfg.capabilities == {"steer": False}


def test_core_config_capabilities_rejects_bad_entries(tmp_path):
    with pytest.raises(ValueError):
        CoreConfig(state_dir=tmp_path, agent_id="a", capabilities={"steer": "no"})
    with pytest.raises(ValueError):
        CoreConfig(state_dir=tmp_path, agent_id="a", capabilities={1: True})
    with pytest.raises(ValueError):
        CoreConfig(state_dir=tmp_path, agent_id="a", capabilities={"": True})
