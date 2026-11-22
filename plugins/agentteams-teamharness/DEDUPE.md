# Shared TeamHarness source

`plugins/teamharness/` owns the Task protocol's prompts, skills, contracts and
MCP implementation. This bundle references those trees using relative symlinks.
Its QwenPaw entry point and Task tracing module reference the same implementation
used in the built-in QwenPaw plugin.

The bundle still owns its package metadata and install adapters. The portable
tarball and QwenPaw ZIP materialize the linked files into their own contents so
they can be installed without a repository checkout. The QwenPaw plugin ID
remains `teamharness`.
