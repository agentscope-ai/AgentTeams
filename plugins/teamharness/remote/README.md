# TeamHarness Remote Runtime Packages

`plugins/teamharness/remote/` contains runtime assets for remote-managed local
workers. It is intentionally separate from the runtime-neutral TeamHarness base
package used by cloud-side workers such as QwenPaw.

Current contents:

- `claude-code/`: Claude Code local management bundle source, including Claude
  plugin assets, MCP server assets, hooks, local worker loop, and LoongSuite
  runtime template for `runtime=claude-code`.

Build the Claude Code local bundle with:

```bash
ruby plugins/teamharness/remote/claude-code/scripts/build-claude-local-bundle.rb
```

The generated bundle uses the Claude Code remote-managed assets under this
package. Remote-managed runtime process code and LoongSuite integration stay
outside the runtime-neutral TeamHarness package surface.
