# Integrating AgentTeams with Alibaba Cloud CMS 2.0

This guide explains how to integrate AgentTeams with Alibaba Cloud Monitor Service (CMS) 2.0 for comprehensive observability of your AI Agent applications.

For Core v1.2.7 + Dashboard v1.4.1, see [Dashboard Tracing](../dashboard.md#optional-tracing) for Controller-managed Collector configuration and Langfuse/AgentLoop queries. The CMS environment settings below describe the separate CMS export integration; they do not configure Dashboard's query backend.

## Version Requirements

**⚠️ Important: CMS 2.0 observability support is available starting from AgentTeams v1.0.9.**

## Overview

AgentTeams supports integration with CMS 2.0 through OpenTelemetry protocol, enabling you to:

- Monitor complete request traces across Manager and Worker Agents
- Track LLM API call latency and token consumption
- Visualize Agent collaboration patterns
- Analyze task execution paths and bottlenecks

## Prerequisites

- AgentTeams v1.0.9 or higher
- Alibaba Cloud account with CMS 2.0 service enabled
- CMS 2.0 workspace created
- Network connectivity from AgentTeams deployment to CMS 2.0 endpoints

## Step 1: Obtain CMS 2.0 Integration Configuration

Before configuring AgentTeams, you need to obtain the integration parameters from the CMS 2.0 console.

### 1.1 Login to CMS 2.0 Console

1. Navigate to [CMS 2.0 Console](https://cmsnext.console.aliyun.com/next/home)
2. Select your target workspace

### 1.2 Access Integration Center

1. In the left navigation bar, click **Integration Center**
2. In the **AI Application Observability** section, locate the **OpenClaw** card
3. Click on the **OpenClaw** card to open the integration configuration page

### 1.3 Generate Integration Command

1. In the parameter configuration area, enter your **application name** (this will be the service name displayed in CMS 2.0)
2. Select the **connection method**:
   - **Public Endpoint**: For AgentTeams deployed in public cloud or requires internet access
   - **VPC Endpoint**: For AgentTeams deployed within Alibaba Cloud VPC (recommended for better security and lower latency)
3. Click **Get** next to the LicenseKey field to generate authentication credentials
4. The page will generate the complete integration command based on your configuration

### 1.4 Record Integration Parameters

Note down the following parameters from the generated configuration:

| **Parameter**          | **Description**                                    | **Example**                                                                                                          |
|------------------------|---------------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `endpoint`             | OTLP Trace/Metric data reporting endpoint         | `https://proj-xtrace-xxx.cn-hangzhou.log.aliyuncs.com/apm/trace/opentelemetry`                                      |
| `x-arms-license-key`   | License key for data write authentication        | `d95vgxi0cn@xxxxx`                                                                                                   |
| `x-arms-project`       | Log Service project name                          | `proj-xtrace-xxx-cn-hangzhou`                                                                                        |
| `x-cms-workspace`      | CMS 2.0 workspace identifier                      | `default-cms-xxx-cn-hangzhou`                                                                                        |
| `serviceName`          | Application name (displayed in CMS 2.0)           | `agentteams-manager`                                                                                                     |

**Important Notes:**
- The endpoint URL automatically routes to either trace or metrics collection based on the path suffix
- For VPC connections, ensure your AgentTeams deployment has network access to the VPC endpoint
- The license key is sensitive information - store it securely

## Step 2: Configure CMS export defaults

Configure the following environment variables on the Controller deployment so it can project CMS defaults into managed Agent containers:

```bash
# Enable trace collection
AGENTTEAMS_CMS_TRACES_ENABLED=true

# CMS 2.0 workspace identifier (from x-cms-workspace)
AGENTTEAMS_CMS_WORKSPACE=default-cms-xxx-cn-hangzhou

# Service name for Manager (from serviceName)
AGENTTEAMS_CMS_SERVICE_NAME=agentteams-manager

# Log Service project name (from x-arms-project)
AGENTTEAMS_CMS_PROJECT=proj-xtrace-xxx-cn-hangzhou

# Authentication license key (from x-arms-license-key)
AGENTTEAMS_CMS_LICENSE_KEY=d95vgxi0cn@xxxxx

# OTLP endpoint URL (from endpoint)
AGENTTEAMS_CMS_ENDPOINT=https://proj-xtrace-xxx.cn-hangzhou.log.aliyuncs.com/apm/trace/opentelemetry
```

Apply the updated deployment configuration and recreate containers through the normal deployment workflow. `docker restart` alone does not load changed environment variables from a host file.

### v1.1.0+ multi-container / Helm placement

| Where | What to configure |
|-------|-------------------|
| **Embedded Docker** | Persist the `AGENTTEAMS_CMS_*` settings in the installer environment configuration and apply through the installer. It passes them to `agentteams-controller`, which supplies managed Agent defaults. |
| **Helm / Kubernetes** | Set `cms.enabled`, `cms.endpoint`, `cms.licenseKey`, `cms.project`, `cms.workspace`, `cms.serviceName`, and optional `cms.metricsEnabled` in chart values. The Controller Deployment receives these settings; the Manager is a CR-managed Pod, not a separate Helm Deployment. |

## Step 3: Configure AgentTeams Workers

The Controller projects configured CMS defaults into new managed Agent containers. Existing containers need their effective environment updated and recreation while idle. Verify the selected Harness supports the CMS exporter; the generic QwenPaw/Hermes Collector flow is configured separately through Dashboard.

## Step 4: Verify Integration

1. Login to Element Web, interact with Manager and create a Worker to execute tasks
2. Visit [CMS 2.0 Console](https://cmsnext.console.aliyun.com/next/home) → Select workspace → **Application Observability** → **AI Application Observability**
3. You should see `agentteams-manager` and `agentteams-worker-*` in the application list
4. Click on application name to view trace analysis, metrics, and other details

## Troubleshooting

### No data in CMS 2.0?

1. Check network connectivity: `curl -I <ENDPOINT_URL>`
2. Verify `AGENTTEAMS_CMS_LICENSE_KEY` is correct (no spaces/newlines)
3. Confirm the container was recreated with the updated environment; restarting an unchanged container does not add variables
4. Check container logs: `docker logs agentteams-manager`
5. OTLP data is batched, may have up to 60s delay

### Workers not sending telemetry?

1. Confirm the Controller has the intended CMS defaults and the selected Harness supports them
2. Verify Worker container's `AGENTTEAMS_CMS_*` environment variables
3. Update the effective Worker environment and recreate the managed container while idle; do not delete the Worker resource merely to refresh telemetry
