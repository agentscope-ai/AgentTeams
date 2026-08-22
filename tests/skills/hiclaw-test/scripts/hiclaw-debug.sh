#!/bin/bash
# hiclaw-debug.sh - Export HiClaw debug logs for analysis
# Usage: hiclaw-debug.sh [command] [time_range]
#
# Commands:
#   export   - Export debug logs (default)
#   analyze  - Analyze hang issues
#   all      - Export and analyze (default)
#
# Arguments:
#   time_range - Time range (default: 1h), supports 10m, 1h, 1d, etc.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMMAND="${1:-all}"
TIME_RANGE="${2:-1h}"

# Auto-detect HiClaw repository directory
detect_repo_dir() {
    if [ -f "$PROJECT_ROOT/scripts/export-debug-log.py" ]; then
        echo "$PROJECT_ROOT"
        return
    fi
    
    # Check common locations
    for dir in "/tmp/hiclaw" "$HOME/hiclaw" "$HOME/workspace/hiclaw"; do
        if [ -f "$dir/scripts/export-debug-log.py" ]; then
            echo "$dir"
            return
        fi
    done
    
    echo "ERROR: Cannot find HiClaw repository (export-debug-log.py not found)" >&2
    exit 1
}

HICLAW_REPO="$(detect_repo_dir)"

echo "=== HiClaw Debug Log Exporter ==="
echo "Repository: $HICLAW_REPO"
echo "Time range: $TIME_RANGE"

# Export debug logs
export_logs() {
    echo ""
    echo "[1/2] Exporting debug logs..."
    
    cd "$HICLAW_REPO"
    python3 scripts/export-debug-log.py --range "$TIME_RANGE" 2>&1
    
    # Get the latest output directory
    OUTPUT_DIR=$(ls -td "$HICLAW_REPO/debug-log"/* 2>/dev/null | head -1)
    
    if [ -z "$OUTPUT_DIR" ] || [ ! -d "$OUTPUT_DIR" ]; then
        echo "ERROR: Failed to find output directory" >&2
        exit 1
    fi
    
    echo ""
    echo "Output: $OUTPUT_DIR"
}

# Analyze hang issues
analyze_hang() {
    echo ""
    echo "[2/2] Analyzing potential hang issues..."
    
    OUTPUT_DIR="${OUTPUT_DIR:-$(ls -td "$HICLAW_REPO/debug-log"/* 2>/dev/null | head -1)}"
    
    if [ -z "$OUTPUT_DIR" ] || [ ! -d "$OUTPUT_DIR" ]; then
        echo "ERROR: No debug log found. Run 'export' first." >&2
        exit 1
    fi
    
    MATRIX_DIR="$OUTPUT_DIR/matrix-messages"
    ANALYSIS_FILE="$OUTPUT_DIR/hang-analysis.txt"
    
    if [ ! -d "$MATRIX_DIR" ]; then
        echo "ERROR: No matrix-messages directory found" >&2
        exit 1
    fi
    
    {
        echo "=== HiClaw Hang Analysis ==="
        echo "Generated: $(date)"
        echo "Time range: last $TIME_RANGE"
        echo "Source: export-debug-log.py"
        echo ""
        echo "=== Analyzing Matrix messages for PHASE_DONE mentions ==="
        
        # Use Python to analyze Matrix messages
        python3 - "$MATRIX_DIR" <<'PYEOF'
import json
import glob
import sys
import os
import re

matrix_dir = sys.argv[1]
found_issues = []
phase_done_pattern = re.compile(r'\*?\*?PHASE\s*\d*\s*DONE\*?\*?|REVISION_NEEDED', re.IGNORECASE)

for f in glob.glob(f"{matrix_dir}/*.jsonl"):
    with open(f) as file:
        for line_num, line in enumerate(file, 1):
            try:
                msg = json.loads(line)
                if msg.get('type') != 'm.room.message':
                    continue
                    
                body = msg.get('body', '')
                sender = msg.get('sender', '')
                
                # Only check messages from workers
                if not any(w in sender for w in ['alice', 'bob', 'charlie']):
                    continue
                
                # Check for PHASE_DONE / REVISION_NEEDED messages
                if phase_done_pattern.search(body):
                    has_at_manager = '@manager' in body.lower()
                    
                    if not has_at_manager:
                        # Extract PHASE type
                        phase_match = re.search(r'(PHASE\s*\d*\s*DONE|REVISION_NEEDED)', body, re.IGNORECASE)
                        phase_type = phase_match.group(1) if phase_match else 'UNKNOWN'
                        
                        found_issues.append({
                            'file': os.path.basename(f)[:50],
                            'line': line_num,
                            'sender': sender.split(':')[0].replace('@', ''),
                            'phase': phase_type,
                            'body_preview': body[:150].replace('\n', ' ')
                        })
            except:
                pass

if found_issues:
    print(f"\n⚠️  Found {len(found_issues)} PHASE_DONE messages WITHOUT @manager mention:\n")
    for i, issue in enumerate(found_issues, 1):
        print(f"{i}. {issue['sender']}: {issue['phase']}")
        print(f"   File: {issue['file']}")
        print(f"   Preview: {issue['body_preview']}...")
        print()
    print("💡 These messages may cause Manager to miss phase completion and hang.")
    print("   Workers should @mention Manager when reporting PHASE_DONE.")
else:
    print("\n✅ All PHASE_DONE messages include @manager mention")
    print("   (or no PHASE_DONE messages found in the time range)\n")
PYEOF
        
    } > "$ANALYSIS_FILE" 2>&1
    
    echo ""
    cat "$ANALYSIS_FILE"
    echo ""
    echo "Analysis saved to: $ANALYSIS_FILE"
}

case "$COMMAND" in
    export)
        export_logs
        ;;
    analyze)
        analyze_hang
        ;;
    all|*)
        export_logs
        analyze_hang
        ;;
esac

echo ""
echo "=== Complete ==="
