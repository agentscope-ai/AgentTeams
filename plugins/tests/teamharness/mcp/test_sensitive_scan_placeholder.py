"""P12/B7: the artifact sensitive scan must whitelist placeholder values.

A value wrapped in a single pair of angle brackets is a documentation
template ONLY when it carries an explicit placeholder marker
(``<YOUR_API_KEY>``, ``<paste-token-here>``, ``<token:your_key_here>``).
Brackets around credential-shaped content stay flagged: neither the
kebab shape alone (``<sk-abcdefghijklmnopqrstuvwxyz>``) nor arbitrary
label:value content (``<token:abc123def4567890>``) is exempt. Real
credential values must still be flagged.
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest


MCP_DIR = Path(__file__).resolve().parents[3] / "teamharness" / "mcp"
if str(MCP_DIR) not in sys.path:
    sys.path.insert(0, str(MCP_DIR))

import server  # noqa: E402


def _scan(tmp_path: Path, content: str) -> bool:
    path = tmp_path / "artifact.txt"
    path.write_text(content, encoding="utf-8")
    return server._artifact_text_has_sensitive_content(path, "text/plain")


@pytest.mark.parametrize(
    "content",
    [
        "Authorization: Bearer <YOUR_API_KEY>",
        "Authorization: Basic <your-basic-token>",
        "curl -H 'Authorization: Bearer <your-jev-api-key>' https://api.example.test/v1",
        "api_key = <PLACEHOLDER_123456789012>",
        "export TOKEN=<PASTE_TOKEN_HERE>",
        'body = {"token": "<YOUR_TOKEN>"}',
        "No secrets here at all.",
    ],
)
def test_placeholder_values_are_not_flagged(tmp_path: Path, content: str) -> None:
    assert _scan(tmp_path, content) is False, content


@pytest.mark.parametrize(
    "content",
    [
        "Authorization: Bearer sk-a1b2c3d4e5f607182930415263748596",
        "authorization: basic dXNlcjpwYXNzd29yZA==",
        "api_key = a1b2c3d4e5f607182930415263748596",
        'token: "a1b2c3d4e5f607182930415263748596"',
        "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
    ],
)
def test_real_credential_values_still_flagged(tmp_path: Path, content: str) -> None:
    assert _scan(tmp_path, content) is True, content


@pytest.mark.parametrize(
    "content",
    [
        # Brackets do not make credential-shaped content a placeholder.
        "Authorization: Bearer <sk-a1b2c3d4e5f607182930415263748596>",
        "Authorization: Basic <dXNlcjpwYXNzd29yZA==>",
        "Authorization: Bearer <sk-abc123def456>",
        # Shape alone is never enough: no explicit marker -> still flagged.
        "Authorization: Bearer <sk-abcdefghijklmnopqrstuvwxyz>",
        "Authorization: Bearer <token:abc123def4567890>",
        "Authorization: Basic <basic-token>",
    ],
)
def test_bracketed_credential_shapes_still_flagged(
    tmp_path: Path,
    content: str,
) -> None:
    assert _scan(tmp_path, content) is True, content


def test_mixed_documentation_and_real_key_flags(
    tmp_path: Path,
) -> None:
    content = (
        "Usage: Authorization: Bearer <YOUR_API_KEY>\n"
        "Real one leaked: api_key = a1b2c3d4e5f607182930415263748596\n"
    )
    assert _scan(tmp_path, content) is True
