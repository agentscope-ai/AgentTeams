from cli_harness_worker.sync import _deep_merge, _merge_openclaw_config


def test_deep_merge_override_wins():
    base = {"a": {"b": 1, "c": 2}}
    override = {"a": {"b": 9}}
    assert _deep_merge(base, override) == {"a": {"b": 9, "c": 2}}


def test_merge_keeps_local_token():
    remote = '{"channels": {"matrix": {"accessToken": "remote-tok", "homeserver": "h"}}}'
    local = '{"channels": {"matrix": {"accessToken": "local-tok"}}}'
    merged = _merge_openclaw_config(remote, local)
    matrix = merged and __import__("json").loads(merged)["channels"]["matrix"]
    assert matrix["accessToken"] == "local-tok"
    assert matrix["homeserver"] == "h"


def test_merge_replaces_models():
    remote = '{"models": {"providers": {"p": {"apiKey": "k"}}}}'
    local = '{"models": {"providers": {"old": {}}}}'
    merged = __import__("json").loads(_merge_openclaw_config(remote, local))
    assert "p" in merged["models"]["providers"]
    assert "old" not in merged["models"]["providers"]
