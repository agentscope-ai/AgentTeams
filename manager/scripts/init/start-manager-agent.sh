#!/bin/bash
# start-manager-agent.sh - Initialize and start the Manager Agent
# Supports local (supervisord), cloud (SAE), and K8s (Helm) deployments.
# In local mode this is the last supervisord component to start (priority 800).
# In cloud/k8s mode (AGENTTEAMS_RUNTIME=aliyun|k8s) this is the container entrypoint.
#
# Runtime selection:
#   AGENTTEAMS_MANAGER_RUNTIME=openclaw (default) - OpenClaw gateway mode
#   AGENTTEAMS_MANAGER_RUNTIME=copaw              - CoPaw workspace mode
# (hermes runtime is supported for Workers only; Managers run openclaw or copaw.)

source /opt/hiclaw/scripts/lib/hiclaw-env.sh

_BOOTSTRAP_DIR="/opt/hiclaw/scripts/lib/bootstrap"
for _lib in runtime cloud-validate local secrets workspace matrix-token higress admin-dm \
    openclaw-config cms-plugin container-runtime workers pre-start cloud-sync start-runtime; do
    # shellcheck disable=SC1090
    source "${_BOOTSTRAP_DIR}/${_lib}.sh"
done

bootstrap_select_runtime
bootstrap_set_timezone
bootstrap_promote_yolo
bootstrap_validate_cloud
bootstrap_setup_local
bootstrap_manage_secrets
bootstrap_pull_cloud_workspace
bootstrap_init_workspace
bootstrap_obtain_matrix_token
bootstrap_init_higress
bootstrap_setup_admin_dm
bootstrap_generate_openclaw_config
bootstrap_configure_cms_plugin
bootstrap_detect_container_runtime
bootstrap_manage_workers
bootstrap_pre_start
bootstrap_start_cloud_sync
bootstrap_start_manager_runtime
