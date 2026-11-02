"""Inbound media retrieval: a real HTTP roundtrip, then the frame contract.

The OpenClaw model pipeline only hydrates attachments it can read from disk,
so the sidecar downloads each ``mxc://`` reference and adds ``local_path``.
These tests run a real local HTTP server (no Matrix server, no network) and
assert the bytes, the auth header, the frame enrichment, and the
best-effort failure path.
"""

from __future__ import annotations

import http.server
import threading
import urllib.parse

import pytest

from sidecar import media
from sidecar.bridge import AgentBridge

PNG = (
    b"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01"
    b"\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\nIDATx\x9cc\x00"
    b"\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"
)


class _Handler(http.server.BaseHTTPRequestHandler):
    seen: list = []

    def do_GET(self):  # noqa: N802 - http.server API
        type(self).seen.append(
            {"path": self.path, "auth": self.headers.get("Authorization", "")}
        )
        if self.path.startswith("/_matrix/client/v1/media/download/"):
            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.send_header("Content-Length", str(len(PNG)))
            self.end_headers()
            self.wfile.write(PNG)
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, *_args):  # keep test output clean
        pass


@pytest.fixture()
def http_server():
    _Handler.seen = []
    server = http.server.HTTPServer(("127.0.0.1", 0), _Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_address[1]}", _Handler.seen
    finally:
        server.shutdown()
        server.server_close()


def test_fetch_media_bytes_roundtrip_with_bearer_auth(http_server):
    base, seen = http_server
    data = media.fetch_media_bytes(base, "syt-token", "mxc://example.org/AbCdEf")
    assert data == PNG
    assert len(seen) == 1
    assert seen[0]["auth"] == "Bearer syt-token"
    assert seen[0]["path"] == "/_matrix/client/v1/media/download/example.org/AbCdEf"


def test_fetch_media_bytes_rejects_non_mxc():
    with pytest.raises(ValueError):
        media.fetch_media_bytes("http://127.0.0.1:9", "t", "https://example.org/x.png")


def test_fetch_frame_media_adds_local_path(tmp_path, http_server):
    base, _ = http_server
    frame = {
        "type": "inbound",
        "event_id": "$evt-image",
        "room_id": "!room:example.org",
        "text": "reviewer-media-probe.png",
        "media": [
            {
                "mxc_uri": "mxc://example.org/AbCdEf",
                "mime_type": "image/png",
                "file_name": "reviewer-media-probe.png",
                "size": len(PNG),
            }
        ],
    }
    fetched = media.fetch_frame_media(
        base, "syt-token", frame, str(tmp_path / "media"), log=lambda _m: None
    )
    assert fetched == 1
    ref = frame["media"][0]
    assert ref["local_size"] == len(PNG)
    path = ref["local_path"]
    assert path.startswith(str(tmp_path / "media"))
    with open(path, "rb") as handle:
        assert handle.read() == PNG


def test_fetch_frame_media_sniffs_a_missing_content_type(tmp_path, http_server):
    """The SDK decides image-ness from contentType/fileName; the bytes win."""

    base, _ = http_server
    frame = {
        "event_id": "$evt-image",
        "media": [{"mxc_uri": "mxc://example.org/AbCdEf"}],  # no mime_type, no name
    }
    fetched = media.fetch_frame_media(
        base, "syt-token", frame, str(tmp_path / "media"), log=lambda _m: None
    )
    assert fetched == 1
    assert frame["media"][0]["mime_type"] == "image/png"


def test_fetch_frame_media_is_best_effort_on_failure(tmp_path, monkeypatch):
    frame = {
        "event_id": "$evt-image",
        "media": [{"mxc_uri": "mxc://example.org/Missing", "file_name": "x.png"}],
    }

    def boom(*_a, **_k):
        raise RuntimeError("404")

    monkeypatch.setattr(media, "fetch_media_bytes", boom)
    logs: list[str] = []
    fetched = media.fetch_frame_media(
        "http://127.0.0.1:9", "t", frame, str(tmp_path / "media"), log=logs.append
    )
    assert fetched == 0
    assert "local_path" not in frame["media"][0]
    assert logs and "media download failed" in logs[0]


def test_bridge_enriches_frame_before_send(monkeypatch):
    """The bridge calls the enricher and still forwards on enricher failure."""

    from sidecar import _core

    core = _core.load_core().core

    def core_ref():
        return core.MediaRef(mxc_uri="mxc://example.org/AbCdEf", file_name="probe.png")

    class Server:
        def __init__(self):
            self.sent = []

        def send(self, frame):
            self.sent.append(frame)

    class Event:
        event_id = "$evt-1"
        room_id = "!room:example.org"
        sender = "@alice:example.org"
        text = "hello"
        media = [core_ref()]
        mentions = []
        thread_root = None
        ts = 1_761_000_000_000

    def enrich(frame):
        frame["media"][0]["local_path"] = "/tmp/fake.png"
        return 1

    server = Server()
    bridge = AgentBridge(server, "@agent:example.org", log=lambda _m: None, media_enricher=enrich)
    import asyncio as _asyncio

    _asyncio.run(bridge.on_inbound(Event()))
    assert server.sent and server.sent[0]["media"][0]["local_path"] == "/tmp/fake.png"

    def explode(_frame):
        raise RuntimeError("download exploded")

    server2 = Server()
    bridge2 = AgentBridge(
        server2, "@agent:example.org", log=lambda _m: None, media_enricher=explode
    )
    _asyncio.run(bridge2.on_inbound(Event()))
    assert server2.sent, "a failing enricher must not drop the frame"
    assert "local_path" not in server2.sent[0]["media"][0]


class _RedirectHandler(http.server.BaseHTTPRequestHandler):
    """302 to ``target`` for the Matrix download path; records auth on /final."""

    target = ""
    seen: list = []

    def do_GET(self):  # noqa: N802 - http.server API
        type(self).seen.append(
            {"path": self.path, "auth": self.headers.get("Authorization", "")}
        )
        if self.path.startswith("/_matrix/client/v1/media/download/"):
            self.send_response(302)
            self.send_header("Location", type(self).target)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if self.path == "/final":
            self.send_response(200)
            self.send_header("Content-Length", str(len(PNG)))
            self.end_headers()
            self.wfile.write(PNG)
            return
        self.send_response(404)
        self.end_headers()

    def log_message(self, *_args):
        pass


def _serve(handler_cls):
    server = http.server.HTTPServer(("127.0.0.1", 0), handler_cls)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def test_redirect_same_origin_keeps_the_bearer():
    handler = type("SameOrigin", (_RedirectHandler,), {"target": "/final", "seen": []})
    server = _serve(handler)
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"
        handler.target = "/final"
        data = media.fetch_media_bytes(base, "syt-token", "mxc://example.org/AbCdEf")
        assert data == PNG
        final = [entry for entry in handler.seen if entry["path"] == "/final"]
        assert final and final[0]["auth"] == "Bearer syt-token"
    finally:
        server.shutdown()
        server.server_close()


def test_redirect_cross_origin_strips_the_bearer():
    """A homeserver must not be able to bounce the agent token off-origin."""

    target_handler = type("Target", (_RedirectHandler,), {"target": "", "seen": []})
    target_server = _serve(target_handler)
    origin_handler = type("Origin", (_RedirectHandler,), {"target": "", "seen": []})
    origin_server = _serve(origin_handler)
    try:
        target_url = f"http://127.0.0.1:{target_server.server_address[1]}/final"
        origin_handler.target = target_url
        base = f"http://127.0.0.1:{origin_server.server_address[1]}"
        data = media.fetch_media_bytes(base, "syt-token", "mxc://example.org/AbCdEf")
        # The bytes still arrive (the redirect is followed, just without the
        # credential) …
        assert data == PNG
        # … and the other origin never saw the bearer.
        assert target_handler.seen, "the redirect target must have been called"
        assert all(entry["auth"] == "" for entry in target_handler.seen), target_handler.seen
    finally:
        origin_server.shutdown()
        origin_server.server_close()
        target_server.shutdown()
        target_server.server_close()
