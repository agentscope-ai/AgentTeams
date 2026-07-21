#!/bin/bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "${ROOT_DIR}/shared/lib/agentteams-paths.sh"

tmp_dir=$(mktemp -d)
trap 'rm -rf "${tmp_dir}"' EXIT

legacy="${tmp_dir}/legacy"
current="${tmp_dir}/current"

mkdir -p "${legacy}"
echo preserved > "${legacy}/state"
agentteams_migrate_legacy_path "${legacy}" "${current}"
[ "$(cat "${current}/state")" = preserved ]
[ -L "${legacy}" ]

rm -rf "${legacy}" "${current}"
mkdir -p "${current}"
echo canonical > "${current}/state"
agentteams_migrate_legacy_path "${legacy}" "${current}"
[ "$(cat "${legacy}/state")" = canonical ]
[ -L "${legacy}" ]

rm -rf "${legacy}" "${current}"
mkdir -p "${legacy}" "${current}"
echo mounted > "${legacy}/state"
agentteams_migrate_legacy_path "${legacy}" "${current}"
[ "$(cat "${current}/state")" = mounted ]
[ -L "${legacy}" ]

rm -rf "${legacy}" "${current}"
mkdir -p "${legacy}" "${current}"
echo bind-mounted > "${legacy}/state"
mv() { return 1; }
agentteams_migrate_legacy_path "${legacy}" "${current}"
unset -f mv
[ "$(cat "${current}/state")" = bind-mounted ]
[ -L "${current}" ]
[ ! -L "${legacy}" ]

echo "PASS: AgentTeams legacy path migration"
