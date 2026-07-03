from __future__ import annotations

from hermes_matrix.policies import (
    CURRENT_MESSAGE_MARKER,
    HISTORY_CONTEXT_MARKER,
    DualAllowList,
    HistoryBuffer,
    apply_outbound_mentions,
    extract_mentions_from_text,
    should_suppress_outbound,
)


def test_extract_mentions_deduplicates_and_skips_self() -> None:
    text = (
        "ping @manager:matrix.local "
        "@Manager:matrix.local "
        "@alice:matrix.local"
    )
    assert extract_mentions_from_text(
        text,
        self_user_id="@alice:matrix.local",
    ) == ["@manager:matrix.local"]


def test_apply_outbound_mentions_merges_existing_block_and_edit_body() -> None:
    content = {
        "body": "hello @manager:matrix.local",
        "m.mentions": {"room": True, "user_ids": ["@existing:matrix.local"]},
        "m.new_content": {
            "body": "updated @reviewer:matrix.local",
        },
    }

    apply_outbound_mentions(content, self_user_id="@alice:matrix.local")

    assert content["m.mentions"]["room"] is True
    assert content["m.mentions"]["user_ids"] == [
        "@existing:matrix.local",
        "@manager:matrix.local",
        "@reviewer:matrix.local",
    ]
    assert content["m.new_content"]["m.mentions"]["user_ids"] == [
        "@manager:matrix.local",
        "@reviewer:matrix.local",
    ]


def test_dual_allowlist_respects_dm_and_group_modes() -> None:
    policy = DualAllowList(
        dm_policy="allowlist",
        group_policy="allowlist",
        dm_allow=frozenset({"@dm-allowed:matrix.local"}),
        group_allow=frozenset({"@group-allowed:matrix.local"}),
    )

    assert policy.permits("@dm-allowed:matrix.local", is_dm=True) is True
    assert policy.permits("@group-allowed:matrix.local", is_dm=True) is False
    assert policy.permits("@dm-allowed:matrix.local", is_dm=False) is True
    assert policy.permits("@group-allowed:matrix.local", is_dm=False) is True
    assert policy.permits("@blocked:matrix.local", is_dm=False) is False


def test_dual_allowlist_disabled_and_open_modes() -> None:
    disabled = DualAllowList(dm_policy="disabled", group_policy="disabled")
    assert disabled.permits("@any:matrix.local", is_dm=True) is False
    assert disabled.permits("@any:matrix.local", is_dm=False) is False

    open_policy = DualAllowList(dm_policy="open", group_policy="open")
    assert open_policy.permits("@any:matrix.local", is_dm=True) is True
    assert open_policy.permits("@any:matrix.local", is_dm=False) is True


def test_history_buffer_drain_formats_prefix_and_clears() -> None:
    history = HistoryBuffer(limit=3)
    history.record("!room:matrix.local", "alice", "first")
    history.record("!room:matrix.local", "bob", "second")

    drained = history.drain("!room:matrix.local")

    assert drained == (
        f"{HISTORY_CONTEXT_MARKER}\n"
        "alice: first\n"
        "bob: second\n\n"
        f"{CURRENT_MESSAGE_MARKER}\n"
    )
    assert history.drain("!room:matrix.local") == ""


def test_history_buffer_respects_limit() -> None:
    history = HistoryBuffer(limit=2)
    history.record("!room:matrix.local", "alice", "one")
    history.record("!room:matrix.local", "bob", "two")
    history.record("!room:matrix.local", "carol", "three")

    drained = history.drain("!room:matrix.local")

    assert "alice: one" not in drained
    assert "bob: two" in drained
    assert "carol: three" in drained


def test_should_suppress_outbound_noop_when_both_filters_off() -> None:
    """Mechanism default-off: with no filters enabled, nothing is suppressed,
    including content explicitly tagged as tool/thinking chatter.
    """
    tool_content = {"body": "ran a tool", "hermes.event_kind": "tool"}
    thinking_content = {"body": "thinking...", "hermes.event_kind": "thinking"}

    assert should_suppress_outbound(tool_content) is False
    assert should_suppress_outbound(thinking_content) is False


def test_should_suppress_outbound_drops_tool_and_thinking_when_enabled() -> None:
    tool_content = {"body": "ran a tool", "hermes.event_kind": "tool"}
    thinking_content = {"body": "thinking...", "hermes.event_kind": "thinking"}

    assert should_suppress_outbound(tool_content, filter_tool=True) is True
    assert should_suppress_outbound(thinking_content, filter_thinking=True) is True

    # Each filter only gates its own category.
    assert should_suppress_outbound(tool_content, filter_thinking=True) is False
    assert should_suppress_outbound(thinking_content, filter_tool=True) is False


def test_should_suppress_outbound_always_passes_plain_final_text() -> None:
    plain = {"body": "final answer", "msgtype": "m.text"}
    untagged = {"body": "final answer"}

    assert should_suppress_outbound(
        plain, filter_tool=True, filter_thinking=True
    ) is False
    assert should_suppress_outbound(
        untagged, filter_tool=True, filter_thinking=True
    ) is False


def test_should_suppress_outbound_always_passes_lifecycle_events() -> None:
    for kind in ("start", "finish", "heartbeat"):
        content = {"body": f"[{kind}]", "hermes.event_kind": kind}
        assert should_suppress_outbound(
            content, filter_tool=True, filter_thinking=True
        ) is False


def test_should_suppress_outbound_always_passes_replace_edits() -> None:
    """m.replace edits (streamed final-message updates) must never be
    dropped, even if the event would otherwise look like tool/thinking
    chatter — dropping them could leave the room showing stale text.
    """
    edit_content = {
        "body": "* edited final text",
        "hermes.event_kind": "tool",
        "m.relates_to": {"rel_type": "m.replace", "event_id": "$abc"},
    }

    assert should_suppress_outbound(
        edit_content, filter_tool=True, filter_thinking=True
    ) is False
