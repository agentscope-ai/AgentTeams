#!/usr/bin/env bash
# Committed regression for the legacy CoPaw upgrade gate (issue #1310 / PR #1335).
#
# Stub-driven (fake kubectl/helm on PATH): exercises the gate's decision
# table and its fail-closed error paths (RBAC Forbidden, API down) without
# a cluster. Covers BOTH gate implementations:
#   - tests/copaw-helm-upgrade-gate.sh          (standalone operator pre-check)
#   - helm/agentteams/files/copaw-gate-check.sh (in-chart pre-upgrade hook)
# The chart-side hook RENDERING (annotations, RBAC, env wiring) is asserted
# by tests/check-helm-agentteams.sh, which needs real helm.
#
# The "live" fixture is the actual old-chart default resolution
# (higress-registry.../agentteams-copaw-worker + the install-time global
# tag), the same value test-29 and the Go recreation fixture pin — so the
# "old-default install -> new-chart upgrade without a pin" case below is
# the real regression the gate exists for.

set -u

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STANDALONE_GATE="${ROOT_DIR}/tests/copaw-helm-upgrade-gate.sh"
HOOK_SCRIPT="${ROOT_DIR}/helm/agentteams/files/copaw-gate-check.sh"
LEGACY="higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker:v1.2.4"

[ -f "${STANDALONE_GATE}" ] || { echo "FAIL: missing ${STANDALONE_GATE}"; exit 1; }
[ -f "${HOOK_SCRIPT}" ] || { echo "FAIL: missing ${HOOK_SCRIPT}"; exit 1; }

bash -n "${STANDALONE_GATE}" || { echo "FAIL: bash -n ${STANDALONE_GATE}"; exit 1; }
sh -n "${HOOK_SCRIPT}" || { echo "FAIL: sh -n ${HOOK_SCRIPT}"; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
BIN="${TMP}/bin"
mkdir -p "${BIN}"

export STUB_MODE_FILE="${TMP}/mode"
export FAKE_DEPLOY="${TMP}/fake-deploy"
export FAKE_IMAGE="${TMP}/fake-image"
export FAKE_RENDER="${TMP}/fake-render"
export FAKE_RENDER_FAIL="${TMP}/fake-render-fail"

# Stub kubectl. Behavior is selected by $STUB_MODE_FILE:
#   ok               (default) — serve the FAKE_DEPLOY/FAKE_IMAGE fixtures
#   forbidden-deploy — first kubectl call (deployment list) returns Forbidden
#   forbidden-env    — env read on the deployment returns Forbidden
#   api-down         — every call fails like a dead API server
cat > "${BIN}/kubectl" <<'EOF'
#!/usr/bin/env bash
mode="$(cat "${STUB_MODE_FILE}" 2>/dev/null || true)"
[ -n "${mode}" ] || mode=ok
args="$*"
case "${mode}" in
    api-down)
        echo "The connection to the server localhost:8080 was refused - did you specify the right host or port?" >&2
        exit 1
        ;;
esac
if [[ "${args}" == *"items[0].metadata.name"* || "${args}" == *"items[*].metadata.name"* ]]; then
    if [ "${mode}" = "forbidden-deploy" ]; then
        echo 'Error from server (Forbidden): deployments.apps is forbidden: User "system:serviceaccount:testns:gate" cannot list resource "deployments" in api group "apps" in namespace "testns"' >&2
        exit 1
    fi
    [ -f "${FAKE_DEPLOY}" ] && cat "${FAKE_DEPLOY}"
    exit 0
fi
if [[ "${args}" == *"AGENTTEAMS_COPAW_WORKER_IMAGE"* ]]; then
    if [ "${mode}" = "forbidden-env" ]; then
        echo 'Error from server (Forbidden): deployments.apps "agentteams-controller" is forbidden: User "system:serviceaccount:testns:gate" cannot get resource "deployments" in api group "apps" in namespace "testns"' >&2
        exit 1
    fi
    [ -f "${FAKE_IMAGE}" ] && cat "${FAKE_IMAGE}"
    exit 0
fi
exit 0
EOF
chmod +x "${BIN}/kubectl"

# Stub helm: serves the FAKE_RENDER fixture, or fails when FAKE_RENDER_FAIL exists.
cat > "${BIN}/helm" <<'EOF'
#!/usr/bin/env bash
if [ -f "${FAKE_RENDER_FAIL}" ]; then
    echo "Error: worker.defaultImage.copaw.tag is required for legacy CoPaw deployments" >&2
    exit 1
fi
[ -f "${FAKE_RENDER}" ] && cat "${FAKE_RENDER}"
exit 0
EOF
chmod +x "${BIN}/helm"

VALS="${TMP}/values.yaml"
printf 'worker:\n  defaultImage:\n' > "${VALS}"

PASS=0
FAIL=0
OUT=""
RC=0

check() { # name want_rc want_substring
    local name="$1" want_rc="$2" want_str="$3"
    if [ "${RC}" = "${want_rc}" ] && { [ -z "${want_str}" ] || grep -qF -- "${want_str}" <<<"${OUT}"; }; then
        echo "PASS: ${name} (rc=${RC})"
        PASS=$((PASS + 1))
    else
        echo "FAIL: ${name} (want rc=${want_rc} str='${want_str}', got rc=${RC})"
        echo "---- output ----"
        echo "${OUT}"
        echo "----------------"
        FAIL=$((FAIL + 1))
    fi
}

set_mode() { printf '%s' "$1" > "${STUB_MODE_FILE}"; }
reset_fixtures() { rm -f "${FAKE_DEPLOY}" "${FAKE_IMAGE}" "${FAKE_RENDER}" "${FAKE_RENDER_FAIL}"; }

run_gate() {
    OUT="$(PATH="${BIN}:${PATH}" bash "${STANDALONE_GATE}" "$@" 2>&1)"
    RC=$?
}

run_hook() { # new_image force
    OUT="$(PATH="${BIN}:${PATH}" GATE_NAMESPACE=testns GATE_NEW_COPAW_IMAGE="$1" GATE_FORCE="$2" sh "${HOOK_SCRIPT}" 2>&1)"
    RC=$?
}

# ── Standalone gate: decision table ───────────────────────────────────────

reset_fixtures; set_mode ok
run_gate --namespace testns
check "standalone: no deployment -> OK" 0 "no agentteams controller deployment"

printf 'agentteams-controller' > "${FAKE_DEPLOY}"
run_gate --namespace testns
check "standalone: deployment without legacy env -> OK" 0 "does not carry AGENTTEAMS_COPAW_WORKER_IMAGE"

printf '%s' "${LEGACY}" > "${FAKE_IMAGE}"
run_gate --namespace testns
check "standalone: live legacy image, no values -> FAIL (real old-default upgrade)" 1 "cannot preserve the resolved legacy image"
check "standalone: pin hint (repository)" 1 "repository: \"higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-copaw-worker\""
check "standalone: pin hint (tag)" 1 "tag: \"v1.2.4\""

cat > "${FAKE_RENDER}" <<EOF
            - name: AGENTTEAMS_COPAW_WORKER_IMAGE
              value: "${LEGACY}"
EOF
run_gate --namespace testns --values "${VALS}"
check "standalone: values pin preserves the resolved image -> OK" 0 "safe to upgrade"

rm -f "${FAKE_RENDER}"
run_gate --namespace testns --values "${VALS}"
check "standalone: default values drop the env -> FAIL" 1 "drops AGENTTEAMS_COPAW_WORKER_IMAGE"

printf '            - name: AGENTTEAMS_COPAW_WORKER_IMAGE\n              value: "other.registry/agentteams-copaw-worker:9.9.9"\n' > "${FAKE_RENDER}"
run_gate --namespace testns --values "${VALS}"
check "standalone: values change the image -> WARN (exit 0)" 0 "WARN"

rm -f "${FAKE_RENDER}"
: > "${FAKE_RENDER_FAIL}"
run_gate --namespace testns --values "${VALS}"
check "standalone: upgrade values do not render -> exit 2" 2 "do not render against this chart"
rm -f "${FAKE_RENDER_FAIL}"

run_gate --values "${VALS}"
check "standalone: missing --namespace -> exit 2" 2 "--namespace is required"

# ── Standalone gate: fail-closed error paths (RBAC / API) ─────────────────
# kubectl rc != 0 must NEVER be read as "no deployment / no env" (exit 0).

reset_fixtures
printf 'agentteams-controller' > "${FAKE_DEPLOY}"
set_mode forbidden-deploy
run_gate --namespace testns
check "standalone: deployment list Forbidden -> exit 2 (no fail-open)" 2 "failed to read deployments"
check "standalone: Forbidden error is surfaced" 2 "Error from server (Forbidden)"

set_mode forbidden-env
run_gate --namespace testns
check "standalone: env read Forbidden -> exit 2 (no fail-open)" 2 "failed to read the controller env"

set_mode api-down
run_gate --namespace testns
check "standalone: API down -> exit 2 (no fail-open)" 2 "failed to read deployments"

# ── Pre-upgrade hook script: decision table ───────────────────────────────

reset_fixtures; set_mode ok
run_hook "" "false"
check "hook: no deployment -> OK" 0 "no agentteams controller deployment"

printf 'agentteams-controller' > "${FAKE_DEPLOY}"
run_hook "" "false"
check "hook: deployment without legacy env -> OK" 0 "no live controller deployment carries"

printf '%s' "${LEGACY}" > "${FAKE_IMAGE}"
run_hook "" "false"
check "hook: live legacy image, upgrade renders nothing -> FAIL (real old-default upgrade)" 1 "drops AGENTTEAMS_COPAW_WORKER_IMAGE"
check "hook: FAIL names the live image" 1 "${LEGACY}"

run_hook "" "true"
check "hook: GATE_FORCE=true acks the drop -> exit 0" 0 "GATE_FORCE=true"

run_hook "${LEGACY}" "false"
check "hook: upgrade preserves the resolved image -> OK" 0 "preserved"

run_hook "other.registry/agentteams-copaw-worker:9.9.9" "false"
check "hook: upgrade changes the image -> WARN (exit 0)" 0 "WARN"

# ── Pre-upgrade hook script: fail-closed error paths (RBAC / API) ─────────

set_mode forbidden-deploy
run_hook "" "false"
check "hook: deployment list Forbidden -> exit 2 (no fail-open)" 2 "FAIL-CLOSED"

set_mode forbidden-env
run_hook "" "false"
check "hook: env read Forbidden -> exit 2 (no fail-open)" 2 "FAIL-CLOSED"

set_mode api-down
run_hook "" "false"
check "hook: API down -> exit 2 (no fail-open)" 2 "FAIL-CLOSED"

echo ""
echo "RESULT: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
