"""Unit tests for the runtime-neutral mention helpers (P9, PR1 M2).

Table-driven coverage of the edge cases the channel implementation is
pinned to: empty/absent ``m.mentions``, structured room mentions,
case-insensitivity, full-MXID and bare-localpart text forms with the
localpart token boundary rules, mention-prefix stripping (punctuation
and multi-space separators, display names with decoration), and the
worker-localpart relevance check.

The parity section asserts that the channel's original methods return
identical results on the same inputs (the channel is constructed cheaply;
see ``plugins/tests/agentteams-matrix-channel/test_mention_localpart.py``
for the same construction pattern).
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

from core import (
    extract_mentions_from_text,
    strip_mention_prefix,
    text_mentions_worker,
    was_mentioned,
)

ME = "@alice:matrix.local"

REPO_ROOT = Path(__file__).resolve().parents[3]
CHANNEL_MODULE_PATH = (
    REPO_ROOT
    / "plugins"
    / "agentteams-matrix"
    / "adapters"
    / "qwenpaw"
    / "agentteams_matrix"
    / "channel.py"
)


def _event(
    *,
    user_ids: list | None = None,
    room_mention: bool = False,
    formatted_body: str = "",
):
    """Minimal event whose .source carries the content dict read by was_mentioned."""
    content: dict = {}
    if user_ids is not None or room_mention:
        mentions: dict = {"user_ids": user_ids or []}
        if room_mention:
            mentions["room"] = True
        content["m.mentions"] = mentions
    if formatted_body:
        content["formatted_body"] = formatted_body
    return SimpleNamespace(source={"content": content})


# ---------------------------------------------------------------------------
# was_mentioned — table driven
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("event", "text", "expected"),
    [
        # 1. structured m.mentions.user_ids hit
        (_event(user_ids=[ME]), "hello", True),
        # 1b. structured hit inside a duplicate list
        (_event(user_ids=[ME, ME]), "hello", True),
        # 2. structured room mention
        (_event(room_mention=True), "hello", True),
        # 2b. empty m.mentions + no text mention
        (_event(user_ids=[]), "plain", False),
        # 2c. m.mentions absent entirely
        (_event(), "plain", False),
        # 3. full MXID in plain text (case-insensitive)
        (_event(), "hi @alice:matrix.local", True),
        (_event(), "HI @ALICE:matrix.local", True),
        # 3b. full MXID on ANOTHER domain is not ours
        (_event(), "@alice:other.test", False),
        # 5. bare @localpart forms
        (_event(), "@alice please review", True),
        (_event(), "thanks @alice!", True),
        # 5b. longer localparts are OTHER users
        (_event(), "@alicex hello", False),
        (_event(), "@alice-dev hello", False),
        (_event(), "@alice2 hello", False),
        (_event(), "@alice.2 hello", False),
        (_event(), "@alice+dev hello", False),
        (_event(), "@alice/dev hello", False),
        # 5c. not a mention boundary
        (_event(), "x@alice hello", False),
        (_event(), "@@alice hello", False),
        # 4. matrix.to links in formatted_body
        (
            _event(formatted_body='<a href="https://matrix.to/#/@alice:matrix.local">Alice</a> hi'),
            "hi",
            True,
        ),
        (
            _event(formatted_body='<a href="https://matrix.to/#/%40alice%3Amatrix.local">Alice</a> hi'),
            "hi",
            True,
        ),
        (
            _event(formatted_body='<a href="https://matrix.to/#/@alice">alice</a> ping'),
            "ping",
            True,
        ),
        (
            _event(formatted_body='<a href="https://matrix.to/#/@bob">bob</a> hi'),
            "hi",
            False,
        ),
        # no mention anywhere
        (_event(), "just a plain message", False),
    ],
    ids=[
        "structured-user-id",
        "structured-duplicate",
        "structured-room",
        "structured-empty",
        "structured-absent",
        "text-full-mxid",
        "text-full-mxid-case",
        "text-other-domain",
        "text-bare-localpart",
        "text-bare-localpart-punct",
        "text-longer-localpart",
        "text-hyphen-suffix",
        "text-digit-suffix",
        "text-dot-suffix",
        "text-plus-suffix",
        "text-slash-suffix",
        "text-word-before-at",
        "text-double-at",
        "html-full-mxid-link",
        "html-encoded-mxid-link",
        "html-localpart-link",
        "html-other-user-link",
        "no-mention",
    ],
)
def test_was_mentioned_table(event, text, expected):
    assert was_mentioned(ME, event, text) is expected


def test_was_mentioned_localpart_with_plus_and_slash():
    # Our own localpart may contain `+` / `/`; a bare mention of it must
    # still match (the token boundary compares like-for-like).
    assert was_mentioned("@alice+dev:matrix.local", _event(), "@alice+dev hello") is True
    assert was_mentioned("@alice/dev:matrix.local", _event(), "@alice/dev hello") is True


def test_was_mentioned_no_user_id():
    assert was_mentioned("", _event(user_ids=["@x:hs"]), "@x:hs") is False


# ---------------------------------------------------------------------------
# extract_mentions_from_text — table driven
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("hi @alice:matrix.local and @bob:hs.example", ["@alice:matrix.local", "@bob:hs.example"]),
        ("@alice:matrix.local:8448 please", ["@alice:matrix.local:8448"]),
        ("dup @alice:matrix.local again @alice:matrix.local", ["@alice:matrix.local"]),
        ("cc @a.b:hs done", ["@a.b:hs"]),
        ("order @b:hs first @a:hs second", ["@b:hs", "@a:hs"]),
        ("bare @alice without domain", []),
        ("no mxid here", []),
        ("", []),
        ("@Alice:Matrix.Local case", ["@Alice:Matrix.Local"]),
    ],
    ids=[
        "two-mxids",
        "port",
        "dedupe",
        "dotted-localpart",
        "order-preserved",
        "no-domain",
        "none",
        "empty",
        "uppercase",
    ],
)
def test_extract_mentions_from_text_table(text, expected):
    assert extract_mentions_from_text(text) == expected


# ---------------------------------------------------------------------------
# strip_mention_prefix — table driven
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("@alice:matrix.local /new", "/new"),
        ("@alice:matrix.local: /new", "/new"),
        ("@ALICE:matrix.local /new", "/new"),
        ("@alice /clear", "/clear"),
        ("alice /clear", "/clear"),
        ("alice: /clear", "/clear"),
        ("alice  /clear", "/clear"),
        ("@alice:/new", "/new"),
        ("just a message", "just a message"),
        # Prefix stripping is anchored at the start with no word-boundary
        # guard (channel quirk, kept verbatim): "alicex" loses "alice".
        ("alicex /clear", "x /clear"),
        ("", ""),
    ],
    ids=[
        "mxid-prefix",
        "mxid-colon-space",
        "mxid-case",
        "localpart-at",
        "localpart-bare",
        "localpart-colon",
        "localpart-multi-space",
        "localpart-colon-nospace",
        "no-prefix",
        "no-word-boundary",
        "empty",
    ],
)
def test_strip_mention_prefix_table(text, expected):
    assert strip_mention_prefix(ME, text) == expected


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("math 💕: /clear", "/clear"),
        ("math 💕 /clear", "/clear"),
        ("math 💕  :  /clear", "/clear"),
        ("math /clear", "/clear"),
        ("math💕: /clear", "/clear"),
        ("other message", "other message"),
    ],
    ids=[
        "display-name-colon",
        "display-name-space",
        "display-name-punct-spaces",
        "display-name-localpart-fallback",
        "display-name-no-space-emoji",
        "no-prefix",
    ],
)
def test_strip_mention_prefix_display_name(text, expected):
    # Localpart "math", display name "math 💕": the display name is tried
    # before the shorter localpart.
    assert (
        strip_mention_prefix("@math:hs.example", text, display_name="math 💕")
        == expected
    )


def test_strip_mention_prefix_empty_user_id():
    assert strip_mention_prefix("", "@alice /new", display_name=None) == "@alice /new"


# ---------------------------------------------------------------------------
# text_mentions_worker — table driven
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("user_id", "text", "worker_localpart", "expected"),
    [
        (ME, "", "alice", False),
        (ME, "hi", "", False),
        (None, "hi", "", False),
        (ME, "hi @alice", "alice", True),
        (None, "hi @alice", "alice", True),
        (ME, "@alicex hello", "alice", False),
        (ME, "@alice-dev hello", "alice", False),
        (ME, "hi @ALICE", "alice", False),
        (ME, "hi @alice:matrix.local", "alice", True),
        (ME, "HI @ALICE:matrix.local", "alice", True),
        (ME, "x@alice hello", "alice", False),
        (ME, "unrelated", "alice", False),
    ],
    ids=[
        "empty-text",
        "empty-worker",
        "empty-both",
        "bare-worker",
        "bare-worker-no-user",
        "longer-localpart",
        "hyphen-suffix",
        "case-sensitive-bare",
        "full-mxid",
        "full-mxid-case",
        "word-before-at",
        "unrelated",
    ],
)
def test_text_mentions_worker_table(user_id, text, worker_localpart, expected):
    assert text_mentions_worker(user_id, text, worker_localpart) is expected


# ---------------------------------------------------------------------------
# Parity with the QwenPaw channel (original implementation)
# ---------------------------------------------------------------------------


def _load_channel_module(name: str):
    spec = importlib.util.spec_from_file_location(name, CHANNEL_MODULE_PATH)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def channel():
    pytest.importorskip("nio")
    pytest.importorskip("qwenpaw")
    mod = _load_channel_module("p9_parity_channel_mentions")

    ch = mod.AgentTeamsMatrixChannel(process=lambda *a, **k: None)
    ch._user_id = ME
    return ch


def _fake_room(name_for: dict):
    """Room stand-in exposing the surface _get_display_name uses."""

    class _FakeRoom:
        users = {}

        def user_name(self, uid):
            return name_for.get(uid)

    return _FakeRoom()


def test_parity_was_mentioned(channel):
    cases = [
        ("hello", ""),
        ("hi @alice:matrix.local", ""),
        ("HI @ALICE:matrix.local", ""),
        ("@alice please review", ""),
        ("thanks @alice!", ""),
        ("@alicex hello", ""),
        ("@alice-dev hello", ""),
        ("x@alice hello", ""),
        ("@alice:other.test", ""),
        ("just a plain message", ""),
        ("hi", '<a href="https://matrix.to/#/@alice:matrix.local">Alice</a> hi'),
        ("hi", '<a href="https://matrix.to/#/%40alice%3Amatrix.local">Alice</a> hi'),
        ("ping", '<a href="https://matrix.to/#/@alice">alice</a> ping'),
        ("hi", '<a href="https://matrix.to/#/@bob">bob</a> hi'),
    ]
    for text, formatted_body in cases:
        event = _event(formatted_body=formatted_body)
        assert channel._was_mentioned(event, text) == was_mentioned(ME, event, text), (
            text,
            formatted_body,
        )
    # Structured arms (user_ids / room) — same event objects on both sides.
    for event in (_event(user_ids=[ME]), _event(room_mention=True), _event(user_ids=[])):
        assert channel._was_mentioned(event, "hello") == was_mentioned(ME, event, "hello")


def test_parity_extract_mentions_from_text(channel):
    for text in (
        "hi @alice:matrix.local and @bob:hs.example",
        "@alice:matrix.local:8448 please",
        "dup @alice:matrix.local again @alice:matrix.local",
        "no mxid here",
        "",
    ):
        assert channel._extract_mentions_from_text(text) == extract_mentions_from_text(text)


def test_parity_text_mentions_worker(channel):
    for text in (
        "",
        "hi",
        "hi @alice",
        "@alicex hello",
        "@alice-dev hello",
        "hi @ALICE",
        "hi @alice:matrix.local",
        "HI @ALICE:matrix.local",
        "x@alice hello",
    ):
        assert channel._text_mentions_worker(text, "alice") == text_mentions_worker(
            ME, text, "alice"
        )
    assert channel._text_mentions_worker("hi", "") is False


def test_parity_strip_mention_prefix_no_room(channel):
    for text in (
        "@alice:matrix.local /new",
        "@alice:matrix.local: /new",
        "@ALICE:matrix.local /new",
        "@alice /clear",
        "alice /clear",
        "alice: /clear",
        "alice  /clear",
        "just a message",
        "alicex /clear",
        "",
    ):
        assert channel._strip_mention_prefix(text) == strip_mention_prefix(ME, text)


def test_parity_strip_mention_prefix_with_room(channel):
    room = _fake_room({ME: None, "@math:hs.example": "math 💕"})
    for user_id, math_cases in (
        (ME, ("@alice:matrix.local /new", "alice /clear", "just a message")),
        ("@math:hs.example", ("math 💕: /clear", "math /clear", "other message")),
    ):
        ch = type(channel)(process=lambda *a, **k: None)
        ch._user_id = user_id
        for text in math_cases:
            display = ch._get_display_name(room, user_id)
            assert ch._strip_mention_prefix(text, room) == strip_mention_prefix(
                user_id, text, display_name=display
            )
