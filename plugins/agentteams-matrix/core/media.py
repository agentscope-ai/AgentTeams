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

import logging
from pathlib import Path
from typing import Awaitable, Callable, Dict, Optional

__all__ = [
    "MediaCache",
    "mxc_to_http",
]

logger = logging.getLogger("agentteams.matrix.core.media")


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
        """Download mxc:// to ``media_dir/filename``; return path or None."""
        if not mxc_url.startswith("mxc://"):
            return None
        try:
            data = await self._fetch(mxc_url)
            self._media_dir.mkdir(parents=True, exist_ok=True)
            dest = self._media_dir / filename
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
            dest = self._media_dir / filename
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
