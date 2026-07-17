#!/bin/bash
# start-copaw-manager.sh - Start Manager Agent with CoPaw runtime
# Called by start-manager-agent.sh when AGENTTEAMS_MANAGER_RUNTIME=copaw
#
# Bridge, layout, DM-room bootstrap, CMS, and app launch are handled by
# copaw_worker.run_manager_app (Phase 9 G9.6).

source /opt/hiclaw/scripts/lib/hiclaw-env.sh

export COPAW_LOG_LEVEL="${COPAW_LOG_LEVEL:-info}"

log "Starting CoPaw Manager via run_manager_app..."
exec python3 -m copaw_worker.run_manager_app app --host 0.0.0.0 --port 18799
