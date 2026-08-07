"""
统一 MCP 接入层（Mock）

对应方案第六章『统一 MCP 接入层』。
所有外部系统调用都收敛到 call(system_id, action, **params) 这一个入口，
Mock 与真实 MCP Server 共用同一套请求/响应 Schema，复赛替换实现即可。

关键设计：
  - 系统连通性由 systems.json 的 connectivity 字段决定（mcp_direct / no_api）
  - no_api 系统调用会返回 NOT_CONNECTABLE，触发 Legacy 人工执行路径
  - 每次调用都返回 before/after 状态，满足『执行前后状态留痕』的证据要求
"""

from __future__ import annotations

import copy
import json
from typing import Any, Dict, List, Optional

from core.models import now_iso


class McpError(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


class MockMcpGateway:
    def __init__(self, systems_file: str):
        with open(systems_file, "r", encoding="utf-8") as f:
            data = json.load(f)
        self.systems: Dict[str, Dict[str, Any]] = {s["system_id"]: s for s in data["systems"]}
        self.account_states: Dict[str, Dict[str, Any]] = data["account_states"]
        self.call_log: List[Dict[str, Any]] = []

    # ---------------- 系统元数据 ----------------

    def get_system(self, system_id: str) -> Dict[str, Any]:
        sys_meta = self.systems.get(system_id)
        if not sys_meta:
            raise McpError("SYSTEM_NOT_FOUND", f"未登记的系统: {system_id}")
        return sys_meta

    def connectivity(self, system_id: str) -> str:
        try:
            return self.get_system(system_id)["connectivity"]
        except McpError:
            return "unknown"

    def is_auto_executable(self, system_id: str, action: str) -> bool:
        try:
            return action in self.get_system(system_id).get("auto_executable_actions", [])
        except McpError:
            return False

    # ---------------- 统一调用入口 ----------------

    def call(self, system_id: str, action: str, user_id: str = "", **params) -> Dict[str, Any]:
        sys_meta = self.get_system(system_id)

        if sys_meta["connectivity"] == "no_api":
            resp = {
                "ok": False,
                "code": "NOT_CONNECTABLE",
                "message": f"{sys_meta['name']} 无开放接口，无法通过 MCP 直连，需转人工执行",
                "manual_operator": sys_meta.get("manual_operator", "系统管理员"),
                "at": now_iso(),
            }
            self._log(system_id, action, user_id, params, resp)
            return resp

        if action not in sys_meta.get("supported_actions", []):
            resp = {"ok": False, "code": "ACTION_UNSUPPORTED",
                    "message": f"{sys_meta['name']} 不支持动作 {action}", "at": now_iso()}
            self._log(system_id, action, user_id, params, resp)
            return resp

        handler = getattr(self, f"_do_{action}", None)
        if handler is None:
            resp = {"ok": False, "code": "NOT_IMPLEMENTED",
                    "message": f"Mock 未实现动作 {action}", "at": now_iso()}
        else:
            before = copy.deepcopy(self._state(user_id, system_id))
            resp = handler(system_id, user_id, params)
            after = copy.deepcopy(self._state(user_id, system_id))
            resp.setdefault("before", before)
            resp.setdefault("after", after)
            resp.setdefault("at", now_iso())

        self._log(system_id, action, user_id, params, resp)
        return resp

    # ---------------- 状态存取 ----------------

    def _state(self, user_id: str, system_id: str) -> Dict[str, Any]:
        return self.account_states.get(user_id, {}).get(system_id, {})

    def _mutate(self, user_id: str, system_id: str, patch: Dict[str, Any]) -> Dict[str, Any]:
        self.account_states.setdefault(user_id, {}).setdefault(system_id, {}).update(patch)
        return self.account_states[user_id][system_id]

    def _log(self, system_id: str, action: str, user_id: str,
             params: Dict[str, Any], resp: Dict[str, Any]) -> None:
        self.call_log.append({
            "at": now_iso(), "system_id": system_id, "action": action,
            "user_id": user_id, "params": params,
            "ok": resp.get("ok"), "code": resp.get("code", "OK"),
        })

    # ---------------- 具体动作实现 ----------------

    def _do_query_account_status(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        if not st:
            return {"ok": False, "code": "ACCOUNT_NOT_FOUND",
                    "message": f"在 {system_id} 未查询到该用户状态"}
        return {"ok": True, "code": "OK", "status": st.get("status", "unknown"), "detail": st}

    def _do_query_login_log(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        logs = st.get("login_log", [])
        suspicious = [l for l in logs if "境外" in str(l.get("geo", ""))]
        return {"ok": True, "code": "OK", "total": len(logs), "logs": logs,
                "suspicious_count": len(suspicious)}

    def _do_query_sso_log(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        return {"ok": True, "code": "OK", "logs": st.get("sso_log", [])}

    def _do_list_permissions(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        perms = st.get("permissions", [])
        sensitive = [x for x in perms if any(k in x for k in ("write", "maintainer", "payment", "token"))]
        return {"ok": True, "code": "OK", "permissions": perms,
                "sensitive": sensitive, "sensitive_count": len(sensitive)}

    def _do_unlock_account(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        if st.get("status") != "locked":
            return {"ok": True, "code": "NOOP", "message": "账号当前非锁定状态，无需解锁"}
        self._mutate(user_id, system_id, {
            "status": "active", "lock_reason": None, "failed_attempts": 0,
            "unlocked_at": now_iso(),
        })
        return {"ok": True, "code": "OK", "message": "账号已解锁，失败计数器已清零"}

    def _do_unbind_mfa(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        old_device = st.get("mfa_device", "unknown")
        self._mutate(user_id, system_id, {
            "mfa_bound": False, "mfa_device": None,
            "mfa_unbound_at": now_iso(), "previous_device": old_device,
        })
        return {"ok": True, "code": "OK", "message": f"已解绑原 MFA 设备 {old_device}"}

    def _do_issue_binding_link(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        link = f"https://idaas.internal/bind/{user_id}?t={now_iso()[-8:].replace(':', '')}"
        self._mutate(user_id, system_id, {"pending_binding_link": link, "link_ttl_minutes": 15})
        return {"ok": True, "code": "OK", "binding_link": link, "ttl_minutes": 15,
                "message": "已生成一次性绑定链接，15 分钟内有效"}

    def _do_revoke_sessions(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        n = st.get("active_sessions", 0)
        self._mutate(user_id, system_id, {
            "active_sessions": 0, "sessions_revoked_at": now_iso(), "suspicious_login": False,
        })
        return {"ok": True, "code": "OK", "revoked": n,
                "message": f"已吊销 {n} 个活跃会话（含境外会话）"}

    def _do_force_password_reset(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        self._mutate(user_id, system_id, {
            "must_change_password": True, "password_reset_at": now_iso(),
        })
        return {"ok": True, "code": "OK", "message": "已标记强制改密，用户下次登录必须修改密码"}

    def _do_reset_password(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        self._mutate(user_id, system_id, {"password_reset_at": now_iso(), "status": "active"})
        return {"ok": True, "code": "OK", "message": "密码已重置为一次性口令并通过安全渠道下发"}

    def _do_disable_account(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        self._mutate(user_id, system_id, {"status": "disabled", "disabled_at": now_iso()})
        return {"ok": True, "code": "OK", "message": "账号已停用"}

    def _do_revoke_tokens(self, system_id: str, user_id: str, p: Dict[str, Any]) -> Dict[str, Any]:
        st = self._state(user_id, system_id)
        perms = [x for x in st.get("permissions", []) if not x.startswith("token:")]
        self._mutate(user_id, system_id, {"permissions": perms, "tokens_revoked_at": now_iso()})
        return {"ok": True, "code": "OK", "message": "已吊销全部长期 token"}

    # ---------------- 验证探针 ----------------

    def probe(self, system_id: str, user_id: str, expect: Dict[str, Any]) -> Dict[str, Any]:
        """恢复验证探针。对 no_api 系统返回需人工确认。"""
        sys_meta = self.systems.get(system_id, {})
        if sys_meta.get("connectivity") == "no_api":
            return {"system_id": system_id, "probe": "manual_confirm_required", "passed": None,
                    "message": f"{sys_meta.get('name', system_id)} 无接口，需用户在 IM 确认"}

        st = self._state(user_id, system_id)
        checks, passed = [], True
        for key, want in expect.items():
            got = st.get(key)
            ok = (got == want)
            checks.append({"field": key, "expected": want, "actual": got, "passed": ok})
            passed = passed and ok
        return {"system_id": system_id, "probe": "state_check", "passed": passed, "checks": checks}
