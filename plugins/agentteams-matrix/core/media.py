"""Media handling for the AgentTeams Matrix core (P9, PR1 M4c).

The mxc download path (plain and encrypted attachments) moved out of
the QwenPaw channel: :func:`mxc_to_http` is the pure mxc→http URL
conversion, and :class:`MediaCache` downloads to a local directory
with the authenticated HTTP fetch injected — the access token and the
httpx client stay in the adapter, never in the core.

The encrypted-attachment decryption lazily imports
``nio.crypto.attachments`` inside the function, so importing this
module (and ``core/__init__``) never pulls in matrix-nio.

The behaviour is the channel's legacy behaviour, moved verbatim: same
log message wording, same warning-on-failure degradation, same
``None`` return on failure.
"""

from __future__ import annotations

import hashlib
import logging
import re
from pathlib import Path
from typing import Awaitable, Callable, Dict, Optional

__all__ = [
    "MediaCache",
    "mxc_to_http",
    "safe_cache_name",
]

logger = logging.getLogger("agentteams.matrix.core.media")

# Sender-controlled filenames are never allowed to decide the cache
# path: the name is reduced to a sanitized basename (traversal and
# absolute-path separators are stripped, anything outside
# [A-Za-z0-9._-] is replaced, leading/trailing dots and underscores
# are trimmed).  A name that sanitizes to nothing falls back to a
# hash of the mxc URL, so distinct media never collide.
_SAFE_NAME_RE = re.compile(r"[^A-Za-z0-9._-]+")
_MAX_NAME = 120


def safe_cache_name(mxc_url: str, filename: str) -> str:
    """Sender-controlled names never decide the path: keep a sanitized basename."""
    candidate = str(filename or "").replace("\\", "/").rsplit("/", 1)[-1]
    candidate = _SAFE_NAME_RE.sub("_", candidate).strip("._")
    if not candidate or candidate in {".", ".."} or set(candidate) == {"."}:
        candidate = hashlib.sha256(mxc_url.encode("utf-8")).hexdigest()[:32]
    if len(candidate) > _MAX_NAME:
        candidate = candidate[:_MAX_NAME]
    return candidate


def mxc_to_http(mxc_url: str, homeserver: str) -> str:
    """Convert an mxc:// URL to an HTTP download URL.

    Returns the original URL unchanged if it is not an mxc:// URL or if
    the format is invalid.
    """
    if not mxc_url:
        return mxc_url
    if not mxc_url.startswith("mxc://"):
        return mxc_url
    rest = mxc_url[6:]  # strip "mxc://"
    if "/" not in rest:
        return mxc_url
    server, media_id = rest.split("/", 1)
    return (
        f"{homeserver}/_matrix/media/v3/download"
        f"/{server}/{media_id}"
    )


class MediaCache:
    """Download mxc:// media into a local directory (P9, PR1 M4c).

    Parameters
    ----------
    fetch:
        Async callback performing the authenticated HTTP GET for one
        mxc:// URL and returning the raw response bytes.  The adapter
        resolves the URL against its homeserver and applies the
        access token; the core neither reads the token nor builds an
        HTTP client.
    media_dir:
        Directory files are written into (created on demand).
        The filename is always decided by the caller.
    """

    def __init__(
        self,
        fetch: Callable[[str], Awaitable[bytes]],
        media_dir: Path,
    ) -> None:
        self._fetch = fetch
        self._media_dir = Path(media_dir)

    async def fetch_to_file(
        self,
        mxc_url: str,
        filename: str,
    ) -> Optional[Path]:
        """Download mxc:// into the media cache; return path or None.

        ``filename`` is sender-controlled (the media event body), so
        the cache path uses :func:`safe_cache_name` — traversal and
        absolute paths cannot escape the media dir.
        """
        if not mxc_url.startswith("mxc://"):
            return None
        try:
            data = await self._fetch(mxc_url)
            self._media_dir.mkdir(parents=True, exist_ok=True)
            dest = self._media_dir / safe_cache_name(mxc_url, filename)
            if not self._inside_media_dir(dest):
                return None
            dest.write_bytes(data)
            logger.debug("MatrixChannel: downloaded %s → %s", mxc_url, dest)
            return dest
        except Exception as exc:
            logger.warning(
                "MatrixChannel: failed to download %s: %s",
                mxc_url,
                exc,
            )
            return None

    def _inside_media_dir(self, dest: Path) -> bool:
        """Containment guard: *dest* must resolve inside the cache dir."""
        try:
            inside = dest.resolve().is_relative_to(self._media_dir.resolve())
        except OSError:
            inside = False
        if not inside:
            logger.warning(
                "MatrixChannel: refusing cache write outside %s: %s",
                self._media_dir,
                dest,
            )
        return inside

    async def fetch_encrypted_to_file(
        self,
        mxc_url: str,
        filename: str,
        key: Dict,
        hashes: Dict,
        iv: str,
    ) -> Optional[Path]:
        """Download an encrypted mxc:// URI, decrypt it, and save locally."""
        if not mxc_url.startswith("mxc://"):
            return None
        try:
            data = await self._fetch(mxc_url)
            # Lazy import: matrix-nio (and its crypto dependency) must
            # not be imported at core import time.
            from nio.crypto.attachments import decrypt_attachment

            jwk_key = key.get("k", "")
            sha256_hash = hashes.get("sha256", "")
            plaintext = decrypt_attachment(
                data,
                jwk_key,
                sha256_hash,
                iv,
            )
            self._media_dir.mkdir(parents=True, exist_ok=True)
            dest = self._media_dir / safe_cache_name(mxc_url, filename)
            if not self._inside_media_dir(dest):
                return None
            dest.write_bytes(plaintext)
            logger.debug(
                "MatrixChannel: downloaded+decrypted %s → %s",
                mxc_url,
                dest,
            )
            return dest
        except Exception as exc:
            logger.warning(
                "MatrixChannel: failed to download encrypted %s: %s",
                mxc_url,
                exc,
            )
            return None
