#!/usr/bin/env bash
# Check that the OpenClaw patch series (hack/patches/openclaw/0*.patch)
# applies cleanly, in order, to an OpenClaw checkout.
set -u

usage() {
    cat <<'EOF'
Usage: check-openclaw-patches.sh [--repo <checkout>] [--tag <tag>]

Check that the AgentTeams OpenClaw patch series (hack/patches/openclaw/)
applies cleanly, in 0001 -> 0008 order, to an OpenClaw git checkout.

Options:
  --repo <checkout>  Path to an OpenClaw checkout. Default: the sibling
                     directory named "OpenClaw" of this repository's git
                     common dir (SC/OpenClaw in the standard layout).
  --tag <tag>        Tag the checkout is expected to be at; printed in the
                     success line instead of `git describe --tags --always`.
  -h, --help         Show this help and exit 0.

Exit codes:
  0  all patches apply        -> "OK <n> patches apply to <tag>"
  1  a patch failed           -> "FAILED <patch>: <conflict files>"
  2  usage error (unknown flag or missing value)
  3  --repo path missing or not a git work tree
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PATCH_DIR="$SCRIPT_DIR/patches/openclaw"

REPO=""
TAG=""

while [ $# -gt 0 ]; do
    case "$1" in
        --repo)
            if [ $# -lt 2 ]; then
                echo "ERROR: --repo needs a value" >&2
                exit 2
            fi
            REPO="$2"
            shift 2
            ;;
        --tag)
            if [ $# -lt 2 ]; then
                echo "ERROR: --tag needs a value" >&2
                exit 2
            fi
            TAG="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

if [ -z "$REPO" ]; then
    TOPLEVEL="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>/dev/null || true)"
    if [ -z "$TOPLEVEL" ]; then
        echo "ERROR: cannot determine this repository's location; pass --repo <checkout>" >&2
        exit 3
    fi
    COMMON_DIR="$(cd "$TOPLEVEL" && git rev-parse --git-common-dir 2>/dev/null || true)"
    case "$COMMON_DIR" in
        /*) ;;
        *) COMMON_DIR="$TOPLEVEL/$COMMON_DIR" ;;
    esac
    MAIN_ROOT="$(dirname "$COMMON_DIR")"
    REPO="$MAIN_ROOT/../OpenClaw"
fi

if [ ! -d "$REPO" ]; then
    echo "ERROR: repo not found: $REPO" >&2
    echo "pass --repo <openclaw checkout>" >&2
    exit 3
fi

if ! git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1; then
    echo "ERROR: not a git work tree: $REPO" >&2
    exit 3
fi

PATCHES=()
for p in "$PATCH_DIR"/0*.patch; do
    [ -f "$p" ] || continue
    PATCHES+=("$p")
done

if [ "${#PATCHES[@]}" -eq 0 ]; then
    echo "ERROR: no patches found in $PATCH_DIR" >&2
    exit 3
fi

FAILED_PATCH=""
FAILED_FILES=""
for p in "${PATCHES[@]}"; do
    if ! ERR="$(git -C "$REPO" apply --check "$p" 2>&1)"; then
        FAILED_PATCH="$(basename "$p")"
        FAILED_FILES="$(printf '%s\n' "$ERR" \
            | sed -n 's/^error: patch failed: \([^:,]*\).*/\1/p;
                      s/^error: \([^:]*\): No such file or directory.*/\1/p;
                      s/^error: \([^:]*\): patch does not apply.*/\1/p' \
            | sort -u | paste -sd, -)"
        break
    fi
done

if [ -n "$FAILED_PATCH" ]; then
    [ -n "$FAILED_FILES" ] || FAILED_FILES="(no file-level detail in git apply output)"
    echo "FAILED $FAILED_PATCH: $FAILED_FILES"
    exit 1
fi

if [ -n "$TAG" ]; then
    CHECKOUT_TAG="$TAG"
else
    CHECKOUT_TAG="$(git -C "$REPO" describe --tags --always 2>/dev/null \
        || git -C "$REPO" rev-parse --short HEAD)"
fi

echo "OK ${#PATCHES[@]} patches apply to $CHECKOUT_TAG"
exit 0
