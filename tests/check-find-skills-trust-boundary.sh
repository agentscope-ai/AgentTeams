#!/usr/bin/env bash
# Guard for the find-skills trust boundary (issue #1672).
#
# Worker isolation only limits the blast radius of installed skills; it does
# NOT make registry skills safe or trusted. The find-skills docs used to claim
# "You can safely search and install skills" (and README.md claimed installs
# were "Safe because Workers can't access real credentials"), hiding the fact
# that installed skills are unvetted instructions and code that run inside the
# Worker environment.
#
# This guard fails when:
#   - the legacy "safely search and install" / "Safe because Workers can't
#     access real credentials" claims reappear anywhere in the tree;
#   - a find-skills SKILL.md Security Assurance section no longer explains,
#     in detectable wording, that isolation only limits impact, that installed
#     skills are unvetted code executed inside the Worker, that known/trusted
#     publishers are preferred, and that the skill's files and commands must
#     be reviewed BEFORE installing/enabling it (the review verb, the
#     files/commands object and the pre-install timing must appear together
#     in one sentence, so an unrelated "review" or a review that happens after
#     installation cannot satisfy the check);
#   - anti-review phrasing such as "No need to review ... before installing"
#     is used;
#   - the README.md "Skills Ecosystem" bullet drops its trust-boundary
#     statement.
#
# --self-test runs fixture-based cases (including unrelated-review and
# negated-review fixtures that must fail) instead of checking the repository.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
GUARD_NAME="$(basename "${BASH_SOURCE[0]}")"

# Every shipped find-skills copy that must keep the corrected wording.
CANONICAL_SKILLS=(
    plugins/teamharness/skills/agent/find-skills/SKILL.md
    plugins/agentteams-teamharness/skills/agent/find-skills/SKILL.md
    manager/agent/worker-agent/skills/find-skills/SKILL.md
    manager/agent/openhuman-worker-agent/skills/find-skills/SKILL.md
    manager/agent/hermes-worker-agent/skills/find-skills/SKILL.md
    manager/agent/copaw-worker-agent/skills/find-skills/SKILL.md
)

# Legacy "isolation means safe" claims that must never reappear.
FORBIDDEN_PATTERNS=(
    'you can safely search and install'
    'safe because workers can.?t access real credentials'
)

# Required trust-boundary elements. Each regex co-locates its elements within
# a single sentence (periods act as delimiters) so that unrelated or negated
# wording cannot satisfy a check on its own.
P_ISOLATION_LIMITS='(isolat|sandbox)[^.]{0,150}(only |just |merely )?(limits?|restricts?|bounds?|constrains?)[^.]{0,100}(blast[ ]radius|impact|reach|scope|damage)'
P_UNTRUSTED='(unvetted|untrusted|unaudited)|((not|never|isn.t|aren.t|is not|are not)[^.]{0,80}(automatically )?(safe|trusted|vetted|audited|secure))'
# Order A: "Before installing ..., review ... files/commands".
# Order B: "review ... files/commands ... before installing ...".
P_REVIEW_BEFORE_INSTALL='(before[^.]{0,150}(install|enabl|run|us|trust)[^.]{0,150}[,;][^.]{0,200}(review|inspect|examin|audit|check)[^.]{0,200}(files|commands|scripts|SKILL\.md))|((review|inspect|examin|audit|check)[^.]{0,150}(files|commands|scripts|SKILL\.md)[^.]{0,150}before[^.]{0,100}(install|enabl|run|us|trust))'
P_TRUSTED_PUBLISHERS='(prefer|preference|choose|favor|favour|opt for|look for|stick to)[^.]{0,150}(trusted|known|reputable|well-known|vetted)[^.]{0,80}(publisher|author|creator|maintainer|source)'

# Anti-review phrasing: the docs must not tell the reader that reviewing is
# unnecessary, e.g. "No need to review the skill's files and commands before
# installing it" (which would otherwise still match the review regex above).
P_ANTI_REVIEW='(no need to|not necessary to|don.t need to|do not need to|no reason to|skip the|skip this|without needing to)[^.]{0,60}(review|inspect|examin|audit|check)'

extract_section() {
    # Print the "## Security Assurance" section (up to the next "## "
    # heading) flattened onto one line so sentences stay delimited by periods.
    awk '
        /^## Security Assurance/ { insec = 1; next }
        /^## / { insec = 0 }
        insec { printf "%s ", $0 }
    ' "$1"
}

check_trust_boundary_text() {
    # $1 = source description, $2 = text to validate. All required elements
    # must be present and anti-review phrasing must be absent.
    local source="$1" text="$2" failed=0
    if ! grep -iqE "${P_ISOLATION_LIMITS}" <<<"${text}"; then
        echo "FAIL: ${source}: must state that isolation only limits the blast radius/impact of installed skills" >&2
        failed=1
    fi
    if ! grep -iqE "${P_UNTRUSTED}" <<<"${text}"; then
        echo "FAIL: ${source}: must state that installed skills are unvetted instructions/code and not automatically safe or trusted" >&2
        failed=1
    fi
    if ! grep -iqE "${P_REVIEW_BEFORE_INSTALL}" <<<"${text}"; then
        echo "FAIL: ${source}: must tell the reader to review the skill's files and commands BEFORE installing/enabling it (review verb, files/commands and pre-install timing must appear in the same sentence)" >&2
        failed=1
    fi
    if ! grep -iqE "${P_TRUSTED_PUBLISHERS}" <<<"${text}"; then
        echo "FAIL: ${source}: must recommend preferring known/trusted publishers" >&2
        failed=1
    fi
    if grep -iqE "${P_ANTI_REVIEW}" <<<"${text}"; then
        echo "FAIL: ${source}: must not negate the review step (e.g. 'no need to review ... before installing')" >&2
        failed=1
    fi
    return "${failed}"
}

check_skill_file() {
    local file="$1" section
    section="$(extract_section "${file}")"
    if [ -z "$(printf '%s' "${section}" | tr -d '[:space:]')" ]; then
        echo "FAIL: ${file}: no '## Security Assurance' section found" >&2
        return 1
    fi
    check_trust_boundary_text "${file}" "${section}"
}

check_readme() {
    local root="$1" readme="${root}/README.md" bullet
    if [ ! -f "${readme}" ]; then
        echo "FAIL: README.md is missing" >&2
        return 1
    fi
    bullet="$(grep -E '^- \*\*Skills Ecosystem\*\*' "${readme}" || true)"
    if [ -z "${bullet}" ]; then
        echo "FAIL: README.md: the 'Skills Ecosystem' bullet is missing; it must state the find-skills trust boundary" >&2
        return 1
    fi
    check_trust_boundary_text "README.md Skills Ecosystem bullet" "${bullet}"
}

scan_forbidden() {
    local root="$1" matches patterns
    patterns="$(IFS='|'; echo "${FORBIDDEN_PATTERNS[*]}")"
    if matches="$(grep -rIinE "${patterns}" \
        --exclude="${GUARD_NAME}" \
        --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=vendor \
        "${root}" 2>/dev/null)"; then
        echo "FAIL: legacy 'isolation means safe' claims are still present:" >&2
        echo "${matches}" >&2
        return 1
    fi
    return 0
}

run_check() {
    local root="$1" failed=0 f
    scan_forbidden "${root}" || failed=1

    # In a real checkout, all shipped find-skills copies must exist. Fixture
    # trees used by --self-test have no .git and carry a single copy, so the
    # canonical list is only enforced here.
    if [ -d "${root}/.git" ]; then
        for f in "${CANONICAL_SKILLS[@]}"; do
            if [ ! -f "${root}/${f}" ]; then
                echo "FAIL: required find-skills doc is missing: ${f}" >&2
                failed=1
            fi
        done
    fi

    while IFS= read -r -d '' f; do
        check_skill_file "${f}" || failed=1
    done < <(find "${root}" -path '*/.git' -prune -o -type f -path '*/find-skills/SKILL.md' -print0 2>/dev/null)

    check_readme "${root}" || failed=1

    if [ "${failed}" -ne 0 ]; then
        exit 1
    fi
    echo "PASS: find-skills docs and README state the real trust boundary (isolation only limits blast radius; review files/commands before install)"
}

run_self_test() {
    local tmp guard script
    guard="$(basename "${BASH_SOURCE[0]}")"
    script="${SCRIPT_DIR}/${guard}"
    tmp="$(mktemp -d)"
    trap 'rm -rf "${tmp}"' RETURN

    # Trust-boundary prose up to (not including) the publisher/review
    # sentence, so fixture cases can swap only the last line.
    SKILL_HEAD='## Security Assurance

**Worker is completely isolated and cannot access any admin'"'"'s personal sensitive data.** Isolation only limits the blast radius of what you install. Skills from public or private registries are unvetted instructions and code that run inside your Worker environment with the same reach you have, so they are never automatically safe or trusted. '
    SKILL_TAIL='

## When to Use This Skill
'
    GOOD_SKILL_LINE='Prefer skills from known, trusted publishers, and before installing or enabling a skill, review its files and commands (including its SKILL.md and any scripts) to confirm they do only what they claim.'
    GOOD_README_BULLET='- **Skills Ecosystem**: Workers pull from [skills.sh](https://skills.sh) (80,000+ community skills) on demand. Worker isolation only limits the blast radius — installed skills are unvetted instructions and code that run inside the Worker, so prefer trusted publishers and review a skill'"'"'s files and commands before installing it.'

    expect_pass() {
        local tree="$1" label="$2"
        if ! bash "${tree}/tests/${guard}" >/dev/null 2>&1; then
            echo "self-test FAIL: ${label} did not pass the guard; guard output:" >&2
            bash "${tree}/tests/${guard}" 2>&1 || true
            return 1
        fi
    }

    expect_fail() {
        local tree="$1" label="$2"
        if bash "${tree}/tests/${guard}" >/dev/null 2>&1; then
            echo "self-test FAIL: ${label} did not fail the guard" >&2
            return 1
        fi
    }

    write_skill() {
        # $1 = tree dir, $2 = final publisher/review sentence.
        printf '%s%s%s' "${SKILL_HEAD}" "$2" "${SKILL_TAIL}" > "$1/manager/agent/worker-agent/skills/find-skills/SKILL.md"
    }

    make_tree() {
        # $1 = tree dir; copies the guard and writes the good baseline docs.
        mkdir -p "$1/tests" "$1/manager/agent/worker-agent/skills/find-skills"
        cp "${script}" "$1/tests/"
        write_skill "$1" "${GOOD_SKILL_LINE}"
        printf '%s\n\n## Quick Start\n' "${GOOD_README_BULLET}" > "$1/README.md"
    }

    # Case 1: corrected Security Assurance + README bullet passes.
    make_tree "${tmp}/pass-canonical"
    expect_pass "${tmp}/pass-canonical" "corrected trust-boundary wording"

    # Case 2: legacy "You can safely search and install" claim must fail
    # even when appended to otherwise-correct wording.
    make_tree "${tmp}/fail-old-claim"
    printf '%s\n' 'You can safely search and install skills from public or private registries.' \
        >> "${tmp}/fail-old-claim/manager/agent/worker-agent/skills/find-skills/SKILL.md"
    expect_fail "${tmp}/fail-old-claim" "legacy 'safely search and install' claim"

    # Case 3: an unrelated review sentence must not satisfy the review
    # requirement, even with the rest of the wording intact.
    make_tree "${tmp}/fail-unrelated-review"
    write_skill "${tmp}/fail-unrelated-review" \
        'Prefer skills from known, trusted publishers. Review the search results before presenting them to the user.'
    expect_fail "${tmp}/fail-unrelated-review" "unrelated review sentence"

    # Case 4: negated review guidance ("No need to review ...") must fail
    # even though it mentions files/commands and "before installing".
    make_tree "${tmp}/fail-negated-review"
    write_skill "${tmp}/fail-negated-review" \
        "Prefer skills from known, trusted publishers. No need to review the skill's files and commands before installing it."
    expect_fail "${tmp}/fail-negated-review" "negated review guidance"

    # Case 5: a review that happens only AFTER installation must fail the
    # pre-install requirement.
    make_tree "${tmp}/fail-review-after-install"
    write_skill "${tmp}/fail-review-after-install" \
        "Prefer skills from known, trusted publishers. Review the skill's files and commands after installation if something misbehaves."
    expect_fail "${tmp}/fail-review-after-install" "post-install-only review"

    # Case 6: a Security Assurance section without the review guidance at all
    # must fail.
    make_tree "${tmp}/fail-no-review"
    write_skill "${tmp}/fail-no-review" \
        'Prefer skills from known, trusted publishers.'
    expect_fail "${tmp}/fail-no-review" "missing review guidance"

    # Case 7: a missing Security Assurance section must fail.
    make_tree "${tmp}/fail-no-section"
    printf '# Find Skills\n\nNo security section here.\n' \
        > "${tmp}/fail-no-section/manager/agent/worker-agent/skills/find-skills/SKILL.md"
    expect_fail "${tmp}/fail-no-section" "missing Security Assurance section"

    # Case 8: a README bullet without the trust-boundary statement must fail.
    make_tree "${tmp}/fail-readme-plain"
    printf '%s\n\n## Quick Start\n' \
        '- **Skills Ecosystem**: Workers pull from [skills.sh](https://skills.sh) (80,000+ community skills) on demand.' \
        > "${tmp}/fail-readme-plain/README.md"
    expect_fail "${tmp}/fail-readme-plain" "README bullet without trust boundary"

    # Case 9: the legacy README claim ("Safe because Workers can't access
    # real credentials") must fail even if the rest of the bullet is updated.
    make_tree "${tmp}/fail-readme-legacy"
    printf '%s\n\n## Quick Start\n' \
        '- **Skills Ecosystem**: Workers pull from [skills.sh](https://skills.sh) (80,000+ community skills) on demand. Safe because Workers can'"'"'t access real credentials.' \
        > "${tmp}/fail-readme-legacy/README.md"
    expect_fail "${tmp}/fail-readme-legacy" "legacy README 'Safe because' claim"

    echo "self-test ok: corrected wording passes; legacy claims, unrelated review, negated review, post-install review, missing review guidance, missing section and README drifts all fail"
}

case "${1:-}" in
    --self-test)
        run_self_test
        ;;
    "")
        run_check "${REPO_ROOT}"
        ;;
    *)
        echo "usage: $0 [--self-test]" >&2
        exit 2
        ;;
esac
