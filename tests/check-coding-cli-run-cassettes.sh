#!/bin/bash
# check-coding-cli-run-cassettes.sh
#
# Cassette replay for the qwen structured-output surface. Each version
# family holds a baseline of the CLI's REAL output (recorded live,
# committed under tests/cassettes/), and this suite re-runs the recorded
# invocation and diffs the live output's SHAPE against the baseline.
# Values (session ids, uuids, timings, usage, text) are fingerprinted
# away; structure is not — document shape, per-frame type/subtype,
# is_error, and each frame's top-level key set. A next-version
# output-shape drift turns this red; the fix is a deliberate re-record
# (see the meta's re_record note), not an edit here. The governance
# stub alone is necessary but not sufficient — the cassette
# is the next-version drift guard.
#
# Family selection is the wrapper's own rule: with NO explicit
# CODING_CLI_JSON_BOUNDARY the json-out family is active for EVERY version
# (capture is the wrapper's default — measured 2026-10-30: no published
# qwen writes the native --json-file file in headless one-shot mode); with
# an override, qwen --version (parsed) vs the override via sort -V,
# detection failure or empty -> legacy family. The json-out family compares
# the -o json stdout event array; the legacy family compares the native
# --json-file FILE content. Baselines are conditionally enabled: the active
# family is replayed; the inactive family's baseline is reported (a
# placeholder or an unsupported baseline reports a SKIP, no red).
#
# The replay hits the RAW CLI, not the wrapper: the cassette guards the
# CLI's output shape, so a wrapper revert cannot mis-key the baseline
# (reverting the wrapper does not delete the baselines, and
# they stay keyed to the CLI version family, not the wrapper's state).
# Opt-in like the verify suite: without qwen or python3 on PATH the
# suite skips. CODING_CLI_CASSETTE_HOME (dir holding a .qwen/ auth) can
# point the replay at an isolated HOME.
set -u

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CASSETTES="${REPO_ROOT}/tests/cassettes"

if ! command -v qwen >/dev/null 2>&1; then
    echo "[SKIP] cassette replay (qwen not on PATH — opt-in suite)"
    exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
    echo "[SKIP] cassette replay (python3 not on PATH — shape fingerprint unavailable)"
    exit 0
fi

boundary="${CODING_CLI_JSON_BOUNDARY:-}"
qwen_version="$(qwen --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || true
# The wrapper's rule: no explicit override -> the json-out (capture) family
# for every version; with an override, the detected version vs the override
# (sort -V), detection failure or empty -> legacy family.
if [ -z "${boundary}" ]; then
    family="json-out"
elif [ -n "${qwen_version}" ] && [ "$(printf '%s\n%s\n' "${boundary}" "${qwen_version}" | sort -V | tail -1)" = "${qwen_version}" ]; then
    family="json-out"
else
    family="legacy"
fi
echo "cassette replay: qwen ${qwen_version:-unknown} -> active family ${family} (boundary ${boundary:-default: capture for every version})"

meta_get() {
    python3 -c 'import json, sys
m = json.load(open(sys.argv[1]))
v = m.get(sys.argv[2], "")
print(v if v is not None else "")' "$1" "$2"
}

# One-line summary of a meta's "evidence" object, for SKIP messages that
# must name the settled measurement (unsupported baselines).
evidence_line() {
    python3 -c 'import json, sys
e = json.load(open(sys.argv[1])).get("evidence", {})
tv = ", ".join(e.get("tested_versions", []))
print("no published qwen writes the native --json-file file in headless one-shot mode (tested "
      + tv + " on " + str(e.get("tested_at", "unknown")) + " by " + str(e.get("tested_by", "unknown")) + ")")' "$1" 2>/dev/null \
        || echo "evidence in $(basename "$1")"
}

case "${family}" in
    json-out)
        stdout_file="${CASSETTES}/qwen-json-out.stdout.json";            meta_file="${CASSETTES}/qwen-json-out.meta.json"
        other_file="${CASSETTES}/qwen-json-file-legacy.meta.json";       other_family="legacy" ;;
    *)
        stdout_file="${CASSETTES}/qwen-json-file-legacy.stdout.json";    meta_file="${CASSETTES}/qwen-json-file-legacy.meta.json"
        other_file="${CASSETTES}/qwen-json-out.meta.json";               other_family="json-out" ;;
esac

# Inactive family: report its baseline state (conditionally enabled) —
# a placeholder or an unsupported baseline is a SKIP report, never red.
if [ -f "${other_file}" ]; then
    case "$(meta_get "${other_file}" status)" in
        placeholder)
            echo "[SKIP] cassette family ${other_family}: baseline is a placeholder — $(meta_get "${other_file}" note | cut -c1-120)" ;;
        unsupported)
            echo "[SKIP] cassette family ${other_family}: baseline is unsupported — $(evidence_line "${other_file}")" ;;
    esac
fi

if [ ! -f "${meta_file}" ] || [ ! -f "${stdout_file}" ]; then
    echo "FAIL replay: baseline files missing for family ${family} (${meta_file}, ${stdout_file})"
    exit 1
fi
case "$(meta_get "${meta_file}" status)" in
    placeholder)
        echo "[SKIP] replay family ${family}: baseline is a placeholder — $(meta_get "${meta_file}" note | cut -c1-120)"
        exit 0 ;;
    unsupported)
        echo "[SKIP] replay family ${family}: baseline is unsupported — $(evidence_line "${meta_file}")"
        exit 0 ;;
esac

meta_prompt="$(meta_get "${meta_file}" prompt)"
meta_rc="$(meta_get "${meta_file}" exit_code)"
meta_source="$(meta_get "${meta_file}" output_source)"
mapfile -t meta_flags < <(python3 -c 'import json, sys
for f in json.load(open(sys.argv[1])).get("flags", []):
    print(f)' "${meta_file}")

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
cd "${TMP}"

# legacy family: the recorder's --json-file path is a placeholder — swap in
# a fresh temp path, and the output is the FILE content, not stdout.
live_file=""
if [ "${meta_source}" = "file" ]; then
    live_file="${TMP}/legacy-file.json"
    replay_flags=()
    prev=""
    for f in "${meta_flags[@]}"; do
        if [ "${prev}" = "--json-file" ]; then
            replay_flags+=("${live_file}")
        else
            replay_flags+=("${f}")
        fi
        prev="${f}"
    done
else
    replay_flags=("${meta_flags[@]}")
fi

if [ -n "${CODING_CLI_CASSETTE_HOME:-}" ]; then
    HOME="${CODING_CLI_CASSETTE_HOME}"
    export HOME
fi

live_out="${TMP}/live.out"
live_err="${TMP}/live.err"
timeout 120 qwen "${replay_flags[@]}" "${meta_prompt}" > "${live_out}" 2> "${live_err}"
live_rc=$?

if [ -n "${live_file}" ] && [ ! -f "${live_file}" ]; then
    echo "FAIL replay family ${family}: the CLI wrote no --json-file output at ${live_file} (rc=${live_rc})"
    sed 's/^/    | /' "${live_err}" | tail -5
    exit 1
fi
[ -n "${live_file}" ] && cp "${live_file}" "${live_out}"

if [ "${live_rc}" != "${meta_rc}" ]; then
    echo "FAIL replay family ${family}: exit code ${live_rc} != recorded ${meta_rc} (environment or run-behavior drift — stderr below)"
    sed 's/^/    | /' "${live_err}" | tail -5
    exit 1
fi

# Shape fingerprint: document shape + per-frame type/subtype/is_error +
# top-level key sets. Volatile values are excluded by construction.
fingerprint() {
    python3 -c 'import json, sys

def frame_fp(v):
    if isinstance(v, dict):
        fp = {"keys": sorted(v.keys())}
        for k in ("type", "subtype", "is_error"):
            if k in v:
                fp[k] = v[k]
        return fp
    if isinstance(v, list):
        return ["list", [frame_fp(x) for x in v]]
    return type(v).__name__

doc = json.load(open(sys.argv[1]))
if isinstance(doc, list):
    print(json.dumps({"doc": "array", "frames": [frame_fp(x) for x in doc]}, sort_keys=True, indent=1))
else:
    print(json.dumps({"doc": type(doc).__name__, "root": frame_fp(doc)}, sort_keys=True, indent=1))
' "$1"
}

if ! base_fp="$(fingerprint "${stdout_file}" 2>/dev/null)"; then
    echo "FAIL replay family ${family}: the baseline is not parseable JSON"
    exit 1
fi
if ! live_fp="$(fingerprint "${live_out}" 2>/dev/null)"; then
    echo "FAIL replay family ${family}: live output is not parseable JSON (output-shape drift)"
    exit 1
fi

if [ "${base_fp}" != "${live_fp}" ]; then
    echo "FAIL replay family ${family}: output-shape drift vs baseline (qwen ${qwen_version:-unknown})"
    diff <(echo "${base_fp}") <(echo "${live_fp}") | head -20 | sed 's/^/    /'
    echo "    (fix: deliberate re-record per ${meta_file} — do not edit the fingerprint logic to match)"
    exit 1
fi

base_version="$(meta_get "${meta_file}" qwen_version)"
echo "PASS cassette replay family ${family} (qwen ${qwen_version:-unknown}, shape diff=0 vs baseline recorded on ${base_version})"
exit 0
