"""Authorization and policy helpers for Matrix human-in-the-loop decisions."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any


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


@dataclass(frozen=True)
class MatrixDecision:
    """One structured decision parsed from a Matrix reply."""

    action: str
    index: int | None
    reason: str | None = None
    edited_arguments: dict[str, Any] | None = None


def parse_matrix_decision(body: str) -> MatrixDecision:
    """Parse a Human's compact Matrix approval reply."""
    parts = body.strip().split(maxsplit=2)
    if len(parts) < 2:
        raise ValueError("unsupported approval decision")
    action = parts[0].casefold()
    if action == "approve" and len(parts) == 2 and parts[1].casefold() == "all":
        return MatrixDecision(action="approve_all", index=None)
    index = _positive_action_index(parts[1])
    if action == "approve" and len(parts) == 2:
        return MatrixDecision(action="approve", index=index)
    if action == "reject" and len(parts) == 3 and parts[2].strip():
        return MatrixDecision(action="reject", index=index, reason=parts[2].strip())
    if action == "edit" and len(parts) == 3:
        try:
            arguments = json.loads(parts[2])
        except json.JSONDecodeError as exc:
            raise ValueError("edited arguments must be valid JSON") from exc
        if not isinstance(arguments, dict):
            raise ValueError("edited arguments must be a JSON object")
        return MatrixDecision(action="edit", index=index, edited_arguments=arguments)
    raise ValueError("unsupported approval decision")


def _positive_action_index(value: str) -> int:
    try:
        index = int(value)
    except ValueError as exc:
        raise ValueError("approval action index must be an integer") from exc
    if index < 1:
        raise ValueError("approval action index must be positive")
    return index
