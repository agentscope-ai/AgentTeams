#!/usr/bin/env python3
"""B8: localpart fallback for ``_was_mentioned`` (matrix channel).

Some Matrix clients render @mentions as domain-less
``matrix.to/#/@localpart`` links or as bare ``@localpart`` plain text
instead of full MXIDs. The existing checks (structured m.mentions,
full-MXID matrix.to links, full-MXID text) miss those forms, so a room
with ``require_mention`` enabled (default for group rooms) silently
drops the message. This test pins the two fallbacks:

    4. matrix.to localpart-only link  https://matrix.to/#/@alice
    5. bare @localpart in text (bounded by the full localpart charset)

Regression: full-MXID forms (checks 1-3) must keep working.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

pytest.importorskip("nio")

REPO_ROOT = Path(__file__).resolve().parents[3]
CHANNEL_MODULE_PATH = (
    REPO_ROOT
    / "plugins"
    / "agentteams-matrix-channel"
    / "agentteams_matrix"
    / "channel.py"
)

_spec = importlib.util.spec_from_file_location(
    "agentteams_matrix_channel", CHANNEL_MODULE_PATH
)
assert _spec is not None and _spec.loader is not None
channel_module = importlib.util.module_from_spec(_spec)
sys.modules["agentteams_matrix_channel"] = channel_module
_spec.loader.exec_module(channel_module)

AgentTeamsMatrixChannel = channel_module.AgentTeamsMatrixChannel

ME = "@alice:matrix.local"


def _make_channel(user_id: str = ME) -> "AgentTeamsMatrixChannel":
    channel = AgentTeamsMatrixChannel(
        process=lambda *args, **kw: None,
        share_session_in_group=True,
    )
    channel._user_id = user_id
    return channel


def _event(*, formatted_body: str = ""):
    """Minimal event whose .source carries the content dict read by _was_mentioned."""
    return SimpleNamespace(source={"content": {"formatted_body": formatted_body}})


def _mentioned(channel, text: str, *, formatted_body: str = "") -> bool:
    return channel._was_mentioned(_event(formatted_body=formatted_body), text)


# --- regression: existing full-MXID forms still work ---

def test_structured_mxid_mention():
    channel = _make_channel()
    event = SimpleNamespace(source={"content": {
        "m.mentions": {"user_ids": [ME]},
    }})
    assert channel._was_mentioned(event, "hello") is True


def test_matrix_to_full_mxid_link():
    channel = _make_channel()
    fb = '<a href="https://matrix.to/#/@alice:matrix.local">Alice</a> hi'
    assert _mentioned(channel, "hi", formatted_body=fb) is True


# --- new: localpart fallbacks ---

def test_matrix_to_localpart_link():
    channel = _make_channel()
    fb = '<a href="https://matrix.to/#/@alice">alice</a> ping'
    assert _mentioned(channel, "ping", formatted_body=fb) is True


def test_bare_localpart_in_text():
    channel = _make_channel()
    assert _mentioned(channel, "@alice please review") is True


# --- negative cases ---

def test_other_localpart_does_not_match():
    channel = _make_channel()
    assert _mentioned(channel, "@bob please review") is False
    fb = '<a href="https://matrix.to/#/@bob">bob</a> hi'
    assert _mentioned(channel, "hi", formatted_body=fb) is False


def test_substring_localpart_does_not_match():
    channel = _make_channel()
    # @alicex starts with our localpart but is a different user.
    assert _mentioned(channel, "@alicex hello") is False
    # Word char before the @: not a mention boundary.
    assert _mentioned(channel, "x@alice hello") is False


def test_no_mention_returns_false():
    channel = _make_channel()
    assert _mentioned(channel, "just a plain message") is False


# --- A3: whole-token matching for bare mentions ---

def test_hyphen_suffix_localpart_does_not_match():
    channel = _make_channel()
    # @alice-dev is a different user (longer localpart), not a mention of alice.
    assert _mentioned(channel, "@alice-dev hello") is False
    fb = '<a href="https://matrix.to/#/@alice-dev">Alice Dev</a> hi'
    assert _mentioned(channel, "hi", formatted_body=fb) is False


def test_dot_and_digit_suffix_localpart_do_not_match():
    channel = _make_channel()
    assert _mentioned(channel, "@alice.2 hello") is False
    assert _mentioned(channel, "@alice2 hello") is False


def test_other_domain_mxid_does_not_match():
    channel = _make_channel()
    # A full MXID on another domain is not a bare localpart mention of ours.
    assert _mentioned(channel, "@alice:other.test hello") is False


def test_valid_bare_mention_still_matches():
    channel = _make_channel()
    assert _mentioned(channel, "@alice please review") is True
    assert _mentioned(channel, "thanks @alice!") is True


def test_plus_and_slash_suffix_localpart_do_not_match():
    channel = _make_channel()
    # `+` and `/` are valid Matrix localpart characters: longer localparts
    # like `@alice+dev` / `@alice/dev` are OTHER users, not `@alice`, and
    # full MXIDs on another domain must not be truncated to a match either.
    assert _mentioned(channel, "@alice+dev:other.test hello") is False
    assert _mentioned(channel, "@alice/dev:other.test hello") is False
    assert _mentioned(channel, "@alice+dev hello") is False
    assert _mentioned(channel, "@alice/dev hello") is False


def test_own_localpart_with_plus_or_slash_still_matches():
    # Our own localpart may also contain `+` / `/`; a bare mention of it
    # must still match (the token boundary compares like-for-like).
    plus_channel = _make_channel("@alice+dev:matrix.local")
    assert _mentioned(plus_channel, "@alice+dev hello") is True
    slash_channel = _make_channel("@alice/dev:matrix.local")
    assert _mentioned(slash_channel, "@alice/dev hello") is True
