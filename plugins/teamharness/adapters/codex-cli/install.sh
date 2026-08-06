#!/usr/bin/env bash
set -euo pipefail

if ! command -v codex >/dev/null 2>&1; then
  echo "ERROR: codex command not found" >&2
  exit 1
fi

python_command=""
for candidate in python3 python; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c "import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)" >/dev/null 2>&1; then
    python_command="$candidate"
    break
  fi
done
if [ -z "$python_command" ]; then
  echo "ERROR: Python 3.11 or newer not found" >&2
  exit 1
fi

project_dir="${AGENTTEAMS_PROJECT_DIR:-${PWD}}"
state_dir="${project_dir}/.agentteams/codex-cli"
mkdir -p "$state_dir"

"$python_command" - "$state_dir/adapter.json" "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
path.write_text(
    json.dumps(
        {
            "adapter": "codex-cli",
            "pluginDir": sys.argv[2],
            "credentialsStored": False,
        },
        indent=2,
        sort_keys=True,
    )
    + "\n",
    encoding="utf-8",
)
PY

log_file="${TEAMHARNESS_INSTALL_LOG:-}"
if [ -n "$log_file" ]; then
  mkdir -p "$(dirname "$log_file")"
  printf '{"event":"install","runtime":"codex-cli","pluginDir":"%s"}\n' "${AGENTTEAMS_PLUGIN_DIR:-${PWD}}" >> "$log_file"
fi
