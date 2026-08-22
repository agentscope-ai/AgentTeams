#!/bin/bash
# run-hiclaw-test.sh - Quick HiClaw test runner
# Usage: run-hiclaw-test.sh [options] [test-filter]
#
# Options:
#   --repo-dir <path>     HiClaw repository directory (default: current dir or /tmp/hiclaw)
#   --env-file <path>     Environment config file (default: ~/hiclaw-manager.env)
#   --skip-pull           Skip git pull
#   existing              Run tests using existing installation
#
# Examples:
#   run-hiclaw-test.sh                        # Run all tests
#   run-hiclaw-test.sh "01 02 03"             # Run tests 01, 02, 03 only
#   run-hiclaw-test.sh existing               # Run with existing installation
#   run-hiclaw-test.sh --repo-dir ~/hiclaw   # Specify repository directory

set -e

# Default values (can be overridden by environment variables)
REPO_DIR="${HICLAW_REPO_DIR:-}"
ENV_FILE="${HICLAW_ENV_FILE:-$HOME/hiclaw-manager.env}"
SKIP_PULL=false
TEST_FILTER=""

# Color definitions
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --repo-dir)
            REPO_DIR="$2"
            shift 2
            ;;
        --env-file)
            ENV_FILE="$2"
            shift 2
            ;;
        --skip-pull)
            SKIP_PULL=true
            shift
            ;;
        existing)
            TEST_FILTER="existing"
            shift
            ;;
        *)
            TEST_FILTER="$1"
            shift
            ;;
    esac
done

# Auto-detect repository directory
detect_repo_dir() {
    if [ -n "$REPO_DIR" ]; then
        return
    fi
    
    # Check current directory
    if [ -f "./Makefile" ] && grep -q "hiclaw" ./Makefile 2>/dev/null; then
        REPO_DIR="$(pwd)"
        return
    fi
    
    # Check standard locations
    for dir in "./hiclaw" "../hiclaw" "/tmp/hiclaw" "$HOME/hiclaw"; do
        if [ -d "$dir" ] && [ -f "$dir/Makefile" ]; then
            REPO_DIR="$dir"
            return
        fi
    done
    
    # Default to /tmp/hiclaw
    REPO_DIR="/tmp/hiclaw"
}

# Check prerequisites
check_prerequisites() {
    if [ ! -f "$ENV_FILE" ]; then
        log_error "Config file not found: $ENV_FILE"
        log_info "Please create hiclaw-manager.env first or set HICLAW_ENV_FILE"
        exit 1
    fi
    
    if ! command -v docker &> /dev/null; then
        log_error "Docker not installed"
        exit 1
    fi
}

# Clone/update repository
update_repo() {
    if [ ! -d "$REPO_DIR" ]; then
        log_info "Cloning HiClaw repository to $REPO_DIR..."
        git clone https://github.com/alibaba/hiclaw.git "$REPO_DIR"
        cd "$REPO_DIR"
    elif [ "$SKIP_PULL" = true ]; then
        log_info "Skipping git pull (--skip-pull)"
        cd "$REPO_DIR"
    else
        log_info "Updating HiClaw repository at $REPO_DIR..."
        cd "$REPO_DIR"
        git fetch origin
        git reset --hard origin/main
    fi
    
    log_info "Repository ready at $REPO_DIR"
}

# Run tests
run_tests() {
    cd "$REPO_DIR"
    
    # Load environment variables
    set -a
    source "$ENV_FILE"
    set +a
    
    export HICLAW_YOLO=1
    
    if [ "$TEST_FILTER" = "existing" ]; then
        # Use existing installation
        log_info "Running tests with existing installation..."
        ./tests/run-all-tests.sh --skip-build --use-existing
    elif [ -n "$TEST_FILTER" ]; then
        # Run specific tests
        log_info "Running tests: $TEST_FILTER"
        ./tests/run-all-tests.sh --test-filter "$TEST_FILTER"
    else
        # Run full test cycle
        log_info "Running full test cycle (make test)..."
        make test
    fi
}

# Show results
show_results() {
    echo ""
    log_info "=== Test Results ==="
    
    if [ -d "$REPO_DIR/tests/output" ]; then
        echo "Metrics files:"
        ls -la "$REPO_DIR/tests/output/"*.json 2>/dev/null || echo "  No metrics files found"
    fi
    
    echo ""
    echo "To debug issues, run:"
    echo "  hiclaw-debug.sh analyze"
}

# Main flow
main() {
    log_info "=== HiClaw Test Runner ==="
    
    detect_repo_dir
    log_info "Using repository: $REPO_DIR"
    
    check_prerequisites
    update_repo
    run_tests
    show_results
}

main "$@"
