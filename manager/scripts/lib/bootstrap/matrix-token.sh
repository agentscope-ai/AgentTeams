#!/bin/bash
# bootstrap/matrix-token.sh - Obtain Manager Matrix access token

bootstrap_obtain_matrix_token() {
    if [ -n "${AGENTTEAMS_MANAGER_MATRIX_TOKEN:-}" ]; then
        MANAGER_TOKEN="${AGENTTEAMS_MANAGER_MATRIX_TOKEN}"
        log "Manager Matrix token pre-injected by controller (token prefix: ${MANAGER_TOKEN:0:10}...)"
        export MANAGER_TOKEN
        return 0
    fi

    if [ "${AGENTTEAMS_RUNTIME}" = "k8s" ]; then
        log "K8s mode: obtaining Manager Matrix token via password login..."
        local _LOGIN_RESPONSE
        _LOGIN_RESPONSE=$(curl -s -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
            -H 'Content-Type: application/json' \
            -d '{
                "type": "m.login.password",
                "identifier": {"type": "m.id.user", "user": "manager"},
                "password": "'"${AGENTTEAMS_MANAGER_PASSWORD}"'"
            }' 2>&1)
        MANAGER_TOKEN=$(echo "${_LOGIN_RESPONSE}" | jq -r '.access_token' 2>/dev/null)
        if [ -z "${MANAGER_TOKEN}" ] || [ "${MANAGER_TOKEN}" = "null" ]; then
            log "ERROR: Failed to obtain Manager Matrix token"
            log "ERROR: Login response was: ${_LOGIN_RESPONSE}"
            exit 1
        fi
        log "Manager Matrix token obtained (token prefix: ${MANAGER_TOKEN:0:10}...)"
        export MANAGER_TOKEN
        return 0
    fi

    log "Registering human admin Matrix account..."
    curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/register \
        -H 'Content-Type: application/json' \
        -d '{
            "username": "'"${AGENTTEAMS_ADMIN_USER}"'",
            "password": "'"${AGENTTEAMS_ADMIN_PASSWORD}"'",
            "auth": {
                "type": "m.login.registration_token",
                "token": "'"${AGENTTEAMS_REGISTRATION_TOKEN}"'"
            }
        }' > /dev/null 2>&1 || log "Admin account may already exist"

    log "Registering Manager Agent Matrix account..."
    curl -sf -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/register \
        -H 'Content-Type: application/json' \
        -d '{
            "username": "manager",
            "password": "'"${AGENTTEAMS_MANAGER_PASSWORD}"'",
            "auth": {
                "type": "m.login.registration_token",
                "token": "'"${AGENTTEAMS_REGISTRATION_TOKEN}"'"
            }
        }' > /dev/null 2>&1 || log "Manager account may already exist"

    log "Obtaining Manager Matrix access token..."
    local _LOGIN_RESPONSE _LOGIN_EXIT
    _LOGIN_RESPONSE=$(curl -s -X POST ${AGENTTEAMS_MATRIX_URL}/_matrix/client/v3/login \
        -H 'Content-Type: application/json' \
        -d '{
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": "manager"},
            "password": "'"${AGENTTEAMS_MANAGER_PASSWORD}"'"
        }' 2>&1)
    _LOGIN_EXIT=$?
    log "Matrix login HTTP exit code: ${_LOGIN_EXIT}"
    log "Matrix login response: ${_LOGIN_RESPONSE}"

    MANAGER_TOKEN=$(echo "${_LOGIN_RESPONSE}" | jq -r '.access_token' 2>/dev/null)

    if [ -z "${MANAGER_TOKEN}" ] || [ "${MANAGER_TOKEN}" = "null" ]; then
        log "ERROR: Failed to obtain Manager Matrix token (exit=${_LOGIN_EXIT})"
        log "ERROR: Login response was: ${_LOGIN_RESPONSE}"
        exit 1
    fi
    log "Manager Matrix token obtained (token prefix: ${MANAGER_TOKEN:0:10}...)"
    export MANAGER_TOKEN
}
