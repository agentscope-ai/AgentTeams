#!/bin/bash
# openclaw-version-guard.sh - assert the installed OpenClaw matches the pin
#
# Why: the manager renders its openclaw.json against the OpenClaw pin owned
# by hack/versions.env, but the gateway binary comes from the openclaw-base
# image. When that base lags the pin (it actually did - an April 2026 gateway
# under a v2026.10.9 config, AgentTeams#1460), version-coupled contracts
# such as the Control UI device-identity handshake break at runtime, long
# after the image was built. The openclaw-base image now bakes its tag into
# AGENTTEAMS_OPENCLAW_TAG (openclaw-base/Dockerfile); this guard compares
# the *installed* version against the pin at manager startup so a stale base
# fails fast and loudly instead of serving a drifting gateway (#1479).
#
# Safety contract - #1460 had a 70-restart crash loop caused by a startup
# check that could not parse its own input, so parse failures are non-fatal
# by construction:
#   - versions are normalized by dropping a leading `v` and keeping the
#     leading X.Y.Z prefix, so v2026.10.9 compares equal to 2026.10.9;
#   - an *unknown* installed version (file missing, unparseable) prints a
#     WARNING and returns 0;
#   - an *unknown* expected version prints a NOTE and returns 0;
#   - only a *known* mismatch fails, and only in fatal mode.
#   - nothing here touches the network.
#
# Source this file; it defines:
#   openclaw_version_normalize RAW   leading X.Y.Z of RAW ("" when absent)
#   openclaw_installed_version       print the installed version; "" on any failure
#   openclaw_expected_version        print the pinned version; "" when unavailable
#   openclaw_version_guard MODE      compare; MODE=fatal (default) returns 1 on a
#                                    known mismatch, MODE=warn prints a WARNING
#                                    and returns 0

# Leading X.Y.Z prefix of a raw version string; a leading `v` is dropped
# first, so tag-style (v2026.10.9) and package.json-style (2026.10.9)
# strings compare equal.
openclaw_version_normalize() {
    local raw="${1:-}"
    raw="${raw#v}"
    printf '%s\n' "${raw}" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' 2>/dev/null | head -n1 || true
}

# Print the installed OpenClaw version. The source of truth is the .version
# field of $OPENCLAW_PACKAGE_JSON (default /opt/openclaw/package.json -
# OpenClaw is source-built there by openclaw-base/Dockerfile); `openclaw
# --version` is the documented fallback. Any failure prints an empty line
# and returns 0: the guard must never crash a boot over a version it cannot
# read.
openclaw_installed_version() {
    local pkg="${OPENCLAW_PACKAGE_JSON:-/opt/openclaw/package.json}"
    local v=""
    if [ -f "${pkg}" ]; then
        if command -v node >/dev/null 2>&1; then
            v="$(node -p "require('${pkg}').version" 2>/dev/null | tr -d '\r')" || true
        fi
        if [ -z "${v}" ]; then
            # Dependency-free fallback for hosts without node (stub tests).
            v="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${pkg}" 2>/dev/null | head -n1 | tr -d '\r')" || true
        fi
    fi
    if [ -z "${v}" ]; then
        v="$(openclaw --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' 2>/dev/null | head -n1)" || true
    fi
    printf '%s\n' "${v}"
    return 0
}

# Print the pinned version. $AGENTTEAMS_OPENCLAW_TAG (baked into the
# openclaw-base image as ENV by the pin-derivation commit) wins; the repo
# pin in hack/versions.env is the fallback when this file is sourced from
# the source tree (the CI probe runs on the host, outside any image).
# Unavailable -> empty line, return 0.
openclaw_expected_version() {
    if [ -n "${AGENTTEAMS_OPENCLAW_TAG:-}" ]; then
        printf '%s\n' "${AGENTTEAMS_OPENCLAW_TAG}"
        return 0
    fi
    local here root
    here="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)" || return 0
    root="$(cd "${here}/../../.." >/dev/null 2>&1 && pwd)" || return 0
    if [ -f "${root}/hack/versions.env" ]; then
        sed -n 's/^[[:space:]]*OPENCLAW_TAG=//p' "${root}/hack/versions.env" 2>/dev/null \
            | head -n1 | tr -d '"' | tr -d "'"
        return 0
    fi
    return 0
}

# Print a guard message through log() when the caller sourced base.sh
# (manager container), otherwise plain echo (CI probe, tests).
_openclaw_guard_out() {
    if [ "$(type -t log 2>/dev/null)" = "function" ]; then
        log "$1"
    else
        echo "$1"
    fi
}

# Compare the installed version against the pin. MODE=fatal (default)
# returns 1 on a known mismatch; MODE=warn returns 0. Unknown sides never
# fail (see the safety contract in the header).
openclaw_version_guard() {
    local mode="${1:-fatal}"
    local expected actual
    expected="$(openclaw_expected_version)"
    actual="$(openclaw_installed_version)"
    local norm_expected norm_actual
    norm_expected="$(openclaw_version_normalize "${expected}")"
    norm_actual="$(openclaw_version_normalize "${actual}")"

    if [ -z "${norm_actual}" ]; then
        _openclaw_guard_out "WARNING: cannot determine the installed OpenClaw version; skipping the pin assertion"
        return 0
    fi
    if [ -z "${norm_expected}" ]; then
        _openclaw_guard_out "NOTE: no pinned OpenClaw version available; skipping the pin assertion"
        return 0
    fi
    if [ "${norm_actual}" = "${norm_expected}" ]; then
        return 0
    fi
    if [ "${mode}" = "warn" ]; then
        _openclaw_guard_out "WARNING: OpenClaw ${actual} does not match the pinned ${expected}"
        return 0
    fi
    _openclaw_guard_out "FATAL: OpenClaw ${actual} does not match the pinned ${expected}"
    return 1
}
