#!/usr/bin/env bash
# Guard the coding-cli-management skill's first-use flow.
#
# The persistence rewrite once deleted "Step 1: First-Time Detection", leaving
# Step 2 to require enabled:true and Step 3 to read .cli from a file that was
# never created. This test fails if the detection / explicit admin choice /
# config creation flow disappears again, or if a Worker install is presented as
# equivalent to enabling delegation in the delegating agent's own config.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
skill="$root/manager/agent/skills/coding-cli-management/SKILL.md"

fail=0
need() {
  if ! grep -Fq -- "$1" "$skill"; then
    echo "ERROR: SKILL.md is missing: $1" >&2
    fail=1
  fi
}

need '## Step 1: First-Time Detection and Explicit Enablement'
need 'detect-available-cli.sh'
need '~/coding-cli-config.json'
need '"enabled":false'
need '"enabled":true'
need '"cli":"<chosen-tool>"'
need 'No automatic enablement.'
need 'Worker container'
need 'installing a'
need 'CLI in a Worker is a different operation'

step1_line="$(grep -n '^## Step 1:' "$skill" | cut -d: -f1)"
step2_line="$(grep -n '^## Step 2:' "$skill" | cut -d: -f1)"
step3_line="$(grep -n '^## Step 3:' "$skill" | cut -d: -f1)"

if [ -z "$step1_line" ] || [ -z "$step2_line" ] || [ -z "$step3_line" ]; then
  echo "ERROR: Step 1/2/3 headings are not all present" >&2
  fail=1
elif [ "$step1_line" -ge "$step2_line" ] || [ "$step2_line" -ge "$step3_line" ]; then
  echo "ERROR: steps are out of order (Step1=$step1_line Step2=$step2_line Step3=$step3_line)" >&2
  fail=1
fi

# Step 2 and Step 3 must still consume the file Step 1 creates.
step2_3="$(sed -n "${step2_line},\$p" "$skill")"
if ! printf '%s' "$step2_3" | grep -Fq 'enabled: true'; then
  echo "ERROR: Step 2 no longer gates on enabled: true" >&2
  fail=1
fi
if ! printf '%s' "$step2_3" | grep -Fq "jq -r '.cli' ~/coding-cli-config.json"; then
  echo "ERROR: Step 3 no longer reads .cli from coding-cli-config.json" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "Coding CLI skill flow regression failed." >&2
  exit 1
fi
echo "Coding CLI skill flow OK (detect -> explicit choice -> config -> Step 2/3)."
