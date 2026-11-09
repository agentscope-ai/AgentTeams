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
python3 -m pytest plugins/tests/teamharness/test_pull_project.py -q
python3 -m pytest plugins/tests/teamharness/test_signal_emission.py -q
python3 -m pytest plugins/tests/teamharness/test_transition_validation.py -q
python3 -m pytest plugins/tests/teamharness/mcp/test_continuation.py -q
PYTHONPATH="${REPO_ROOT}/manager/src/agentteams_manager${PYTHONPATH:+:${PYTHONPATH}}" \
  python3 -m pytest manager/src/agentteams_manager/agentteams_manager/tests/test_taskflow_tool.py -q
ruby plugins/tests/teamharness/mcp/test-server.rb
ruby plugins/tests/teamharness/mcp/tools/test-message.rb
ruby plugins/tests/teamharness/mcp/tools/test-filesync.rb
ruby plugins/tests/teamharness/mcp/tools/test-projectflow.rb
ruby plugins/tests/teamharness/mcp/tools/test-taskflow.rb
ruby plugins/tests/teamharness/mcp/tools/test-transition-table.rb
python3 -m pytest plugins/tests/agentteams-matrix-channel/test_session_isolation.py -q
python3 -m pytest plugins/tests/agentteams-matrix-channel/test_fake_completion_marker.py -q
python3 -m pytest plugins/tests/agentteams-matrix-channel/test_consume_error.py -q

python3 -m pytest plugins/tests/agentteams-matrix-channel/test_show_thinking_gate.py -q
python3 -m pytest plugins/tests/agentteams-matrix-channel/test_send_gate_retrigger_placeholder.py -q
python3 -m pytest plugins/tests/agentteams-matrix-channel/test_send_gate_retrigger_relevance.py -q

# Runtime adapter bundles (P0): agentteams-matrix + agentteams-teamharness
bash -n plugins/agentteams-matrix/scripts/install.sh
bash -n plugins/agentteams-matrix/scripts/uninstall.sh
bash -n plugins/agentteams-matrix/adapters/qwenpaw/install.sh
bash -n plugins/agentteams-matrix/adapters/qwenpaw/uninstall.sh
bash -n plugins/agentteams-teamharness/scripts/install.sh
bash -n plugins/agentteams-teamharness/scripts/uninstall.sh
bash -n plugins/agentteams-teamharness/adapters/qwenpaw/install.sh
bash -n plugins/agentteams-teamharness/adapters/qwenpaw/uninstall.sh
ruby plugins/scripts/validate-plugin.rb plugins/agentteams-teamharness/plugin.yaml
OUT_DIR="$(mktemp -d)" ruby plugins/scripts/package-plugin.rb plugins/agentteams-teamharness/plugin.yaml >/dev/null
python3 -m pytest plugins/tests/agentteams-matrix/golden -q
python3 -m pytest plugins/tests/agentteams-teamharness/golden -q
