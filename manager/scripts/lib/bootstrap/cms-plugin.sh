#!/bin/bash
# bootstrap/cms-plugin.sh - OpenClaw CMS plugin (skipped for CoPaw Manager)

bootstrap_configure_cms_plugin() {
    if [ "${MANAGER_RUNTIME}" != "openclaw" ]; then
        log "CoPaw Manager runtime: skipping openclaw-cms-plugin config"
        return 0
    fi
# Optional: enable openclaw-cms-plugin observability
# Config is applied at runtime so secrets stay out of image layers.
# ============================================================
CMS_TRACES_ENABLED="$(echo "${AGENTTEAMS_CMS_TRACES_ENABLED:-false}" | tr '[:upper:]' '[:lower:]')"
if [ "${CMS_TRACES_ENABLED}" = "true" ]; then
    CMS_PLUGIN_NAME="openclaw-cms-plugin"
    CMS_PLUGIN_DIR="${OPENCLAW_CMS_PLUGIN_DIR:-/opt/openclaw/extensions/openclaw-cms-plugin}"
    CMS_PLUGIN_MANIFEST="${CMS_PLUGIN_DIR}/openclaw.plugin.json"
    DIAG_PLUGIN_NAME="diagnostics-otel"
    DIAG_PLUGIN_DIR="/opt/openclaw/extensions/diagnostics-otel"
    CMS_LICENSE_KEY="${AGENTTEAMS_CMS_LICENSE_KEY:-}"
    CMS_PROJECT="${AGENTTEAMS_CMS_PROJECT:-}"
    CMS_METRICS_ENABLED="${AGENTTEAMS_CMS_METRICS_ENABLED:-false}"

    if [ ! -f "${CMS_PLUGIN_MANIFEST}" ]; then
        log "WARNING: ${CMS_PLUGIN_NAME} manifest not found at ${CMS_PLUGIN_MANIFEST}, skipping plugin config"
    else
        _missing=0
        [ -z "${AGENTTEAMS_CMS_ENDPOINT:-}" ] && log "WARNING: AGENTTEAMS_CMS_ENDPOINT is required when AGENTTEAMS_CMS_TRACES_ENABLED=true" && _missing=1
        [ -z "${CMS_LICENSE_KEY:-}" ] && log "WARNING: AGENTTEAMS_CMS_LICENSE_KEY is required when AGENTTEAMS_CMS_TRACES_ENABLED=true" && _missing=1
        [ -z "${AGENTTEAMS_CMS_WORKSPACE:-}" ] && log "WARNING: AGENTTEAMS_CMS_WORKSPACE is required when AGENTTEAMS_CMS_TRACES_ENABLED=true" && _missing=1

        if [ "${_missing}" = "0" ]; then
            CMS_SERVICE_NAME="${AGENTTEAMS_CMS_SERVICE_NAME:-agentteams-manager}"
            CMS_ENABLE_METRICS="${CMS_METRICS_ENABLED}"
            DIAG_AVAILABLE="0"
            _metrics_lc="$(echo "${CMS_ENABLE_METRICS}" | tr '[:upper:]' '[:lower:]')"
            if [ "${_metrics_lc}" = "true" ]; then
                if [ -f "${DIAG_PLUGIN_DIR}/package.json" ]; then
                    DIAG_AVAILABLE="1"
                    if [ ! -d "${DIAG_PLUGIN_DIR}/node_modules" ]; then
                        log "diagnostics-otel dependencies missing, installing..."
                        if (cd "${DIAG_PLUGIN_DIR}" && npm install --omit=dev --ignore-scripts >/tmp/hiclaw-diag-install.log 2>&1); then
                            log "diagnostics-otel dependencies installed"
                        else
                            log "WARNING: diagnostics-otel npm install failed, metrics plugin may not load"
                        fi
                    else
                        log "diagnostics-otel dependencies already present"
                    fi
                else
                    log "WARNING: diagnostics-otel package.json not found at ${DIAG_PLUGIN_DIR}, metrics plugin may not load"
                fi
            fi

            log "Applying ${CMS_PLUGIN_NAME} config to openclaw.json..."
            jq --arg pluginName "${CMS_PLUGIN_NAME}" \
               --arg pluginDir "${CMS_PLUGIN_DIR}" \
               --arg endpoint "${AGENTTEAMS_CMS_ENDPOINT}" \
               --arg licenseKey "${CMS_LICENSE_KEY}" \
               --arg armsProject "${CMS_PROJECT}" \
               --arg cmsWorkspace "${AGENTTEAMS_CMS_WORKSPACE}" \
               --arg serviceName "${CMS_SERVICE_NAME}" \
               --arg diagPluginName "${DIAG_PLUGIN_NAME}" \
               --arg diagPluginDir "${DIAG_PLUGIN_DIR}" \
               --arg metricsRaw "${CMS_ENABLE_METRICS}" \
               --arg diagAvailableRaw "${DIAG_AVAILABLE}" \
               '
                .plugins = (.plugins // {})
                | .plugins.load = (.plugins.load // {})
                | .plugins.entries = (.plugins.entries // {})
                | if (.plugins.allow | type) != "array" then .plugins.allow = [] else . end
                | if (.plugins.allow | index($pluginName)) == null then .plugins.allow += [$pluginName] else . end
                | if (.plugins.load.paths | type) != "array" then .plugins.load.paths = [] else . end
                | if (.plugins.load.paths | index($pluginDir)) == null then .plugins.load.paths += [$pluginDir] else . end
                | .plugins.entries[$pluginName] = {
                    "enabled": true,
                    "config": {
                        "endpoint": $endpoint,
                        "headers": {
                            "x-arms-license-key": $licenseKey,
                            "x-arms-project": $armsProject,
                            "x-cms-workspace": $cmsWorkspace
                        },
                        "serviceName": $serviceName
                    }
                }

                # diagnostics-otel metrics (optional)
                | ($metricsRaw | ascii_downcase) as $m
                | ($diagAvailableRaw == "1") as $diagAvailable
                | (($m == "true") and $diagAvailable) as $metricsEnabled
                | if $metricsEnabled then
                    (if (.plugins.allow | index($diagPluginName)) == null then .plugins.allow += [$diagPluginName] else . end)
                    | (if (.plugins.load.paths | index($diagPluginDir)) == null then .plugins.load.paths += [$diagPluginDir] else . end)
                    | .plugins.entries[$diagPluginName].enabled = true
                    | .diagnostics = (.diagnostics // {})
                    | .diagnostics.otel = (.diagnostics.otel // {})
                    | .diagnostics.enabled = true
                    | .diagnostics.otel.enabled = true
                    | .diagnostics.otel.endpoint = $endpoint
                    | .diagnostics.otel.protocol = (.diagnostics.otel.protocol // "http/protobuf")
                    | .diagnostics.otel.headers = {
                        "x-arms-license-key": $licenseKey,
                        "x-arms-project": $armsProject,
                        "x-cms-workspace": $cmsWorkspace
                    }
                    | .diagnostics.otel.serviceName = $serviceName
                    | .diagnostics.otel.metrics = true
                    | .diagnostics.otel.traces = (.diagnostics.otel.traces // false)
                    | .diagnostics.otel.logs = (.diagnostics.otel.logs // false)
                  else
                    .
                  end
               ' /root/manager-workspace/openclaw.json > /tmp/openclaw-cms.json && \
                mv /tmp/openclaw-cms.json /root/manager-workspace/openclaw.json
            log "${CMS_PLUGIN_NAME} config applied (metrics=${CMS_ENABLE_METRICS}, service=${CMS_SERVICE_NAME})"
        else
            log "Skipping ${CMS_PLUGIN_NAME} config due to missing required env vars"
        fi
    fi
fi

# ============================================================
}
