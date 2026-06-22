#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

cd "${REPO_ROOT}"

python3 plugins/tests/cli/test_agentteams_plugin_cli.py
ruby plugins/scripts/validate-plugin.rb plugins/teamharness/plugin.yaml
OUT_DIR="$(mktemp -d)" ruby plugins/scripts/package-plugin.rb plugins/teamharness/plugin.yaml >/dev/null
ruby plugins/tests/teamharness/test-contracts.rb
python3 -m pytest plugins/tests/teamharness/adapters/qwenpaw/test_adapter.py -q
python3 -m pytest plugins/tests/teamharness/adapters/qwenpaw/test_package.py -q
ruby plugins/tests/teamharness/mcp/test-server.rb
ruby plugins/tests/teamharness/mcp/tools/test-message.rb
ruby plugins/tests/teamharness/mcp/tools/test-filesync.rb
ruby plugins/tests/teamharness/mcp/tools/test-projectflow.rb
ruby plugins/tests/teamharness/mcp/tools/test-taskflow.rb
