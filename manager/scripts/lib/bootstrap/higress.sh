#!/bin/bash
# bootstrap/higress.sh - Higress Console initialization (embedded/docker only)

bootstrap_init_higress() {
    _HIGRESS_CONSOLE_URL=""
    _HIGRESS_USER="${AGENTTEAMS_ADMIN_USER}"
    _HIGRESS_PASS="${AGENTTEAMS_ADMIN_PASSWORD}"
    if [ "${AGENTTEAMS_RUNTIME}" = "k8s" ]; then
        log "K8s mode: skipping Higress initialization (handled by controller)"
    elif [ "${AGENTTEAMS_RUNTIME}" != "aliyun" ]; then
        _HIGRESS_CONSOLE_URL="http://127.0.0.1:8001"
    fi

    if [ -z "${_HIGRESS_CONSOLE_URL}" ]; then
        return 0
    fi

    local COOKIE_FILE="/tmp/higress-session-cookie"

    log "Waiting for Higress Console (${_HIGRESS_CONSOLE_URL}) to be fully ready and initializing admin..."
    local INIT_DONE=false
    local i INIT_RESULT HTTP_CODE LOGIN_OK VERIFY_CODE VERIFY2
    for i in $(seq 1 90); do
        INIT_RESULT=$(curl -s -X POST "${_HIGRESS_CONSOLE_URL}/system/init" \
            -H 'Content-Type: application/json' \
            -d '{"adminUser":{"name":"'"${_HIGRESS_USER}"'","password":"'"${_HIGRESS_PASS}"'","displayName":"'"${_HIGRESS_USER}"'"}}' 2>/dev/null) || true
        if echo "${INIT_RESULT}" | grep -qE '"success":true|already.?init' 2>/dev/null; then
            INIT_DONE=true
            break
        fi
        if echo "${INIT_RESULT}" | grep -q '"name"' 2>/dev/null; then
            INIT_DONE=true
            break
        fi
        sleep 2
    done

    if [ "${INIT_DONE}" != "true" ]; then
        log "ERROR: Higress Console did not become ready within 180s"
        exit 1
    fi
    log "Higress Console init done"

    log "Logging into Higress Console..."
    LOGIN_OK=false
    for i in $(seq 1 10); do
        HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${_HIGRESS_CONSOLE_URL}/session/login" \
            -H 'Content-Type: application/json' \
            -c "${COOKIE_FILE}" \
            -d '{"username":"'"${_HIGRESS_USER}"'","password":"'"${_HIGRESS_PASS}"'"}' 2>/dev/null) || true
        if { [ "${HTTP_CODE}" = "200" ] || [ "${HTTP_CODE}" = "201" ]; } && [ -f "${COOKIE_FILE}" ] && [ -s "${COOKIE_FILE}" ]; then
            LOGIN_OK=true
            break
        fi
        log "Login attempt $i (HTTP ${HTTP_CODE}), retrying in 3s..."
        sleep 3
    done

    if [ "${LOGIN_OK}" != "true" ]; then
        log "ERROR: Could not login to Higress Console after retries"
        exit 1
    fi
    log "Higress Console login successful"

    VERIFY_CODE=$(curl -s -o /dev/null -w '%{http_code}' "${_HIGRESS_CONSOLE_URL}/v1/consumers" -b "${COOKIE_FILE}" 2>/dev/null) || true
    if [ "${VERIFY_CODE}" = "200" ]; then
        log "Console session verified (cookie valid)"
    else
        log "WARNING: Console session may be invalid (verify returned HTTP ${VERIFY_CODE})"
        rm -f "${COOKIE_FILE}"
        for i in $(seq 1 5); do
            curl -s -o /dev/null -w '%{http_code}' -X POST "${_HIGRESS_CONSOLE_URL}/session/login" \
                -H 'Content-Type: application/json' \
                -c "${COOKIE_FILE}" \
                -d '{"username":"'"${_HIGRESS_USER}"'","password":"'"${_HIGRESS_PASS}"'"}' 2>/dev/null
            VERIFY2=$(curl -s -o /dev/null -w '%{http_code}' "${_HIGRESS_CONSOLE_URL}/v1/consumers" -b "${COOKIE_FILE}" 2>/dev/null) || true
            if [ "${VERIFY2}" = "200" ]; then
                log "Re-login successful, session verified"
                break
            fi
            sleep 2
        done
    fi

    export HIGRESS_COOKIE_FILE="${COOKIE_FILE}"
    export HIGRESS_CONSOLE_URL="${_HIGRESS_CONSOLE_URL}"

    /opt/hiclaw/scripts/init/setup-higress.sh
}
