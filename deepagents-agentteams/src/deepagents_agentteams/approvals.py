"""Authorization and policy helpers for Matrix human-in-the-loop decisions."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class ApprovalPrincipals:
    """Human Matrix identities allowed to decide pending actions."""

    requester: str
    team_admins: frozenset[str]
    coordinators: frozenset[str]

    def can_decide(self, *, sender: str, identity_kind: str) -> bool:
        """Return whether a sender is an approved human principal."""
        if identity_kind.casefold() != "human":
            return False
        return sender == self.requester or sender in self.team_admins or sender in self.coordinators


@dataclass(frozen=True)
class MCPApprovalRule:
    """Approval override for one exact MCP server/tool pair."""

    server: str
    tool: str
    mode: str


@dataclass(frozen=True)
class ToolApprovalPolicy:
    """Effective approval requirements for DeepAgents tools."""

    file_writes: str
    mcp_default: str
    mcp_rules: tuple[MCPApprovalRule, ...]

    def requires_approval(
        self,
        *,
        tool_name: str,
        mcp_server: str | None = None,
        mcp_tool: str | None = None,
    ) -> bool:
        """Return whether a tool call must interrupt for human review."""
        if tool_name == "execute":
            return True
        if tool_name in {"write_file", "edit_file", "delete"}:
            return self.file_writes == "required"
        if tool_name == "mcp":
            for rule in self.mcp_rules:
                if rule.server == mcp_server and rule.tool == mcp_tool:
                    return rule.mode == "required"
            return self.mcp_default == "required"
        return False
