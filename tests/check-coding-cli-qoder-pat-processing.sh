#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DETECT="${ROOT_DIR}/manager/agent/skills/coding-cli-management/scripts/detect-available-cli.sh"
RUN="${ROOT_DIR}/manager/agent/skills/coding-cli-management/scripts/run-coding-cli.sh"

# Regression for the #1342 reviewer follow-up (shiyiyue1102), two P2s:
#   [P2] Qoder PAT-only auth missed by availability detection — a fresh HOME,
#        qodercli on PATH and a non-empty QODER_PERSONAL_ACCESS_TOKEN must count
#        as configured; honor QODER_CONFIG_DIR (default ~/.qoder); never leak
#        the token; empty/unset PAT is NOT configured.
#   [P2] .processing guard checked the workspace instead of the task root where
#        the marker actually lives, and had no ownership semantics — the
#        delegating flow creates its OWN marker before invoking the wrapper, so
#        that legitimate run must stay possible while a conflicting holder is
#        blocked.
# Synthetic credentials + a fake CLI only; this does NOT assert a successful
# authenticated Qoder model execution.

command -v jq >/dev/null 2>&1 || { echo "SKIP: jq required" >&2; exit 0; }

bash -n "${DETECT}"
bash -n "${RUN}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/home"

# ── detect-available-cli.sh: Qoder PAT + QODER_CONFIG_DIR ─────────────────
printf '#!/bin/sh\necho qodercli\n' > "$work/bin/qodercli"; chmod +x "$work/bin/qodercli"

# 1. PAT-only (non-empty), no config dir -> configured + available
out=$(HOME="$work/home" QODER_PERSONAL_ACCESS_TOKEN="synthetic-pat-xyz" PATH="$work/bin:$PATH" bash "${DETECT}")
[ "$(jq -r '.details.qodercli.config' <<<"$out")" = "true" ] || { echo "FAIL: non-empty QODER_PERSONAL_ACCESS_TOKEN must count as configured" >&2; exit 1; }
jq -e '.available | index("qodercli")' <<<"$out" >/dev/null || { echo "FAIL: PAT-only qodercli must be in .available" >&2; exit 1; }
[ -z "$(grep -F 'synthetic-pat-xyz' <<<"$out" || true)" ] || { echo "FAIL: PAT value leaked into detect output" >&2; exit 1; }

# 2. empty PAT + no config dir -> NOT configured
out=$(HOME="$work/home" QODER_PERSONAL_ACCESS_TOKEN="" PATH="$work/bin:$PATH" bash "${DETECT}")
[ "$(jq -r '.details.qodercli.config' <<<"$out")" = "false" ] || { echo "FAIL: empty PAT must NOT count as configured" >&2; exit 1; }

# 3. unset PAT + no config dir -> NOT configured
out=$(HOME="$work/home" env -u QODER_PERSONAL_ACCESS_TOKEN PATH="$work/bin:$PATH" bash "${DETECT}")
[ "$(jq -r '.details.qodercli.config' <<<"$out")" = "false" ] || { echo "FAIL: unset PAT must NOT count as configured" >&2; exit 1; }

# 4. custom QODER_CONFIG_DIR present -> configured via dir
mkdir -p "$work/home/custom-qoder"
out=$(HOME="$work/home" QODER_CONFIG_DIR="$work/home/custom-qoder" PATH="$work/bin:$PATH" bash "${DETECT}")
[ "$(jq -r '.details.qodercli.config_source.dir' <<<"$out")" = "true" ] || { echo "FAIL: QODER_CONFIG_DIR config dir must be detected" >&2; exit 1; }

# 5. default ~/.qoder still detected (regression guard for the original path)
mkdir -p "$work/home/.qoder"
out=$(HOME="$work/home" PATH="$work/bin:$PATH" bash "${DETECT}")
[ "$(jq -r '.details.qodercli.config_source.dir' <<<"$out")" = "true" ] || { echo "FAIL: default ~/.qoder must still be detected" >&2; exit 1; }

# ── run-coding-cli.sh: .processing guard (task root + ownership) ──────────
task_root="$work/task-1"; ws="$task_root/workspace"; mkdir -p "$ws"
prompt="$work/p.txt"; echo "do x" > "$prompt"
fut() { date -u -d '+15 minutes' +%Y-%m-%dT%H:%M:%SZ; }
mk_marker() { jq -n --arg processor "$1" --arg expires "$2" --arg run_id "${3:-}" '{processor:$processor, expires_at:$expires, run_id:$run_id}' > "$task_root/.processing"; }
run_cli() { ( set +e; PATH="$work/bin:$PATH" bash "${RUN}" --cli qodercli --workspace "$ws" --prompt-file "$prompt" "$@" >"$work/run.out" 2>&1; echo $? ); }

# A. conflicting processor -> blocked (125)
mk_marker alice "$(fut)"; got=$(run_cli --task-dir "$task_root" --processor bob)
[ "$got" = "125" ] || { echo "FAIL: conflicting processor must be blocked (got $got, want 125)" >&2; exit 1; }
# B. your processor AND unique run ID match -> allowed (0)
mk_marker manager "$(fut)" run-a; got=$(run_cli --task-dir "$task_root" --processor manager --run-id run-a)
[ "$got" = "0" ] || { echo "FAIL: delegating flow's own marker must be allowed (got $got, want 0)" >&2; exit 1; }
# C. undeclared caller with a marker present -> blocked (125)
mk_marker manager "$(fut)"; got=$(run_cli --task-dir "$task_root")
[ "$got" = "125" ] || { echo "FAIL: undeclared caller must be blocked when a marker exists (got $got, want 125)" >&2; exit 1; }
# D. expired marker -> ignored (0)
mk_marker alice "2020-01-01T00:00:00Z"; got=$(run_cli --task-dir "$task_root" --processor bob)
[ "$got" = "0" ] || { echo "FAIL: expired marker must be ignored (got $got, want 0)" >&2; exit 1; }
# E. default task root (dirname of workspace) + conflicting marker -> blocked (125)
mk_marker alice "$(fut)"; got=$(run_cli --processor bob)
[ "$got" = "125" ] || { echo "FAIL: default task root (dirname workspace) must be honored (got $got, want 125)" >&2; exit 1; }

# F. the same processor's DIFFERENT run must not inherit ownership
mk_marker manager "$(fut)" run-a
got=$(run_cli --task-dir "$task_root" --processor manager --run-id run-b)
[ "$got" = "125" ] || { echo "FAIL: same processor with a different run ID must be blocked (got $got)" >&2; exit 1; }
! grep -q 'Finished with exit code' "$work/run.out" || { echo "FAIL: conflicting run invoked the CLI" >&2; exit 1; }

# G. a matching processor without its run ID is not the owner
got=$(run_cli --task-dir "$task_root" --processor manager)
[ "$got" = "125" ] || { echo "FAIL: an undeclared run must be blocked (got $got)" >&2; exit 1; }

# H. a run ID does not permit impersonating another processor
got=$(run_cli --task-dir "$task_root" --processor other --run-id run-a)
[ "$got" = "125" ] || { echo "FAIL: a different processor must be blocked even with the run ID (got $got)" >&2; exit 1; }

# I. legacy markers without run IDs cannot prove ownership
printf '{"processor":"manager","expires_at":"%s"}' "$(fut)" > "$task_root/.processing"
got=$(run_cli --task-dir "$task_root" --processor manager --run-id run-a)
[ "$got" = "125" ] || { echo "FAIL: active legacy marker must be respected (got $got)" >&2; exit 1; }

# J. no marker still permits old callers without ownership arguments
rm -f "$task_root/.processing"
got=$(run_cli)
[ "$got" = "0" ] || { echo "FAIL: marker-free legacy call should work (got $got)" >&2; exit 1; }

# Exercise the actual marker scripts with only their fixed filesystem root
# redirected into the fixture; no production logic is replaced.
mkdir -p "$work/shared/tasks"
for script in create-processing-marker remove-processing-marker; do
    sed "s|/root/agentteams-fs/shared/tasks/|$work/shared/tasks/|g" \
        "$ROOT_DIR/manager/agent/skills/task-coordination/scripts/$script.sh" > "$work/$script.sh"
done
bash "$work/create-processing-marker.sh" ownership-test manager 15 run-a >/dev/null
marker="$work/shared/tasks/ownership-test/.processing"
[ "$(jq -r '.run_id' "$marker")" = run-a ] || { echo "FAIL: marker creator lost the run ID" >&2; exit 1; }
mkdir -p "$work/shared/tasks/ownership-test/workspace"
got=$(run_cli --workspace "$work/shared/tasks/ownership-test/workspace" \
    --task-dir "$work/shared/tasks/ownership-test" --processor manager --run-id run-a)
[ "$got" = 0 ] || { echo "FAIL: wrapper rejected the actual creator's marker (got $got)" >&2; exit 1; }
got=$( (set +e; bash "$work/remove-processing-marker.sh" ownership-test run-b >/dev/null 2>&1; echo $?) )
[ "$got" != 0 ] && [ -f "$marker" ] || { echo "FAIL: different run removed the marker" >&2; exit 1; }
bash "$work/remove-processing-marker.sh" ownership-test run-a >/dev/null
[ ! -f "$marker" ] || { echo "FAIL: owner could not remove its marker" >&2; exit 1; }
bash "$work/create-processing-marker.sh" ownership-test manager 15 >/dev/null
jq -e 'has("run_id") | not' "$marker" >/dev/null || { echo "FAIL: legacy marker format changed" >&2; exit 1; }
bash "$work/remove-processing-marker.sh" ownership-test >/dev/null
[ ! -f "$marker" ] || { echo "FAIL: legacy marker removal broke" >&2; exit 1; }

echo "PASS: coding-cli Qoder PAT/QODER_CONFIG_DIR detection + .processing task-root ownership guard"
