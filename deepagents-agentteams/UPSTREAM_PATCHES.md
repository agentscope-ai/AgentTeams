# DeepAgents Upstream Patch Ledger

Upstream source is imported with Git subtree from
`langchain-ai/deepagents`, tag `deepagents==0.7.3`, commit
`f60951e3f5e4d7e57d6379a4bbc64259bdfdb884`.

No AgentTeams-specific upstream patches are currently required. Any future
patch under `../deepagents/` must record the affected upstream API, why the
adapter boundary is insufficient, its regression test, and the upstream issue
or pull request used to retire the patch.
