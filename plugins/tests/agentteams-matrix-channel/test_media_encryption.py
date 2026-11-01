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


# --- #1469 P1: the long-text fallback uses the same room-aware encrypted
# attachment path (the receiver must get a decryptable descriptor, and the
# media repository must hold ciphertext, not the full plaintext text). ---

import asyncio
import json
import subprocess
import sys

from types import SimpleNamespace

LONG_TEXT = "matrix-long-text-" + ("x" * 200_000)


class _LongTextClient:
    """A fake nio client whose upload really encrypts, so the test can check
    that the repository (what we would store) receives ciphertext."""

    def __init__(self):
        self.uploads = []
        self.sent = []
        self.olm = object()  # the channel treats a present olm as E2EE-ready

    async def upload(self, data, content_type=None, filename=None, filesize=None, encrypt=False, **kw):
        from nio.crypto import encrypt_attachment

        payload = data.read() if hasattr(data, "read") else bytes(data or b"")
        from nio.responses import UploadResponse

        if encrypt:
            ciphertext, info = encrypt_attachment(payload)
            self.uploads.append({"encrypt": True, "data": ciphertext, "info": info,
                                 "filename": filename})
            return UploadResponse(content_uri="mxc://example.org/long"), info
        self.uploads.append({"encrypt": False, "data": payload, "info": None,
                             "filename": filename})
        return UploadResponse(content_uri="mxc://example.org/long"), None

    async def room_send(self, room_id, message_type, content, **kw):
        self.sent.append({"room_id": room_id, "type": message_type, "content": content})
        return SimpleNamespace(event_id="$long-event")


def _long_text_channel(module):
    channel = object.__new__(module.AgentTeamsMatrixChannel)
    channel._client = _LongTextClient()
    channel._user_id = "@bot:test"
    channel.encryption = True
    channel._active_thread_roots = {}

    async def _prepare_room_send(_room_id):
        return None

    channel._prepare_room_send = _prepare_room_send
    channel._room_will_encrypt = lambda _room_id: True
    return channel


def _assert_encrypted_long_text(client):
    assert client.uploads and client.uploads[0]["encrypt"] is True
    upload = client.uploads[0]
    assert upload["data"] != LONG_TEXT.encode()  # repository holds ciphertext
    file_events = [s for s in client.sent if s["content"].get("file")]
    assert file_events, "the long-text fallback must emit the file descriptor"
    file_info = file_events[0]["content"]["file"]
    for key in ("url", "key", "iv", "hashes", "v"):
        assert key in file_info, f"missing {key} in the file descriptor"
    from nio.crypto import decrypt_attachment

    plaintext = decrypt_attachment(
        upload["data"], file_info["key"]["k"], file_info["hashes"]["sha256"], file_info["iv"]
    )
    assert plaintext == LONG_TEXT.encode()


def test_long_text_fallback_encrypts_in_encrypted_room(tmp_path):
    module = _load_channel_module()
    channel = _long_text_channel(module)
    channel._media_dir = lambda: tmp_path

    event_id = asyncio.run(
        channel._send_long_text_fallback("!room:test", LONG_TEXT, {}, "m.text")
    )

    assert event_id == "$long-event"
    _assert_encrypted_long_text(channel._client)


def test_long_text_fallback_plaintext_room_uses_url(tmp_path):
    module = _load_channel_module()
    channel = _long_text_channel(module)
    channel._media_dir = lambda: tmp_path
    channel._room_will_encrypt = lambda _room_id: False

    asyncio.run(channel._send_long_text_fallback("!room:test", LONG_TEXT, {}, "m.text"))

    client = channel._client
    assert client.uploads[0]["encrypt"] is False
    file_events = [s for s in client.sent if s["content"].get("url")]
    assert file_events and "file" not in file_events[0]["content"]


_BUNDLE_SCRIPT = r"""
import asyncio, json, sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from agentteams_matrix.channel import AgentTeamsMatrixChannel as C
from types import SimpleNamespace

LONG = "bundle-long-text-" + ("y" * 200_000)

class Client:
    def __init__(self):
        self.uploads = []; self.sent = []; self.olm = object()
    async def upload(self, data, content_type=None, filename=None, filesize=None, encrypt=False, **kw):
        from nio.crypto import encrypt_attachment
        payload = data.read() if hasattr(data, "read") else bytes(data or b"")
        info = None; stored = payload
        if encrypt:
            stored, info = encrypt_attachment(payload)
        self.uploads.append({"encrypt": encrypt, "data": stored, "info": info})
        from nio.responses import UploadResponse
        return UploadResponse(content_uri="mxc://example.org/long"), info
    async def room_send(self, room_id, message_type, content, **kw):
        self.sent.append(content)
        return SimpleNamespace(event_id="$long")

ch = object.__new__(C)
ch._client = Client(); ch._user_id = "@bot:test"; ch.encryption = True; ch._active_thread_roots = {}
async def _prep(_r): return None
ch._prepare_room_send = _prep
ch._room_will_encrypt = lambda _r: True
ch._media_dir = lambda: Path(sys.argv[2])
asyncio.run(ch._send_long_text_fallback("!room:test", LONG, {}, "m.text"))
client = ch._client
assert client.uploads[0]["encrypt"] is True
assert client.uploads[0]["data"] != LONG.encode()
files = [c for c in client.sent if isinstance(c, dict) and c.get("file")]
assert files, "no file descriptor in the bundle entry"
info = files[0]["file"]
from nio.crypto import decrypt_attachment
plain = decrypt_attachment(client.uploads[0]["data"], info["key"]["k"], info["hashes"]["sha256"], info["iv"])
assert plain == LONG.encode()
print("BUNDLE_OK")
"""


def test_bundle_entry_long_text_encrypts(tmp_path):
    """Both installed entries must behave the same: run the bundle's own
    channel module in a fresh process so its package imports cannot collide
    with the live tree."""
    script = tmp_path / "bundle_long_text_probe.py"
    script.write_text(_BUNDLE_SCRIPT, encoding="utf-8")
    bundle_root = REPO_ROOT / "plugins" / "agentteams-matrix" / "adapters" / "qwenpaw"
    result = subprocess.run(
        [sys.executable, str(script), str(bundle_root), str(tmp_path)],
        capture_output=True,
        text=True,
        timeout=120,
    )
    assert result.returncode == 0, result.stderr
    assert "BUNDLE_OK" in result.stdout
