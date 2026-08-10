"""Minimal Matrix Client-Server API client (stdlib only).

This is the ONLY data channel between the host machine and the Agent Team
running inside Docker. Every message rendered by either view is fetched
through this client from the Tuwunel homeserver inside the container.
No mock data, no hardcoded conversations.
"""

from __future__ import annotations

import json
import os
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


DEFAULT_ENV_CANDIDATES = [
    os.path.join(os.path.expanduser("~"), "agentteams-manager.env"),
    os.path.join(os.getcwd(), "agentteams-manager.env"),
]


def load_env_file(path: str | None = None) -> dict[str, str]:
    """Read the agentteams-manager.env produced by the official installer."""
    candidates = [path] if path else DEFAULT_ENV_CANDIDATES
    for candidate in candidates:
        if candidate and os.path.isfile(candidate):
            data: dict[str, str] = {}
            with open(candidate, "r", encoding="utf-8", errors="replace") as handle:
                for raw in handle:
                    line = raw.strip()
                    if not line or line.startswith("#") or "=" not in line:
                        continue
                    key, _, value = line.partition("=")
                    data[key.strip()] = value.strip().strip('"').strip("'")
            data["__env_file__"] = candidate
            return data
    return {}


class MatrixError(RuntimeError):
    pass


class MatrixClient:
    """Talks to the homeserver through the Higress gateway published on the host.

    We connect to 127.0.0.1:<gateway_port> and set the Host header to the
    virtual domain, so the demo works without touching the hosts file.
    """

    def __init__(
        self,
        connect_url: str,
        host_header: str,
        server_name: str,
        timeout: int = 70,
    ) -> None:
        self.connect_url = connect_url.rstrip("/")
        self.host_header = host_header
        self.server_name = server_name
        self.timeout = timeout
        self.token: str | None = None
        self.user_id: str | None = None
        self._ssl_ctx = ssl.create_default_context()
        self._ssl_ctx.check_hostname = False
        self._ssl_ctx.verify_mode = ssl.CERT_NONE

    # ------------------------------------------------------------------
    # low level
    # ------------------------------------------------------------------
    def _request(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        timeout: int | None = None,
    ) -> dict[str, Any]:
        url = f"{self.connect_url}{path}"
        data = json.dumps(body).encode("utf-8") if body is not None else None
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Host", self.host_header)
        req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")
        try:
            with urllib.request.urlopen(
                req, timeout=timeout or self.timeout, context=self._ssl_ctx
            ) as resp:
                payload = resp.read().decode("utf-8", errors="replace")
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")
            raise MatrixError(f"{method} {path} -> HTTP {exc.code}: {detail[:400]}") from exc
        except urllib.error.URLError as exc:
            raise MatrixError(f"{method} {path} -> connection failed: {exc.reason}") from exc
        if not payload:
            return {}
        try:
            return json.loads(payload)
        except json.JSONDecodeError:
            return {"raw": payload}

    # ------------------------------------------------------------------
    # auth
    # ------------------------------------------------------------------
    def login(self, user: str, password: str) -> str:
        resp = self._request(
            "POST",
            "/_matrix/client/v3/login",
            {
                "type": "m.login.password",
                "identifier": {"type": "m.id.user", "user": user},
                "password": password,
            },
            timeout=30,
        )
        token = resp.get("access_token")
        if not token:
            raise MatrixError(f"login failed, no access_token in response: {resp}")
        self.token = token
        self.user_id = resp.get("user_id") or f"@{user}:{self.server_name}"
        return token

    def whoami(self) -> dict[str, Any]:
        return self._request("GET", "/_matrix/client/v3/account/whoami", timeout=20)

    # ------------------------------------------------------------------
    # rooms
    # ------------------------------------------------------------------
    @staticmethod
    def _encode_room(room_id: str) -> str:
        return urllib.parse.quote(room_id, safe="")

    def joined_rooms(self) -> list[str]:
        return self._request("GET", "/_matrix/client/v3/joined_rooms", timeout=30).get(
            "joined_rooms", []
        )

    def room_members(self, room_id: str) -> list[str]:
        resp = self._request(
            "GET",
            f"/_matrix/client/v3/rooms/{self._encode_room(room_id)}/members",
            timeout=30,
        )
        members = []
        for event in resp.get("chunk", []):
            if event.get("content", {}).get("membership") == "join":
                members.append(event.get("state_key"))
        return [m for m in members if m]

    def room_name(self, room_id: str) -> str | None:
        try:
            resp = self._request(
                "GET",
                f"/_matrix/client/v3/rooms/{self._encode_room(room_id)}/state/m.room.name/",
                timeout=20,
            )
            return resp.get("name")
        except MatrixError:
            return None

    def create_room(
        self,
        name: str,
        invite: list[str] | None = None,
        topic: str | None = None,
        is_direct: bool = False,
    ) -> str:
        body: dict[str, Any] = {
            "name": name,
            "preset": "trusted_private_chat",
            "invite": invite or [],
        }
        if topic:
            body["topic"] = topic
        if is_direct:
            body["is_direct"] = True
        resp = self._request("POST", "/_matrix/client/v3/createRoom", body, timeout=45)
        room_id = resp.get("room_id")
        if not room_id:
            raise MatrixError(f"createRoom failed: {resp}")
        return room_id

    def invite(self, room_id: str, user_id: str) -> None:
        self._request(
            "POST",
            f"/_matrix/client/v3/rooms/{self._encode_room(room_id)}/invite",
            {"user_id": user_id},
            timeout=30,
        )

    def send_text(self, room_id: str, body: str) -> str:
        txn = f"wxsim{int(time.time() * 1000)}{os.getpid() % 1000}"
        resp = self._request(
            "PUT",
            f"/_matrix/client/v3/rooms/{self._encode_room(room_id)}/send/m.room.message/{txn}",
            {"msgtype": "m.text", "body": body},
            timeout=45,
        )
        return resp.get("event_id", "")

    def messages(self, room_id: str, limit: int = 50, direction: str = "b") -> list[dict]:
        resp = self._request(
            "GET",
            f"/_matrix/client/v3/rooms/{self._encode_room(room_id)}/messages"
            f"?dir={direction}&limit={limit}",
            timeout=45,
        )
        return resp.get("chunk", [])

    # ------------------------------------------------------------------
    # sync
    # ------------------------------------------------------------------
    def sync(self, since: str | None = None, timeout_ms: int = 25000) -> dict[str, Any]:
        params = {"timeout": str(timeout_ms), "full_state": "false"}
        if since:
            params["since"] = since
        query = urllib.parse.urlencode(params)
        return self._request(
            "GET",
            f"/_matrix/client/v3/sync?{query}",
            timeout=(timeout_ms // 1000) + 25,
        )


def build_client_from_env(env: dict[str, str] | None = None) -> tuple[MatrixClient, dict[str, str]]:
    """Construct a client using the installer-generated env file."""
    env = env if env is not None else load_env_file()
    domain = env.get("AGENTTEAMS_MATRIX_DOMAIN", "matrix-local.agentteams.io:18080")
    gateway_port = env.get("AGENTTEAMS_PORT_GATEWAY", "").strip()
    if not gateway_port:
        gateway_port = domain.rsplit(":", 1)[-1] if ":" in domain else "18080"
    connect = os.environ.get("WX_MATRIX_CONNECT_URL", f"http://127.0.0.1:{gateway_port}")
    client = MatrixClient(connect_url=connect, host_header=domain, server_name=domain)
    return client, env
