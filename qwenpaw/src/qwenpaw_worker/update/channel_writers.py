"""Runtime desired-state update support for qwenpaw-worker."""

from __future__ import annotations

import json
import logging
import os
from typing import Any, Dict, List, Optional, Tuple

from qwenpaw_worker.config import WorkerConfig
from qwenpaw_worker.update.constants import DEFAULT_AGENT_ID
from qwenpaw_worker.update.runtime_config import MemberRuntimeConfig
from qwenpaw_worker.update.utils import _bool, _env_bool, _section, _string, _string_list

logger = logging.getLogger(__name__)


class ChannelWriterMixin:
    """Matrix/DingTalk channel and access-control writers."""

    config: WorkerConfig

    def _apply_channel_policy(self, config: MemberRuntimeConfig) -> None:
        group_allow, dm_allow, group_deny, dm_deny = self._matrix_policy_ids(config)
        if not (group_allow or dm_allow or group_deny or dm_deny):
            return

        self_allow = _string(config.member.get("matrixUserId"))
        self_allowlist = [self_allow] if self_allow else []
        whitelist = self._dedupe(self_allowlist + group_allow + dm_allow)
        blacklist = self._dedupe(group_deny + dm_deny)
        deny_set = set(blacklist)
        whitelist = [value for value in whitelist if value not in deny_set]
        self._apply_matrix_channel_access_flags(
            group_enabled=bool(group_allow or group_deny),
            dm_enabled=bool(dm_allow or dm_deny),
        )
        self._write_matrix_access_control(whitelist, blacklist)

    def _apply_matrix_channel(self, config: MemberRuntimeConfig) -> None:
        desired = self._matrix_channel_desired_state(config)
        if desired is None:
            return
        try:
            from qwenpaw.config.config import load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_matrix_channel action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "channels", None) is None:
            try:
                from qwenpaw.config.config import ChannelConfig
            except ImportError:
                return
            agent_config.channels = ChannelConfig()
        matrix_config = getattr(agent_config.channels, "matrix", None)
        if matrix_config is None:
            return

        changed = False
        desired_fields = {
            "enabled": True,
            "homeserver": desired["homeserver"],
            "user_id": desired["user_id"],
            "access_token": desired["access_token"],
            "password": "",
            "encryption": _env_bool("AGENTTEAMS_MATRIX_E2EE"),
            "group_disabled": False,
            "dm_disabled": False,
            "filter_tool_messages": False,
            "filter_thinking": False,
        }
        for field, value in desired_fields.items():
            if getattr(matrix_config, field, None) != value:
                setattr(matrix_config, field, value)
                changed = True

        groups = dict(getattr(matrix_config, "groups", None) or {})
        if self._ensure_require_mention_group(groups, "*"):
            changed = True
        room_id = desired["room_id"]
        if room_id:
            if self._ensure_require_mention_group(groups, room_id):
                changed = True
        if getattr(matrix_config, "groups", None) != groups:
            matrix_config.groups = groups
            changed = True

        if changed:
            save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _apply_dingtalk_channel(self, config: MemberRuntimeConfig) -> None:
        desired = config.dingtalk_channel
        if desired is None:
            return
        try:
            from qwenpaw.config.config import load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_dingtalk_channel action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "channels", None) is None:
            try:
                from qwenpaw.config.config import ChannelConfig
            except ImportError:
                return
            agent_config.channels = ChannelConfig()
        dingtalk_config = getattr(agent_config.channels, "dingtalk", None)
        if dingtalk_config is None:
            return

        changed = False
        if not _bool(desired.get("enabled")):
            if getattr(dingtalk_config, "enabled", None) is not False:
                dingtalk_config.enabled = False
                changed = True
            if changed:
                save_agent_config(DEFAULT_AGENT_ID, agent_config)
            return

        streaming_enabled = _bool(desired.get("streaming_enabled"))
        client_id = _string(desired.get("client_id"))
        client_secret = _string(desired.get("client_secret"))
        robot_code = _string(desired.get("robot_code"))
        desired_fields = {
            "enabled": True,
            "client_id": client_id,
            "client_secret": client_secret,
            "robot_code": robot_code,
            "filter_thinking": _bool(desired.get("filter_thinking")),
            "filter_tool_messages": _bool(desired.get("filter_tool_messages")),
            "streaming_enabled": streaming_enabled,
        }
        if streaming_enabled:
            missing = [
                name
                for name, value in (
                    ("client_id", client_id),
                    ("client_secret", client_secret),
                    ("robot_code", robot_code),
                    ("card_template_id", _string(desired.get("card_template_id"))),
                )
                if not value
            ]
            if missing:
                raise ValueError(
                    "DingTalk streaming requires client_id, client_secret, "
                    "robot_code, and card_template_id. Create and publish the "
                    "streaming card template in DingTalk Open Platform, select "
                    "card mode, then set card_template_id; missing "
                    f"{', '.join(missing)}"
                )
            card_template_id = _string(desired.get("card_template_id"))
            previous_message_type = _string(getattr(dingtalk_config, "message_type", ""))
            previous_template_id = _string(getattr(dingtalk_config, "card_template_id", ""))
            if (
                previous_message_type == "card"
                and previous_template_id
                and previous_template_id != card_template_id
            ):
                logger.warning(
                    "DingTalk streaming enabled; current runtime card configuration will switch "
                    "component=update step=apply_dingtalk_channel previous_template=%s next_template=%s "
                    "existing_template_deleted=False",
                    previous_template_id,
                    card_template_id,
                )
            desired_fields.update(
                {
                    "message_type": "card",
                    "card_template_id": card_template_id,
                    "card_template_key": _string(
                        desired.get("card_template_key") or "content"
                    ),
                    "card_auto_layout": False,
                }
            )
        else:
            if "message_type" in desired:
                desired_fields["message_type"] = _string(desired.get("message_type") or "markdown")
            if "card_template_id" in desired:
                desired_fields["card_template_id"] = _string(desired.get("card_template_id"))
            if "card_template_key" in desired:
                desired_fields["card_template_key"] = _string(
                    desired.get("card_template_key") or "content"
                )
            if "card_auto_layout" in desired:
                desired_fields["card_auto_layout"] = _bool(desired.get("card_auto_layout"))
        for field, value in desired_fields.items():
            if getattr(dingtalk_config, field, None) != value:
                setattr(dingtalk_config, field, value)
                changed = True

        if changed:
            save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _matrix_channel_desired_state(self, config: MemberRuntimeConfig) -> Optional[Dict[str, str]]:
        homeserver = _string(
            os.getenv("AGENTTEAMS_MATRIX_URL")
            or os.getenv("AGENTTEAMS_MATRIX_SERVER")
            or os.getenv("AGENTTEAMS_MATRIX_HOMESERVER")
        ).rstrip("/")
        user_id = _string(config.member.get("matrixUserId") or os.getenv("AGENTTEAMS_MATRIX_USER_ID"))
        if not user_id:
            matrix_domain = _string(os.getenv("AGENTTEAMS_MATRIX_DOMAIN"))
            if config.member_name and matrix_domain:
                user_id = f"@{config.member_name}:{matrix_domain}"
        token_env = _string(config.credentials.get("matrixTokenEnv") or "AGENTTEAMS_WORKER_MATRIX_TOKEN")
        access_token = _string(os.getenv(token_env)) if token_env else ""
        if not access_token:
            access_token = _string(os.getenv("AGENTTEAMS_MATRIX_TOKEN"))
        room_id = _string(config.team.get("teamRoomId") or config.member.get("personalRoomId"))
        if not (homeserver and user_id and access_token):
            return None
        return {
            "homeserver": homeserver,
            "user_id": user_id,
            "access_token": access_token,
            "room_id": room_id,
        }

    def _ensure_require_mention_group(self, groups: Dict[str, Any], room_id: str) -> bool:
        room_cfg = dict(groups.get(room_id) or {})
        changed = False
        if room_cfg.pop("autoReply", None) is not None:
            changed = True
        if room_cfg.get("requireMention") is not True:
            room_cfg["requireMention"] = True
            changed = True
        if changed:
            groups[room_id] = room_cfg
        return changed

    def _matrix_policy_ids(self, config: MemberRuntimeConfig) -> Tuple[List[str], List[str], List[str], List[str]]:
        policy = config.channel_policy
        domain = _string(os.getenv("AGENTTEAMS_MATRIX_DOMAIN"))
        group_allow = self._default_group_allow(config, domain)
        dm_allow = list(group_allow)
        group_allow.extend(self._matrix_ids(_string_list(policy.get("groupAllowExtra")), domain))
        dm_allow.extend(self._matrix_ids(_string_list(policy.get("dmAllowExtra")), domain))
        group_deny = self._matrix_ids(_string_list(policy.get("groupDenyExtra")), domain)
        dm_deny = self._matrix_ids(_string_list(policy.get("dmDenyExtra")), domain)
        return (
            self._dedupe(group_allow),
            self._dedupe(dm_allow),
            self._dedupe(group_deny),
            self._dedupe(dm_deny),
        )

    def _default_group_allow(self, config: MemberRuntimeConfig, domain: str) -> List[str]:
        team_admin = _string(_section(config.team, "admin").get("matrixUserId"))
        system_admin_user = _string(os.getenv("AGENTTEAMS_ADMIN_USER") or "admin")
        system_admin = self._matrix_id(system_admin_user, domain)
        admin = team_admin or system_admin
        roster_allow = self._team_roster_group_allow(config, domain, admin)
        if roster_allow:
            # Ensure system admin is always present even when team admin differs
            if system_admin and system_admin not in roster_allow:
                roster_allow.append(system_admin)
            return roster_allow
        if config.team and config.member_role not in {"team_leader", "leader"}:
            leader = _string(config.team.get("leaderRuntimeName") or config.team.get("leaderName"))
            return [item for item in (self._matrix_id(leader, domain), admin, system_admin) if item]
        manager = self._matrix_id("manager", domain)
        return [item for item in (manager, admin, system_admin) if item]

    def _team_roster_group_allow(self, config: MemberRuntimeConfig, domain: str, admin: str) -> List[str]:
        members = config.team_members
        if not config.team or not members:
            return []

        current_names = {
            _string(config.member.get("name")),
            _string(config.member.get("runtimeName")),
        }
        current_names.discard("")
        current_mxid = _string(config.member.get("matrixUserId"))
        leader_roles = {"team_leader", "leader"}
        leader_ids: List[str] = []
        peer_ids: List[str] = []

        for member in members:
            mxid = self._member_matrix_id(member, domain)
            if not mxid:
                continue
            if mxid == current_mxid or _string(member.get("runtimeName") or member.get("name")) in current_names:
                continue
            if _string(member.get("role")) in leader_roles:
                leader_ids.append(mxid)
            else:
                peer_ids.append(mxid)

        if config.member_role in leader_roles:
            manager = self._matrix_id("manager", domain)
            return [item for item in (manager, admin, *peer_ids) if item]

        leader = _string(config.team.get("leaderRuntimeName") or config.team.get("leaderName"))
        if not leader_ids:
            leader_ids = [self._matrix_id(leader, domain)]
        return [item for item in (*leader_ids, admin, *peer_ids) if item]

    def _member_matrix_id(self, member: Dict[str, str], domain: str) -> str:
        mxid = _string(member.get("matrixUserId"))
        if mxid:
            return mxid
        return self._matrix_id(_string(member.get("runtimeName") or member.get("name")), domain)

    def _matrix_ids(self, values: List[str], domain: str) -> List[str]:
        return [mxid for mxid in (self._matrix_id(value, domain) for value in values) if mxid]

    def _matrix_id(self, value: str, domain: str) -> str:
        text = _string(value)
        if not text:
            return ""
        if text.startswith("@") or text.startswith("!"):
            return text
        return f"@{text}:{domain}" if domain else ""

    def _mcporter_servers(self, config: MemberRuntimeConfig) -> Dict[str, Any]:
        raw = config.mcp_servers
        gateway_key = self._gateway_key(config)
        if isinstance(raw, dict) and isinstance(raw.get("mcpServers"), dict):
            raw = raw["mcpServers"]

        servers: Dict[str, Any] = {}
        if isinstance(raw, list):
            for item in raw:
                if isinstance(item, dict):
                    name = _string(item.get("name"))
                    payload = self._mcporter_server_payload(item, gateway_key)
                    if name and payload:
                        servers[name] = payload
            return servers

        if isinstance(raw, dict):
            for name, item in raw.items():
                if isinstance(item, dict):
                    payload = self._mcporter_server_payload(item, gateway_key)
                    if _string(name) and payload:
                        servers[_string(name)] = payload
        return servers

    def _mcporter_server_payload(self, item: Dict[str, Any], gateway_key: str) -> Dict[str, Any]:
        url = _string(item.get("url"))
        if not url:
            return {}
        headers = item.get("headers")
        headers = dict(headers) if isinstance(headers, dict) else {}
        if gateway_key and "Authorization" not in headers:
            headers["Authorization"] = f"Bearer {gateway_key}"
        return {
            "url": url,
            "transport": _string(item.get("transport") or "http"),
            "headers": headers,
        }

    def _gateway_key(self, config: MemberRuntimeConfig) -> str:
        env_name = _string(config.credentials.get("gatewayKeyEnv") or "AGENTTEAMS_WORKER_GATEWAY_KEY")
        return os.getenv(env_name, "") if env_name else ""

    def _apply_matrix_channel_access_flags(self, group_enabled: bool, dm_enabled: bool) -> None:
        try:
            from qwenpaw.config.config import load_agent_config, save_agent_config
        except ImportError:
            logger.info("qwenpaw package unavailable component=update step=apply_channel_access_flags action=skip")
            return

        agent_config = load_agent_config(DEFAULT_AGENT_ID)
        if getattr(agent_config, "channels", None) is None:
            try:
                from qwenpaw.config.config import ChannelConfig
            except ImportError:
                return
            agent_config.channels = ChannelConfig()
        matrix_config = getattr(agent_config.channels, "matrix", None)
        if matrix_config is None:
            return
        matrix_config.access_control_group = group_enabled
        matrix_config.access_control_dm = dm_enabled
        save_agent_config(DEFAULT_AGENT_ID, agent_config)

    def _write_matrix_access_control(self, whitelist: List[str], blacklist: List[str]) -> None:
        try:
            from qwenpaw.app.channels.access_control import get_access_control_store
        except ImportError:
            self._write_matrix_access_control_json(whitelist, blacklist)
            return

        store = get_access_control_store(self.config.default_workspace_dir)
        store.set_whitelist("matrix", whitelist)
        store.set_blacklist("matrix", blacklist)

    def _write_matrix_access_control_json(self, whitelist: List[str], blacklist: List[str]) -> None:
        path = self.config.default_workspace_dir / "access_control.json"
        existing: Dict[str, Any] = {}
        if path.exists():
            try:
                loaded = json.loads(path.read_text(encoding="utf-8"))
                existing = loaded if isinstance(loaded, dict) else {}
            except json.JSONDecodeError:
                existing = {}
        matrix = _section(existing, "matrix")
        pending = matrix.get("pending")
        existing["matrix"] = {
            "whitelist": {value: "" for value in whitelist},
            "blacklist": {value: "" for value in blacklist},
            "pending": pending if isinstance(pending, list) else [],
        }
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(existing, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")

    def _dedupe(self, values: List[str]) -> List[str]:
        result = []
        seen = set()
        for value in values:
            if value not in seen:
                result.append(value)
                seen.add(value)
        return result
