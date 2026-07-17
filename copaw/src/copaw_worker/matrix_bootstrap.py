"""Matrix HTTP bootstrap before CoPaw's channel loop starts."""

from __future__ import annotations

import json
import logging
import urllib.error
import urllib.parse
import urllib.request
from typing import TYPE_CHECKING, Any

from copaw_worker.bridge import _is_in_container, _port_remap

if TYPE_CHECKING:
    from copaw_worker.sync import FileSync

logger = logging.getLogger(__name__)


class MatrixBootstrapClient:
    """Re-login and accept pending invites via Matrix Client-Server API."""

    def __init__(self, sync: FileSync, *, worker_name: str) -> None:
        self._sync = sync
        self._worker_name = worker_name

    def relogin(self, openclaw_cfg: dict[str, Any]) -> dict[str, Any]:
        """Refresh access token so E2EE gets a new device_id after restart."""
        password_key = f"{self._sync._prefix}/credentials/matrix/password"
        matrix_password = self._sync._cat(password_key)
        if not matrix_password:
            logger.info(
                "No Matrix password in MinIO; skipping re-login "
                "(E2EE may not work after restart)"
            )
            return openclaw_cfg

        matrix_password = matrix_password.strip()
        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        homeserver = _port_remap(
            matrix_cfg.get("homeserver", ""), _is_in_container()
        )
        if not homeserver or not matrix_password:
            return openclaw_cfg

        login_url = f"{homeserver}/_matrix/client/v3/login"
        login_body = json.dumps(
            {
                "type": "m.login.password",
                "identifier": {"type": "m.id.user", "user": self._worker_name},
                "password": matrix_password,
            }
        ).encode()

        try:
            req = urllib.request.Request(
                login_url,
                data=login_body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                login_resp = json.loads(resp.read())

            new_token = login_resp.get("access_token", "")
            new_device = login_resp.get("device_id", "")
            if not new_token:
                logger.warning(
                    "Matrix re-login returned no token; using existing access token"
                )
                return openclaw_cfg

            openclaw_cfg["channels"]["matrix"]["accessToken"] = new_token
            config_path = self._sync.local_dir / "openclaw.json"
            with open(config_path, "w", encoding="utf-8") as handle:
                json.dump(openclaw_cfg, handle, indent=2, ensure_ascii=False)
            logger.info(
                "Matrix re-login OK (device: %s, token: %s...)",
                new_device,
                new_token[:10],
            )
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as exc:
            logger.warning(
                "Matrix re-login failed: %s — using existing access token "
                "(E2EE may not work)",
                exc,
            )

        return openclaw_cfg

    def join_pending_invites(self, openclaw_cfg: dict[str, Any]) -> None:
        """Accept pending room invites before the Matrix channel sync loop."""
        matrix_cfg = openclaw_cfg.get("channels", {}).get("matrix", {})
        access_token = matrix_cfg.get("accessToken", "")
        homeserver = _port_remap(
            matrix_cfg.get("homeserver", ""), _is_in_container()
        )
        if not homeserver or not access_token:
            return

        headers = {"Authorization": f"Bearer {access_token}"}
        sync_url = (
            f"{homeserver}/_matrix/client/v3/sync?"
            "timeout=0&full_state=true"
        )
        try:
            req = urllib.request.Request(sync_url, headers=headers, method="GET")
            with urllib.request.urlopen(req, timeout=30) as resp:
                data = json.loads(resp.read())
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as exc:
            logger.warning("Matrix pending invite sync failed: %s", exc)
            return

        invites = (data.get("rooms", {}).get("invite") or {}).keys()
        for room_id in invites:
            encoded = urllib.parse.quote(room_id, safe="")
            join_url = f"{homeserver}/_matrix/client/v3/join/{encoded}"
            try:
                req = urllib.request.Request(
                    join_url,
                    data=b"{}",
                    headers={**headers, "Content-Type": "application/json"},
                    method="POST",
                )
                with urllib.request.urlopen(req, timeout=30):
                    pass
                logger.info("Joined pending Matrix invite: %s", room_id)
            except (urllib.error.URLError, OSError) as exc:
                logger.warning(
                    "Matrix invite join failed for %s: %s", room_id, exc
                )
