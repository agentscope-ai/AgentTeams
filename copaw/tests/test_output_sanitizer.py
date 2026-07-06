"""Tests for hooks/output_sanitizer.py user-rule loading."""

from copaw_worker.hooks.output_sanitizer import OutputSanitizer


def test_invalid_min_length_skips_only_that_rule():
    """A non-numeric min_length must not abort loading of the other rules."""
    sanitizer = OutputSanitizer()
    rules = [
        # Invalid: non-numeric min_length -> must be skipped, not fatal.
        {"type": "prefix", "prefix": "sk-", "min_length": "not-a-number"},
        # Valid sibling rule: must still load and redact despite the bad rule.
        {"type": "regex", "pattern": r"SEKRET-[0-9]+", "replacement": "REDACTED"},
    ]

    sanitizer.load_user_rules(rules)

    assert sanitizer.sanitize("value SEKRET-12345 here") == "value REDACTED here"


def test_valid_min_length_prefix_rule_loads_and_redacts():
    sanitizer = OutputSanitizer()
    rules = [{"type": "prefix", "prefix": "sk-", "min_length": 10}]

    sanitizer.load_user_rules(rules)

    redacted = sanitizer.sanitize("token sk-abcdefghijklmnop in output")
    assert "abcdefghijklmnop" not in redacted
    assert "sk-****" in redacted
