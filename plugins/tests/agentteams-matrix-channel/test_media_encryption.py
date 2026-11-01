"""#1469 P1: encrypted-room attachments carry the file descriptor.

An encrypted room's media event must carry ``file`` (url + key/iv/hashes/v);
a bare ``url`` there is a BadEvent for the receiving client, and the media
repository would hold the plaintext bytes. Plaintext rooms keep the ordinary
``url`` content — the compatibility path.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

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


def _load_channel_module():
    spec = importlib.util.spec_from_file_location(
        "agentteams_matrix_channel_media_encryption_test",
        CHANNEL_MODULE_PATH,
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _channel():
    return object.__new__(_load_channel_module().AgentTeamsMatrixChannel)


def test_encrypted_media_content_uses_file_descriptor(tmp_path):
    channel = _channel()
    path = tmp_path / "report.pdf"
    payload = b"%PDF-1.4 encrypted attachment"
    path.write_bytes(payload)
    decryption = {
        "v": "v2",
        "key": {"kty": "oct", "alg": "A256CTR", "k": "ZmFrZS1rZXk"},
        "iv": "ZmFrZS1pdg",
        "hashes": {"sha256": "ZmFrZS1oYXNo"},
    }

    content = channel._matrix_media_event_content(
        str(path), "m.file", "mxc://example.org/abc", decryption
    )

    assert "url" not in content
    file_info = content["file"]
    assert file_info["url"] == "mxc://example.org/abc"
    assert file_info["v"] == "v2"
    assert file_info["key"] == decryption["key"]
    assert file_info["iv"] == decryption["iv"]
    assert file_info["hashes"] == decryption["hashes"]
    assert file_info["mimetype"] == "application/pdf"
    assert file_info["size"] == len(payload)


def test_plaintext_media_content_keeps_url(tmp_path):
    channel = _channel()
    path = tmp_path / "report.pdf"
    payload = b"%PDF-1.4 plaintext attachment"
    path.write_bytes(payload)

    content = channel._matrix_media_event_content(
        str(path), "m.file", "mxc://example.org/abc"
    )

    assert content["url"] == "mxc://example.org/abc"
    assert "file" not in content
    assert content["info"]["mimetype"] == "application/pdf"
    assert content["info"]["size"] == len(payload)
