"""Inbound Matrix media retrieval for the OpenClaw channel.

Why this module exists: the OpenClaw model pipeline hydrates an attachment
only when it can read the bytes from disk — ``media://inbound/<id>``, a
``file:`` URL, or a plain filesystem path. A bare ``mxc://`` reference is
classified as an unsupported scheme, so the fact is dropped and an
image-capable model never sees an image block (the sidecar already holds the
bytes' address, but not a path). The sidecar owns the Matrix credentials, so
it downloads each inbound media reference once and enriches the frame with a
local path the plugin can hand to the SDK.

The download is best-effort by contract: a failure never drops the event.
The frame is still forwarded with the refs (no ``local_path``), so text-only
handling keeps working and the failure is visible in the sidecar log.
"""

from __future__ import annotations

import os
import re
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Optional

# One Matrix media ref is a few hundred KB at most in practice; cap the read
# so a hostile or broken home server cannot exhaust memory.
MAX_MEDIA_BYTES = 32 * 1024 * 1024
DEFAULT_TIMEOUT_S = 15.0

_MXC_RE = re.compile(r"^mxc://([^/]+)/(.+)$")
_SAFE_NAME_RE = re.compile(r"[^A-Za-z0-9._-]+")


def _download_url(homeserver: str, mxc_uri: str) -> str:
    """Translate ``mxc://server/mediaId`` into the authenticated download URL.

    Uses the Matrix 1.11 client endpoint (``/_matrix/client/v1/media/download``)
    which takes ``Authorization: Bearer``. The legacy media endpoint
    (``/_matrix/media/v3/download``) is tried as a fallback by the caller.
    """
    match = _MXC_RE.match(mxc_uri.strip())
    if not match:
        raise ValueError(f"not an mxc uri: {mxc_uri!r}")
    server, media_id = match.group(1), match.group(2)
    base = homeserver.rstrip("/")
    return (
        f"{base}/_matrix/client/v1/media/download/"
        f"{urllib.parse.quote(server, safe='')}/{urllib.parse.quote(media_id, safe='')}"
    )


def _legacy_url(homeserver: str, mxc_uri: str) -> str:
    match = _MXC_RE.match(mxc_uri.strip())
    if not match:
        raise ValueError(f"not an mxc uri: {mxc_uri!r}")
    server, media_id = match.group(1), match.group(2)
    base = homeserver.rstrip("/")
    return (
        f"{base}/_matrix/media/v3/download/"
        f"{urllib.parse.quote(server, safe='')}/{urllib.parse.quote(media_id, safe='')}"
    )


def _safe_file_name(name: str, fallback: str) -> str:
    candidate = _SAFE_NAME_RE.sub("_", (name or "").strip())[:80].strip("._")
    return candidate or fallback


def _read_limited(response: Any) -> bytes:
    length = response.headers.get("Content-Length")
    if length is not None:
        try:
            if int(length) > MAX_MEDIA_BYTES:
                raise ValueError(f"media too large: {length} bytes")
        except ValueError:
            raise
    chunks = []
    total = 0
    while True:
        chunk = response.read(64 * 1024)
        if not chunk:
            break
        total += len(chunk)
        if total > MAX_MEDIA_BYTES:
            raise ValueError(f"media too large: more than {MAX_MEDIA_BYTES} bytes")
        chunks.append(chunk)
    return b"".join(chunks)


def fetch_media_bytes(
    homeserver: str,
    access_token: str,
    mxc_uri: str,
    *,
    timeout: float = DEFAULT_TIMEOUT_S,
) -> bytes:
    """Download one mxc reference with the agent's own credentials."""

    headers = {"Authorization": f"Bearer {access_token}"}
    last_error: Optional[Exception] = None
    for url in (_download_url(homeserver, mxc_uri), _legacy_url(homeserver, mxc_uri)):
        try:
            request = urllib.request.Request(url, headers=headers, method="GET")
            with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - config-owned base URL
                return _read_limited(response)
        except urllib.error.HTTPError as exc:  # 404 on the v1 endpoint -> legacy
            last_error = exc
            continue
        except Exception as exc:  # network/parse failure: report, do not retry blind
            last_error = exc
            break
    raise RuntimeError(f"media download failed: {last_error}")


def fetch_frame_media(
    homeserver: str,
    access_token: str,
    frame: dict,
    media_dir: str,
    *,
    log: Optional[Callable[[str], None]] = None,
    timeout: float = DEFAULT_TIMEOUT_S,
) -> int:
    """Download every media ref of one inbound frame, in place.

    Adds ``local_path`` to each ref that was fetched. Returns the number of
    refs materialized. Never raises: the frame must still reach the plugin.
    """

    refs = frame.get("media") or []
    if not isinstance(refs, list) or not refs:
        return 0
    try:
        os.makedirs(media_dir, mode=0o700, exist_ok=True)
    except OSError as exc:
        if log:
            log(f"media dir unavailable ({media_dir}): {exc}")
        return 0

    event_id = _safe_file_name(str(frame.get("event_id") or "event"), "event")
    fetched = 0
    for index, ref in enumerate(refs):
        if not isinstance(ref, dict):
            continue
        mxc_uri = str(ref.get("mxc_uri") or "").strip()
        if not mxc_uri:
            continue
        try:
            data = fetch_media_bytes(
                homeserver, access_token, mxc_uri, timeout=timeout
            )
        except Exception as exc:  # best-effort: keep forwarding the frame
            if log:
                log(f"media download failed for {mxc_uri}: {exc}")
            continue
        name = _safe_file_name(
            str(ref.get("file_name") or ""), f"media-{index}.bin"
        )
        path = os.path.join(media_dir, f"{event_id}-{index}-{name}")
        try:
            with open(path, "wb") as handle:
                handle.write(data)
            os.chmod(path, 0o600)
        except OSError as exc:
            if log:
                log(f"media write failed ({path}): {exc}")
            continue
        ref["local_path"] = path
        ref["local_size"] = len(data)
        fetched += 1
    return fetched
