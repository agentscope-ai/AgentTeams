#!/bin/bash
# Create a processing marker for a task directory
# Usage: create-processing-marker.sh <task-id> <processor-name> [timeout-mins]

set -e

task_id="$1"
processor="$2"
timeout_mins="${3:-15}"

if [ -z "$task_id" ] || [ -z "$processor" ]; then
    echo "Usage: $0 <task-id> <processor-name> [timeout-mins]" >&2
    exit 2
fi

task_dir="/root/agentteams-fs/shared/tasks/${task_id}"
marker_file="$task_dir/.processing"

# Ensure task directory exists
mkdir -p "$task_dir"

# Calculate timestamps
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Add timeout minutes to current time
if [[ "$OSTYPE" == "darwin"* ]]; then
    # macOS
    expires_at=$(date -u -v+${timeout_mins}M +%Y-%m-%dT%H:%M:%SZ)
else
    # Linux
    expires_at=$(date -u -d "+${timeout_mins} minutes" +%Y-%m-%dT%H:%M:%SZ)
fi

# Create marker file
cat > "$marker_file" << EOF
{
  "processor": "${processor}",
  "started_at": "${started_at}",
  "expires_at": "${expires_at}"
}
EOF

echo "[create-processing-marker] Created marker for ${task_id}"
echo "  Processor: ${processor}"
echo "  Started: ${started_at}"
echo "  Expires: ${expires_at}"
