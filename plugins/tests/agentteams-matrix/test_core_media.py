"""MediaCache and mxc_to_http tests (P9, PR1 M4c).

Pins the moved media behaviour: the pure mxc→http conversion (valid
and the passthrough cases), the download-to-file success/failure paths
with an injected fetch callback (the token/HTTP client stay out of the
core), and the encrypted-attachment decryption path — a real
matrix-nio encrypt/decrypt round trip when the crypto dependency is
importable, a monkeypatched stand-in for the lazy import point
otherwise.
"""

from __future__ import annotations

import asyncio
import subprocess
import sys
import types
from pathlib import Path
from typing import List, Optional

import pytest

from core.media import MediaCache, mxc_to_http, safe_cache_name

try:
    from nio.crypto.attachments import (
        decrypt_attachment as _real_decrypt,
        encrypt_attachment as _real_encrypt,
    )

    HAVE_NIO_CRYPTO = True
except ImportError:
    _real_decrypt = None
    _real_encrypt = None
    HAVE_NIO_CRYPTO = False

MHC = "mxc://hs.example.com/abc123"
HOMESERVER = "https://matrix.example.com"
BUNDLE_DIR = Path(__file__).resolve().parents[2] / "agentteams-matrix"


def _run(coro):
    """Run an async scenario to completion (no pytest-asyncio)."""
    return asyncio.run(coro)


class FakeFetch:
    """Recorded stand-in for the adapter's authenticated GET callback."""

    def __init__(
        self,
        content: bytes,
        raise_exc: Optional[Exception] = None,
    ) -> None:
        self.content = content
        self.raise_exc = raise_exc
        self.calls: List[str] = []

    async def __call__(self, mxc_url: str) -> bytes:
        self.calls.append(mxc_url)
        if self.raise_exc is not None:
            raise self.raise_exc
        return self.content


def _fake_decrypt_module(decrypt_fn) -> tuple:
    """Build (nio, nio.crypto, nio.crypto.attachments) stand-in modules."""
    fake_attachments = types.ModuleType("nio.crypto.attachments")
    fake_attachments.decrypt_attachment = decrypt_fn
    fake_crypto = types.ModuleType("nio.crypto")
    fake_crypto.attachments = fake_attachments
    fake_nio = types.ModuleType("nio")
    fake_nio.crypto = fake_crypto
    return fake_nio, fake_crypto, fake_attachments


def _install_fake_decrypt(monkeypatch, decrypt_fn) -> None:
    fake_nio, fake_crypto, fake_attachments = _fake_decrypt_module(decrypt_fn)
    monkeypatch.setitem(sys.modules, "nio", fake_nio)
    monkeypatch.setitem(sys.modules, "nio.crypto", fake_crypto)
    monkeypatch.setitem(sys.modules, "nio.crypto.attachments", fake_attachments)


# ----------------------------------------------------------------------
# mxc_to_http
# ----------------------------------------------------------------------


def test_mxc_to_http_valid():
    assert (
        mxc_to_http(MHC, HOMESERVER)
        == f"{HOMESERVER}/_matrix/media/v3/download/hs.example.com/abc123"
    )


def test_mxc_to_http_passthrough_invalid_and_non_mxc():
    for url in (
        "",
        "https://matrix.example.com/already/http",
        "mxc://",
        "mxc://hs.example.com",
    ):
        assert mxc_to_http(url, HOMESERVER) == url


# ----------------------------------------------------------------------
# fetch_to_file
# ----------------------------------------------------------------------


def test_fetch_to_file_writes_bytes(tmp_path):
    fetch = FakeFetch(b"hello media")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(cache.fetch_to_file(MHC, "a_b.jpg"))

    assert dest == tmp_path / "media" / "a_b.jpg"
    assert dest.read_bytes() == b"hello media"
    assert fetch.calls == [MHC]


def test_fetch_to_file_creates_missing_dir(tmp_path):
    fetch = FakeFetch(b"x")
    cache = MediaCache(fetch, tmp_path / "deep" / "media")

    dest = _run(cache.fetch_to_file(MHC, "f.bin"))

    assert dest is not None
    assert dest.read_bytes() == b"x"


def test_fetch_to_file_fetch_error_returns_none(tmp_path, caplog):
    fetch = FakeFetch(b"", raise_exc=ConnectionError("boom"))
    cache = MediaCache(fetch, tmp_path / "media")

    with caplog.at_level("WARNING", logger="agentteams.matrix.core.media"):
        dest = _run(cache.fetch_to_file(MHC, "f.bin"))

    assert dest is None
    assert not (tmp_path / "media" / "f.bin").exists()
    assert any(
        "MatrixChannel: failed to download" in record.getMessage()
        for record in caplog.records
    )


def test_fetch_to_file_non_mxc_returns_none_without_fetch(tmp_path):
    fetch = FakeFetch(b"x")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_to_file("https://plain.example.com/x", "f.bin")
    )

    assert dest is None
    assert fetch.calls == []


# ----------------------------------------------------------------------
# fetch_encrypted_to_file
# ----------------------------------------------------------------------


def test_fetch_encrypted_to_file_non_mxc_returns_none_without_fetch(tmp_path):
    fetch = FakeFetch(b"x")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            "https://plain.example.com/x", "f.bin", {"k": "k"}, {"sha256": "h"}, "iv"
        )
    )

    assert dest is None
    assert fetch.calls == []


@pytest.mark.skipif(
    not HAVE_NIO_CRYPTO, reason="matrix-nio crypto not importable here"
)
def test_fetch_encrypted_to_file_real_nio_roundtrip(tmp_path):
    """Real matrix-nio encrypt → core decrypt: the file holds the plaintext.

    ``encrypt_attachment`` returns ``(ciphertext, meta)`` where ``meta``
    carries the JWK ``key`` object (with its ``k`` field), the ``hashes``
    object (with ``sha256``) and the ``iv`` — the same shape the channel
    passes from an encrypted event's content.
    """
    plaintext = b"secret attachment payload"
    ciphertext, meta = _real_encrypt(plaintext)
    fetch = FakeFetch(ciphertext)
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            MHC, "enc.bin", meta["key"], meta["hashes"], meta["iv"]
        )
    )

    assert dest == tmp_path / "media" / "enc.bin"
    assert dest.read_bytes() == plaintext


@pytest.mark.skipif(
    not HAVE_NIO_CRYPTO, reason="matrix-nio crypto not importable here"
)
def test_fetch_encrypted_to_file_bad_hash_returns_none(tmp_path):
    """A mismatched SHA-256 digest makes nio raise; core degrades to None."""
    plaintext = b"secret attachment payload"
    ciphertext, meta = _real_encrypt(plaintext)
    bad_hashes = {"sha256": "A" * 44}
    fetch = FakeFetch(ciphertext)
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            MHC, "enc.bin", meta["key"], bad_hashes, meta["iv"]
        )
    )

    assert dest is None
    assert not (tmp_path / "media" / "enc.bin").exists()


@pytest.mark.skipif(HAVE_NIO_CRYPTO, reason="real nio crypto available here")
def test_fetch_encrypted_to_file_fake_decrypt_writes_plaintext(
    tmp_path, monkeypatch
):
    def fake_decrypt(data, jwk_key, sha256_hash, iv):
        assert data == b"cipher-bytes"
        return b"PLAINTEXT"

    _install_fake_decrypt(monkeypatch, fake_decrypt)
    fetch = FakeFetch(b"cipher-bytes")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            MHC, "enc.bin", {"k": "k"}, {"sha256": "h"}, "iv"
        )
    )

    assert dest == tmp_path / "media" / "enc.bin"
    assert dest.read_bytes() == b"PLAINTEXT"


@pytest.mark.skipif(HAVE_NIO_CRYPTO, reason="real nio crypto available here")
def test_fetch_encrypted_to_file_fake_decrypt_error_returns_none(
    tmp_path, monkeypatch
):
    def fake_decrypt(data, jwk_key, sha256_hash, iv):
        raise ValueError("hash mismatch")

    _install_fake_decrypt(monkeypatch, fake_decrypt)
    fetch = FakeFetch(b"cipher-bytes")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            MHC, "enc.bin", {"k": "k"}, {"sha256": "h"}, "iv"
        )
    )

    assert dest is None
    assert not (tmp_path / "media" / "enc.bin").exists()


# ----------------------------------------------------------------------
# no nio at core import time
# ----------------------------------------------------------------------


def test_core_media_import_does_not_pull_in_nio():
    """Importing core.media (and the core package) must not import nio."""
    code = (
        "import sys; "
        f"sys.path.insert(0, {str(BUNDLE_DIR)!r}); "
        "import core.media; "
        "print(any(name.startswith('nio') for name in sys.modules))"
    )
    result = subprocess.run(
        [sys.executable, "-c", code],
        capture_output=True,
        text=True,
        check=True,
    )
    assert result.stdout.strip() == "False"


# ----------------------------------------------------------------------
# sender-controlled filename containment (cache layer)
# ----------------------------------------------------------------------


def test_safe_cache_name_never_escapes():
    assert safe_cache_name(MHC, "../sentinel.txt") == "sentinel.txt"
    assert safe_cache_name(MHC, "..\\windows\\x.txt") == "x.txt"
    assert safe_cache_name(MHC, "/tmp/agentteams-escape-sentinel") == (
        "agentteams-escape-sentinel"
    )
    assert safe_cache_name(MHC, "normal-name_1.2.jpg") == "normal-name_1.2.jpg"
    # Empty / dot-only names fall back to a stable 32-char mxc hash.
    hashed_empty = safe_cache_name(MHC, "")
    assert len(hashed_empty) == 32
    assert safe_cache_name(MHC, None) == hashed_empty
    assert safe_cache_name(MHC, "..") == safe_cache_name(MHC, ".")
    # Overlong names are truncated to the cap.
    assert safe_cache_name(MHC, "a" * 200) == "a" * 120


def test_fetch_to_file_traversal_name_stays_in_cache(tmp_path):
    fetch = FakeFetch(b"traversal")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(cache.fetch_to_file(MHC, "../sentinel.txt"))

    assert dest is not None
    assert dest == tmp_path / "media" / "sentinel.txt"
    assert dest.read_bytes() == b"traversal"
    assert not (tmp_path / "sentinel.txt").exists()


def test_fetch_to_file_absolute_path_stays_in_cache(tmp_path):
    fetch = FakeFetch(b"absolute")
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(cache.fetch_to_file(MHC, "/tmp/agentteams-escape-sentinel"))

    assert dest is not None
    assert dest == tmp_path / "media" / "agentteams-escape-sentinel"
    assert dest.read_bytes() == b"absolute"
    assert not Path("/tmp/agentteams-escape-sentinel").exists()


def _encrypted_fetch(monkeypatch) -> tuple:
    """(fetch, key, hashes, iv) for a decryptable payload in either env:
    the real matrix-nio round trip when the crypto backend is
    importable, the fake-decrypt stand-in at the lazy import point
    otherwise."""
    if HAVE_NIO_CRYPTO:
        ciphertext, meta = _real_encrypt(b"PLAINTEXT")
        return FakeFetch(ciphertext), meta["key"], meta["hashes"], meta["iv"]
    _install_fake_decrypt(monkeypatch, lambda data, k, h, iv: b"PLAINTEXT")
    return FakeFetch(b"cipher-bytes"), {"k": "k"}, {"sha256": "h"}, "iv"


def test_fetch_encrypted_to_file_traversal_name_stays_in_cache(
    tmp_path, monkeypatch
):
    fetch, key, hashes, iv = _encrypted_fetch(monkeypatch)
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            MHC, "../sentinel.txt", key, hashes, iv
        )
    )

    assert dest is not None
    assert dest == tmp_path / "media" / "sentinel.txt"
    assert dest.read_bytes() == b"PLAINTEXT"
    assert not (tmp_path / "sentinel.txt").exists()


def test_fetch_encrypted_to_file_absolute_path_stays_in_cache(
    tmp_path, monkeypatch
):
    fetch, key, hashes, iv = _encrypted_fetch(monkeypatch)
    cache = MediaCache(fetch, tmp_path / "media")

    dest = _run(
        cache.fetch_encrypted_to_file(
            MHC, "/tmp/agentteams-escape-sentinel", key, hashes, iv
        )
    )

    assert dest is not None
    assert dest == tmp_path / "media" / "agentteams-escape-sentinel"
    assert dest.read_bytes() == b"PLAINTEXT"
    assert not Path("/tmp/agentteams-escape-sentinel").exists()
