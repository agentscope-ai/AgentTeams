#!/bin/bash
# test29-evidence.sh - failure-evidence collection for test-29 (legacy CoPaw
# upgrade), with credential-safe forensics (#1476 round 2).
#
# Two layers, both fail-closed:
#
#   1. projection - `docker inspect` is reduced to the diagnostic fields a
#      failure report needs (state/status/exit code, restart count, image,
#      network mode, port mappings, mount names, env key names). The full
#      Config.Env is never emitted; env appears as key names with a literal
#      "***" value.
#   2. unified redaction - EVERY artifact (inspect summary, container logs,
#      workspace listing + QwenPaw config, Worker CR, MinIO listing) passes
#      through the SAME evidence_redact() before it is written to disk.
#
# Text redaction rules (evidence_redact):
#   R1  "Authorization:" header (incl. Bearer/Basic scheme) -> "***"
#   R2  free-form "Bearer <token>"                          -> "Bearer ***"
#   R3  JSON {"name": <sensitive>, "value": <secret>} pair  -> value "***"
#   R4  "key": value (JSON) with a sensitive key name       -> "***"
#   R5  NAME=value / --name value where the name contains
#       KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL (case-insensitive) -> "***"
#       Value forms consumed WHOLE: double-quoted (incl. \"
#       escape pairs), single-quoted, or a plain run - an escaped
#       quote or a single quote can therefore not split the value
#       and leave a secret tail (#1476 round 3).
#   R6  key: value (unquoted) with a sensitive key name     -> "***"
#   R7  explicit blacklist: AGENTTEAMS_LLM_API_KEY, *_TOKEN, *_SECRET,
#       *_API_KEY, *_PASSWORD (covers the spec's named variables)  -> "***"
#   R8  hyphenated sensitive name + whitespace + value (log/header/
#       CLI form, e.g. "x-auth-token <token>") -> "***"; bare prose
#       words without a hyphen are intentionally not masked
#   R9  URL userinfo "scheme://user:pass@host" -> "scheme://user:***@host"
#
# Long (>=24 char) high-entropy values under a suspected sensitive key name
# are subsumed by R3-R7: the whole value is replaced regardless of length,
# so no entropy gate can let a long secret through a short-value path.
#
# Structured JSON artifacts (Worker CR) additionally pass through
# evidence_scrub_json (jq): any object value under a sensitive key, and any
# {name, value} pair whose name is sensitive, becomes "***" (both key
# orders, any nesting). If jq is missing or the input is not JSON, the raw
# document is WITHHELD (a marker note is written) - never leaked.
#
# Nothing in this file may fail the test: every collector step is guarded,
# and the collector always returns 0.
#
# External functions the collector relies on (provided by the caller):
#   worker_container_name <worker>, exec_in_agent, exec_in_manager, log_info
# Caller-provided globals:
#   EVIDENCE_DIR            (where artifacts are written)
#   STORAGE_PREFIX          (default: agentteams/agentteams-storage)
#   EVIDENCE_COLLECTED      (once-collected guard; defaults to 0)

# ------------------------------------------------------------
# Unified text redactor: stdin -> stdout.
# GNU sed (CI ubuntu-latest, dev containers). Case-insensitive (I flag).
# ------------------------------------------------------------
evidence_redact() {
    # Each expression references ONLY groups that precede the sensitive value;
    # the (KEY|TOKEN|...) alternation is never back-referenced, otherwise sed
    # splices the matched keyword back into the output (the "KEYKEY***" bug).
    sed -E \
        -e 's/(Authorization[[:space:]]*:)[[:space:]]*(Bearer[[:space:]]+|Basic[[:space:]]+)?[^[:space:]]*/\1 ***/gI' \
        -e 's/Bearer[[:space:]]+[^[:space:]]+/Bearer ***/gI' \
        -e 's/("name"[[:space:]]*:[[:space:]]*"[^"]*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)[^"]*"[[:space:]]*,[[:space:]]*"value"[[:space:]]*:[[:space:]]*)"((\\.|[^"\\])*)"/\1"***"/gI' \
        -e 's/("[^"]*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)[^"]*"[[:space:]]*:[[:space:]]*)("[^"]*"|[^,}[:space:]]+)/\1"***"/gI' \
        -e 's/([A-Za-z0-9_.-]*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|apiKey)[A-Za-z0-9_.-]*)([[:space:]]*[:=][[:space:]]*)("[^"]*"|'\''[^'\'']*'\''|([^[:space:]",\\]|\\.)+)/\1\3***/gI' \
        -e 's/(([A-Za-z0-9_.-]*-(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)[A-Za-z0-9_.-]*|[A-Za-z0-9_.-]*(KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL)-[A-Za-z0-9_.-]*))[[:space:]]+([^[:space:]]+)/\1 ***/gI' \
        -e 's|(://[^/:[:space:]@]+:)[^@[:space:]]+[[:space:]]*@|\1***@|gI'
}

# ------------------------------------------------------------
# Inspect projection: stdin = full `docker inspect` JSON (array of one
# object) -> stdout = only the diagnostic fields, env as key names with
# "***" values. Fail-closed: on missing jq / non-JSON input, emit a marker
# note instead of the raw document (which would carry Config.Env secrets).
# ------------------------------------------------------------
evidence_inspect_summary() {
    local raw
    raw="$(cat)"
    if [ -z "${raw}" ]; then
        printf '{}\n'
        return 0
    fi
    if command -v jq >/dev/null 2>&1; then
        if printf '%s\n' "${raw}" | jq -c '
            [ .[0] | {
                id: (.Id? // ""),
                name: (.Name? // ""),
                created: (.Created? // ""),
                image: (.Config.Image? // ""),
                restartCount: (.RestartCount? // 0),
                state: (.State? // {} | {
                    status: (.Status? // null),
                    running: (.Running? // null),
                    exitCode: (.ExitCode? // null),
                    error: (.Error? // null),
                    startedAt: (.StartedAt? // null),
                    finishedAt: (.FinishedAt? // null)
                }),
                networkMode: (.HostConfig.NetworkMode? // ""),
                ports: ((.NetworkSettings.Ports? // {}) | keys),
                mounts: [ (.Mounts? // [])[] | {
                    source: (.Source? // ""),
                    destination: (.Destination? // ""),
                    mode: (.Mode? // "")
                } ],
                env: [ (.Config.Env? // [])[] | (split("=") | {name: (.[0] // ""), value: "***"}) ]
            } ]
        ' 2>/dev/null; then
            return 0
        fi
    fi
    printf '{"note":"worker-inspect summary unavailable (jq missing or input is not inspect JSON); raw output withheld to avoid leaking Config.Env"}\n'
}

# ------------------------------------------------------------
# Structured JSON scrub (Worker CR and any JSON artifact): stdin -> stdout.
# Sensitive keys (case-insensitive substring) get "***" values at any
# nesting depth; {name, value} pairs with a sensitive name get a "***"
# value in either key order. Fail-closed like evidence_inspect_summary.
# ------------------------------------------------------------
evidence_scrub_json() {
    local raw
    raw="$(cat)"
    if [ -z "${raw}" ]; then
        printf '{}\n'
        return 0
    fi
    if command -v jq >/dev/null 2>&1; then
        if printf '%s\n' "${raw}" | jq -c '
            def sens: test("key|token|secret|password|credential|bearer"; "i");
            # Redact by KEY NAME only - a key called "kind" or "runtime" is
            # diagnostic and must survive verbatim. {name,value} pairs are
            # handled in either key order, at any depth.
            def scrub:
              if type == "object" then
                (with_entries(
                   if ((.key | type) == "string") and (.key | sens)
                   then .value = "***"
                   else .value |= scrub
                   end))
                | (if (type == "object")
                      and ((.name? | type) == "string") and (.name | sens)
                      and ((.value? | type) == "string")
                   then .value = "***" else . end)
              elif type == "array" then map(scrub)
              else . end;
            scrub
        ' 2>/dev/null; then
            return 0
        fi
    fi
    printf '{"note":"JSON scrub unavailable (jq missing or input is not JSON); raw output withheld to avoid leaking credentials"}\n'
}

# ------------------------------------------------------------
# Failure evidence collector (credential-safe).
#
# Persists diagnostics for a failing Worker under ${EVIDENCE_DIR} BEFORE
# cleanup deletes the Worker/CR/storage, so CI artifacts contain evidence.
# Every step is guarded and the function always returns 0.
#
# Artifacts (all redacted before write):
#   worker-logs.txt        container logs (last 1000 lines)
#   worker-inspect.json    projected inspect (diagnostic fields only)
#   workspace-listing.txt  workspace tree + QwenPaw config file contents
#   worker-cr.json         Worker CR (structured scrub + text redaction)
#   minio-listing.txt      MinIO objects under the worker prefix
# ------------------------------------------------------------
collect_test29_evidence() {
    local worker="$1"
    local container inspect_raw inspect_rc
    local storage_prefix="${STORAGE_PREFIX:-agentteams/agentteams-storage}"
    [ "${EVIDENCE_COLLECTED:-0}" = "1" ] && return 0
    EVIDENCE_COLLECTED=1
    container="$(worker_container_name "${worker}")"

    log_info "Collecting failure evidence for ${worker} into ${EVIDENCE_DIR}"
    mkdir -p "${EVIDENCE_DIR}" 2>/dev/null || true

    # Worker container logs
    docker logs --tail 1000 "${container}" 2>&1 \
        | evidence_redact > "${EVIDENCE_DIR}/worker-logs.txt" || true

    # Worker container inspect - projected to diagnostic fields only (never
    # the full Config.Env; env appears as key names with *** values), then
    # through the same redactor. On inspect failure, record the exit code
    # but withhold the raw output.
    inspect_raw="$(docker inspect "${container}" 2>/dev/null)"
    inspect_rc=$?
    if [ "${inspect_rc}" -eq 0 ]; then
        printf '%s\n' "${inspect_raw}" \
            | evidence_inspect_summary \
            | evidence_redact > "${EVIDENCE_DIR}/worker-inspect.json" || true
    else
        printf 'docker inspect failed (exit %s); raw output withheld\n' "${inspect_rc}" \
            > "${EVIDENCE_DIR}/worker-inspect.json" || true
    fi

    # Worker config / workspace listing (QwenPaw config contents included)
    {
        printf '=== workspace listing: /root/agentteams-fs/agents/%s ===\n' "${worker}"
        docker exec "${container}" sh -c "ls -laR '/root/agentteams-fs/agents/${worker}' 2>/dev/null" 2>/dev/null | head -400 || true
        printf '\n=== .qwenpaw config files ===\n'
        docker exec "${container}" sh -c "
            for f in '/root/agentteams-fs/agents/${worker}/.qwenpaw/config.json' \
                    '/root/agentteams-fs/agents/${worker}/.qwenpaw/workspaces/default/agent.json'; do
                printf -- '--- %s ---\n' \"\$f\"
                cat \"\$f\" 2>/dev/null || printf '(missing)\n'
            done
        " 2>/dev/null || true
    } | evidence_redact > "${EVIDENCE_DIR}/workspace-listing.txt" 2>&1 || true

    # Worker CR (structured scrub, then the same text redactor)
    exec_in_agent agt get workers "${worker}" -o json 2>/dev/null \
        | evidence_scrub_json \
        | evidence_redact > "${EVIDENCE_DIR}/worker-cr.json" || true

    # MinIO listing under the worker prefix
    exec_in_manager mc ls --recursive "${storage_prefix}/agents/${worker}/" 2>/dev/null \
        | evidence_redact > "${EVIDENCE_DIR}/minio-listing.txt" || true

    return 0
}
