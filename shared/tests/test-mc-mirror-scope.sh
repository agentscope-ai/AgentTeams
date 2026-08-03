#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

test_controller_scope_never_mirrors_worker_data() {
    local tmpdir prefix local_root
    tmpdir="$(mktemp -d)"
    prefix="agentteams/agentteams-storage"
    local_root="${tmpdir}/agentteams-fs"
    mkdir -p "${tmpdir}/bin" "${local_root}"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${FAKE_MC_LOG}"
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_MC_LOG="${tmpdir}/mc.log"
    export PATH="${tmpdir}/bin:${PATH}"

    # shellcheck source=../lib/mc-mirror-scope.sh
    source "${REPO_ROOT}/shared/lib/mc-mirror-scope.sh"
    agentteams_mirror_initial controller "${prefix}" "${local_root}"
    agentteams_mirror_fallback controller "${prefix}" "${local_root}"

    [ "$(wc -l < "${FAKE_MC_LOG}" | tr -d ' ')" = "1" ] ||
        fail "controller scope performed an unexpected fallback mirror"
    grep -Fq \
        "mirror ${prefix}/agentteams-config/ ${local_root}/agentteams-config/ --overwrite" \
        "${FAKE_MC_LOG}" ||
        fail "controller initial sync was not limited to agentteams-config"
    if grep -Fq "mirror ${prefix}/ ${local_root}/" "${FAKE_MC_LOG}"; then
        fail "controller scope mirrored the storage root"
    fi

    rm -rf "${tmpdir}"
}

test_full_scope_keeps_legacy_manager_mirror() {
    local tmpdir prefix local_root
    tmpdir="$(mktemp -d)"
    prefix="agentteams/agentteams-storage"
    local_root="${tmpdir}/agentteams-fs"
    mkdir -p "${tmpdir}/bin" "${local_root}"

    cat > "${tmpdir}/bin/mc" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "${FAKE_MC_LOG}"
EOF
    chmod +x "${tmpdir}/bin/mc"

    export FAKE_MC_LOG="${tmpdir}/mc.log"
    export PATH="${tmpdir}/bin:${PATH}"

    agentteams_mirror_initial full "${prefix}" "${local_root}"
    agentteams_mirror_fallback full "${prefix}" "${local_root}"

    [ "$(wc -l < "${FAKE_MC_LOG}" | tr -d ' ')" = "2" ] ||
        fail "full scope did not retain initial and fallback mirrors"
    grep -Fq "mirror ${prefix}/ ${local_root}/ --overwrite" "${FAKE_MC_LOG}" ||
        fail "full initial mirror changed"
    grep -Fq \
        "mirror ${prefix}/ ${local_root}/ --overwrite --newer-than 5m" \
        "${FAKE_MC_LOG}" ||
        fail "full fallback mirror changed"

    rm -rf "${tmpdir}"
}

test_controller_scope_never_mirrors_worker_data
test_full_scope_keeps_legacy_manager_mirror

echo "PASS: mc mirror scope"
