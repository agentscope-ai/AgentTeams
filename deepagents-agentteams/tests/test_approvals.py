import unittest

from deepagents_agentteams.approvals import ApprovalPrincipals, MCPApprovalRule, ToolApprovalPolicy


class ApprovalPrincipalTests(unittest.TestCase):
    def test_rejects_agent_identity_even_when_listed_as_coordinator(self) -> None:
        principals = ApprovalPrincipals(
            requester="@human:example.org",
            team_admins=frozenset(),
            coordinators=frozenset({"@manager:example.org"}),
        )

        allowed = principals.can_decide(
            sender="@manager:example.org",
            identity_kind="manager",
        )

        self.assertFalse(allowed)

    def test_allows_requester_admin_and_coordinator_when_they_are_human(self) -> None:
        principals = ApprovalPrincipals(
            requester="@requester:example.org",
            team_admins=frozenset({"@admin:example.org"}),
            coordinators=frozenset({"@coordinator:example.org"}),
        )

        for sender in (
            "@requester:example.org",
            "@admin:example.org",
            "@coordinator:example.org",
        ):
            with self.subTest(sender=sender):
                self.assertTrue(principals.can_decide(sender=sender, identity_kind="human"))


class ToolApprovalPolicyTests(unittest.TestCase):
    def test_execute_always_requires_approval(self) -> None:
        policy = ToolApprovalPolicy(
            file_writes="notRequired",
            mcp_default="notRequired",
            mcp_rules=(),
        )

        self.assertTrue(policy.requires_approval(tool_name="execute"))

    def test_file_write_tools_follow_file_write_policy(self) -> None:
        required = ToolApprovalPolicy(
            file_writes="required",
            mcp_default="notRequired",
            mcp_rules=(),
        )
        not_required = ToolApprovalPolicy(
            file_writes="notRequired",
            mcp_default="notRequired",
            mcp_rules=(),
        )

        for tool_name in ("write_file", "edit_file", "delete"):
            with self.subTest(tool_name=tool_name):
                self.assertTrue(required.requires_approval(tool_name=tool_name))
                self.assertFalse(not_required.requires_approval(tool_name=tool_name))

    def test_mcp_rules_match_exact_server_and_tool_then_fall_back_to_default(self) -> None:
        policy = ToolApprovalPolicy(
            file_writes="notRequired",
            mcp_default="notRequired",
            mcp_rules=(
                MCPApprovalRule(server="github", tool="create_issue", mode="required"),
            ),
        )

        self.assertTrue(
            policy.requires_approval(
                tool_name="mcp",
                mcp_server="github",
                mcp_tool="create_issue",
            )
        )
        self.assertFalse(
            policy.requires_approval(
                tool_name="mcp",
                mcp_server="gitlab",
                mcp_tool="create_issue",
            )
        )


if __name__ == "__main__":
    unittest.main()
