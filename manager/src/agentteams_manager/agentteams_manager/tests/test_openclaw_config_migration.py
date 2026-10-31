"""Migration of persisted openclaw.json configs.

The upstream v2026.10.9 matrix room schema rejects the fork-only
``groups.<room>.allow`` switch. Existing workspaces still carry it, and both
the Manager (existing config) and Worker (pulled config) boot paths run this
migration, so the migration itself has to remove it while keeping the deny
semantics.
"""

import json

from agentteams_manager.sync import _migrate_openclaw_config


def _config(groups):
    return {"channels": {"matrix": {"homeserver": "http://matrix", "groups": groups}}}


def test_allow_true_is_dropped_and_mention_kept():
    out = _migrate_openclaw_config(
        _config({"*": {"allow": True, "requireMention": True}})
    )
    groups = out["channels"]["matrix"]["groups"]
    assert groups["*"] == {"requireMention": True}
    assert "allow" not in groups["*"]


def test_allow_false_becomes_enabled_false():
    out = _migrate_openclaw_config(
        _config({"!room:example.org": {"allow": False, "requireMention": True}})
    )
    entry = out["channels"]["matrix"]["groups"]["!room:example.org"]
    assert entry["enabled"] is False
    assert entry["requireMention"] is True
    assert "allow" not in entry


def test_explicit_enabled_wins_over_allow_false():
    out = _migrate_openclaw_config(
        _config({"!room:example.org": {"enabled": True, "allow": False}})
    )
    entry = out["channels"]["matrix"]["groups"]["!room:example.org"]
    assert entry["enabled"] is True
    assert "allow" not in entry


def test_entries_without_allow_are_untouched():
    groups = {"!room:example.org": {"requireMention": False}}
    out = _migrate_openclaw_config(_config(groups))
    assert out["channels"]["matrix"]["groups"] == groups


def test_non_object_groups_and_entries_are_left_alone():
    out = _migrate_openclaw_config(_config({"!room:example.org": "disabled"}))
    assert out["channels"]["matrix"]["groups"] == {"!room:example.org": "disabled"}

    raw = {"channels": {"matrix": {"groups": ["not", "a", "map"]}}}
    assert _migrate_openclaw_config(raw)["channels"]["matrix"]["groups"] == [
        "not",
        "a",
        "map",
    ]


def test_migration_is_idempotent():
    once = _migrate_openclaw_config(
        _config(
            {
                "*": {"allow": True, "requireMention": True},
                "!room:example.org": {"allow": False},
            }
        )
    )
    twice = _migrate_openclaw_config(json.loads(json.dumps(once)))
    assert once == twice
