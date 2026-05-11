# Integrating HiClaw with Alibaba Cloud CMS 2.0

This guide explains how to integrate HiClaw with Alibaba Cloud Monitor Service (CMS) 2.0 for comprehensive observability of your AI Agent applications.

## Version Requirements

**⚠️ Important: CMS 2.0 observability support is available starting from HiClaw v1.0.9.**

## Overview

HiClaw supports integration with CMS 2.0 through OpenTelemetry protocol, enabling you to:

- Monitor complete request traces across Manager and Worker Agents
- Track LLM API call latency and token consumption
- Visualize Agent collaboration patterns
- Analyze task execution paths and bottlenecks

## Prerequisites

- HiClaw v1.0.9 or higher
- Alibaba Cloud account with CMS 2.0 service enabled
- CMS 2.0 workspace created
- Network connectivity from HiClaw deployment to CMS 2.0 endpoints

## Step 1: Obtain CMS 2.0 Integration Configuration

Before configuring HiClaw, you need to obtain the integration parameters from the CMS 2.0 console.

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
   - **Public Endpoint**: For HiClaw deployed in public cloud or requires internet access
   - **VPC Endpoint**: For HiClaw deployed within Alibaba Cloud VPC (recommended for better security and lower latency)
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
| `serviceName`          | Application name (displayed in CMS 2.0)           | `hiclaw-manager`                                                                                                     |

**Important Notes:**
- The endpoint URL automatically routes to either trace or metrics collection based on the path suffix
- For VPC connections, ensure your HiClaw deployment has network access to the VPC endpoint
- The license key is sensitive information - store it securely

## Step 2: Configure HiClaw Manager

Add the following environment variables to your HiClaw Manager container, then restart it:

```bash
# Enable trace collection
HICLAW_CMS_TRACES_ENABLED=true

# CMS 2.0 workspace identifier (from x-cms-workspace)
HICLAW_CMS_WORKSPACE=default-cms-xxx-cn-hangzhou

# Service name for Manager (from serviceName)
HICLAW_CMS_SERVICE_NAME=hiclaw-manager

# Log Service project name (from x-arms-project)
HICLAW_CMS_PROJECT=proj-xtrace-xxx-cn-hangzhou

# Authentication license key (from x-arms-license-key)
HICLAW_CMS_LICENSE_KEY=d95vgxi0cn@xxxxx

# OTLP endpoint URL (from endpoint)
HICLAW_CMS_ENDPOINT=https://proj-xtrace-xxx.cn-hangzhou.log.aliyuncs.com/apm/trace/opentelemetry
```

After configuration, restart the container: `docker restart hiclaw-manager`

### v1.1.0+ multi-container / Helm placement

| Where | What to configure |
|-------|-------------------|
| **Embedded Docker** | Put all `HICLAW_CMS_*` variables on **`hiclaw-manager`** (or `hiclaw-manager-copaw` if you use QwenPaw Manager). They are loaded from your `hiclaw-manager.env` at container start. **`hiclaw-controller`** does not need CMS env for normal agent tracing. |
| **Helm / Kubernetes** | Set the same keys on the **Manager** Deployment (see `helm/hiclaw/values.yaml` → manager env). Optionally seed worker defaults there so every Worker Pod receives CMS settings even if Manager was configured late. |

## Step 3: Configure HiClaw Workers

Manager will automatically propagate CMS configuration to newly created Workers. Ensure you configure Manager before creating Workers.

## Step 4: Verify Integration

1. Login to Element Web, interact with Manager and create a Worker to execute tasks
2. Visit [CMS 2.0 Console](https://cmsnext.console.aliyun.com/next/home) → Select workspace → **Application Observability** → **AI Application Observability**
3. You should see `hiclaw-manager` and `hiclaw-worker-*` in the application list
4. Click on application name to view trace analysis, metrics, and other details

## Troubleshooting

### No data in CMS 2.0?

1. Check network connectivity: `curl -I <ENDPOINT_URL>`
2. Verify `HICLAW_CMS_LICENSE_KEY` is correct (no spaces/newlines)
3. Confirm container restarted: `docker restart hiclaw-manager`
4. Check container logs: `docker logs hiclaw-manager`
5. OTLP data is batched, may have up to 60s delay

### Workers not sending telemetry?

1. Confirm Manager has CMS environment variables configured before creating Workers
2. Verify Worker container's `HICLAW_CMS_*` environment variables
3. Workers created before Manager configuration need to be deleted and recreated