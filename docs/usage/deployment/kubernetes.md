# Kubernetes Deployment Guide

English | [中文](../../zh-cn/usage/deployment/kubernetes.md)

This guide explains how to create and maintain an AgentTeams instance on Kubernetes with the official Helm chart. It covers cluster preparation, model services, Manager and Worker runtimes, images, persistence, access, upgrades, and uninstall options.

For a first evaluation, start with the local path in [Quickstart](../../quickstart.md). Kubernetes is better suited to shared teams, long-running environments, and production deployments.

## 1. What the deployment creates

The default profile deploys a self-contained AgentTeams instance:

| Component | Default form | Purpose |
|---|---|---|
| Higress | Helm subchart | Model and API gateway; also routes Web and Matrix traffic |
| Tuwunel | StatefulSet + PVC | Matrix homeserver |
| MinIO | StatefulSet + PVC | Agent configuration, workspaces, and shared file storage |
| Element Web | Deployment | Default Matrix web client |
| AgentTeams Controller | Deployment | Manages Manager, Worker, Team, and Human CRs |
| Manager | Pod created from a `Manager` CR | Receives user goals and orchestrates Workers |
| Worker | Pod created on demand | Executes focused tasks; no Worker is pre-created during installation |

Kubernetes mode does not bundle these components into one container. Infrastructure, the Manager, and every Worker are separate workloads, and the Controller manages their lifecycle through CRDs.

## 2. Prerequisites

Before installing, confirm that you have:

- Kubernetes 1.24 or later.
- Helm 3.7 or later.
- A `kubectl` context that points to the target cluster, with permission to create Namespaces, CRDs, ClusterRoles, StatefulSets, Deployments, Jobs, and Secrets.
- Cluster nodes that can pull the selected images and reach the model service.
- A default StorageClass, or a prepared StorageClass name, when using the default Tuwunel and MinIO deployments.
- A model API key and model name, plus a Base URL when using a custom OpenAI-compatible service.

Check the environment first:

```bash
kubectl version
kubectl cluster-info
kubectl get storageclass
helm version
```

For an initial evaluation, allocate at least 4 CPU cores, 8 GiB of memory, and 20 GiB of dynamically provisioned storage. Actual requirements depend on the number of Workers, their runtimes, and their tasks. Parallel Workers, browser automation, and build workloads generally require more resources.

## 3. Choose a deployment profile

### 3.1 Infrastructure combinations

The current chart supports these combinations:

| Capability | Recommended default | Optional cloud combination |
|---|---|---|
| Matrix | `tuwunel` + `managed` | The current chart still accepts only managed Tuwunel |
| Gateway | `higress` + `managed` | `ai-gateway` + `existing` |
| Storage | `minio` + `managed` | `oss` + `existing` |

Use the fully managed defaults for a first deployment. External AI Gateway or OSS requires an additional `credentialProvider` sidecar that issues scoped temporary credentials. It is not a general external-service mode that works by supplying only an endpoint.

The chart fails during Helm rendering for these combinations:

- A `matrix.provider` other than `tuwunel`, or a `matrix.mode` other than `managed`.
- `gateway.provider=higress` with a mode other than `managed`.
- `gateway.provider=ai-gateway` with a mode other than `existing`.
- `storage.provider=minio` with a mode other than `managed`.
- `storage.provider=oss` with a mode other than `existing`.
- AI Gateway or OSS without an enabled and configured `credentialProvider`.

### 3.2 Access method

Choose `gateway.publicURL` before installation:

| Scenario | Example | Notes |
|---|---|---|
| Temporary local access | `http://localhost:18080` | Used with `kubectl port-forward`; unavailable after the command stops |
| Shared intranet | `https://agentteams.example.internal` | Expose through an internal Ingress or LoadBalancer |
| Internet access | `https://agentteams.example.com` | Requires a trusted TLS certificate and access controls |

This value is written into Element Web and Matrix-related configuration. It must exactly match the origin users open. Plan the hostname and HTTPS configuration before installing a shared production instance, because changing the address later can disrupt client connectivity.

### 3.3 Manager runtime

The user-facing Manager runtime choices are **OpenClaw** and **CoPaw** only:

| Choice | `manager.runtime` | Manager image | Notes |
|---|---|---|---|
| OpenClaw | `openclaw` | `agentteams-manager` | Current chart default |
| CoPaw | `qwenpaw` | `agentteams-manager-qwenpaw` | Current Python Manager implementation; `copaw` is a legacy compatibility alias |

Hermes, OpenHuman, and other Worker runtimes are not Manager runtime choices.

The current chart does not rewrite `manager.image.repository` based only on `manager.runtime`. When choosing CoPaw, set both values:

```yaml
manager:
  runtime: qwenpaw
  image:
    repository: higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-manager-qwenpaw
```

Changing only the runtime while retaining the OpenClaw Manager image can leave the Manager Pod unable to start or running with mismatched behavior.

### 3.4 Default Worker runtime

`worker.defaultRuntime` selects the default runtime for subsequently created Workers. An explicit `spec.runtime` on an individual `Worker` CR overrides it.

| Runtime | Default image value | Notes |
|---|---|---|
| `openclaw` | `worker.defaultImage.openclaw` | Default general-purpose Worker runtime |
| `copaw` | `worker.defaultImage.copaw` | Python / CoPaw Worker |
| `hermes` | `worker.defaultImage.hermes` | Hermes Worker |
| `openhuman` | `worker.defaultImage.openhuman` | The chart has a default image value, but the current Worker CRD enum does not accept an explicit `spec.runtime: openhuman` |

The Controller recognizes the `qwenpaw` Worker runtime, but the current chart does not provide a separate default image value for it. Set the QwenPaw Worker image explicitly in `Worker.spec.image` when using it. The OpenHuman backend and Helm value exist, but the CRD contract is not aligned yet; do not use `openhuman` explicitly in Worker YAML until a separate business-code change resolves it.

## 4. Configure the model service

The Helm chart uses `credentials.*` for the default model service:

| Value | Default | Meaning | Example |
|---|---|---|---|
| `credentials.llmApiKey` | None | Model service API key; required | `sk-...` |
| `credentials.llmProvider` | `openai-compat` | Model service type used by the gateway | `openai-compat`, `qwen` |
| `credentials.defaultModel` | `gpt-5.4` | Model used by the Manager and Workers without an explicit model | `gpt-5.4`, `qwen3.5-plus` |
| `credentials.llmBaseUrl` | Empty | Base URL for an OpenAI-compatible API; leave empty for official OpenAI or a provider default | `https://api.deepseek.com/v1` |

Set `llmBaseUrl` to the API root, not a specific `/chat/completions` endpoint. The model name must be an identifier accepted by that service.

### Official OpenAI API

```yaml
credentials:
  llmApiKey: "<your-openai-api-key>"
  llmProvider: openai-compat
  defaultModel: gpt-5.4
  llmBaseUrl: ""
```

### Custom OpenAI-compatible service

```yaml
credentials:
  llmApiKey: "<your-provider-api-key>"
  llmProvider: openai-compat
  defaultModel: your-model-name
  llmBaseUrl: https://your-provider.example.com/v1
```

### Qwen

```yaml
credentials:
  llmApiKey: "<your-qwen-api-key>"
  llmProvider: qwen
  defaultModel: qwen3.5-plus
```

Before installation and upgrades, the chart creates a temporary Job that probes the model service by default:

```yaml
preflight:
  llm:
    enabled: true
    strict: true
    timeoutSeconds: 30
    retries: 2
    activeDeadlineSeconds: 120
```

- `enabled`: whether to run the probe.
- `strict`: whether a failed probe blocks the installation or upgrade.
- `timeoutSeconds`: timeout for each request.
- `retries`: retries for network errors, rate limits, and server errors.
- `activeDeadlineSeconds`: maximum runtime for the complete probe Job.

Keep strict probing enabled in production. Temporarily set `strict: false` or disable the probe only when the cluster cannot yet reach the model service and network setup will be completed after installation.

## 5. Write a values file

A values file is easier to review, reuse, and upgrade than a long list of `--set` arguments. This example uses managed infrastructure, an OpenClaw Manager, and OpenClaw Workers:

```yaml
# agentteams-values.yaml
credentials:
  llmApiKey: "<replace-with-your-api-key>"
  llmProvider: openai-compat
  defaultModel: gpt-5.4
  llmBaseUrl: ""
  adminUser: admin
  adminPassword: "<replace-with-a-strong-password>"

gateway:
  provider: higress
  mode: managed
  publicURL: http://localhost:18080
  higress:
    enabled: true

matrix:
  provider: tuwunel
  mode: managed
  tuwunel:
    persistence:
      enabled: true
      size: 10Gi
      storageClassName: ""

storage:
  provider: minio
  mode: managed
  bucket: agentteams-storage
  minio:
    persistence:
      enabled: true
      size: 10Gi
      storageClassName: ""
    auth:
      rootUser: minioadmin
      rootPassword: "<replace-with-a-strong-password>"

manager:
  enabled: true
  runtime: openclaw
  image:
    repository: higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/agentteams-manager
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: "2"
      memory: 4Gi

worker:
  defaultRuntime: openclaw
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
    limits:
      cpu: "2"
      memory: 2Gi
```

Restrict a file containing secrets to the current user:

```bash
chmod 600 agentteams-values.yaml
```

Do not commit real secrets to Git. Production environments can use a Secret management tool to generate or inject values. Regardless of the mechanism, `credentials.llmApiKey` must be available when Helm renders the release.

When `credentials.adminPassword` is empty, the chart generates a password and reuses the existing Secret value on later upgrades. Explicitly managing a strong password is preferable in production.

## 6. Install AgentTeams

### 6.1 Use the official Helm repository

```bash
helm repo add higress.io https://higress.io/helm-charts
helm repo update
helm show values higress.io/agentteams
```

Install or idempotently update the instance:

```bash
helm upgrade --install agentteams higress.io/agentteams \
  --namespace agentteams-system \
  --create-namespace \
  --values agentteams-values.yaml \
  --render-subchart-notes \
  --wait \
  --timeout 15m
```

`agentteams` is the Helm release name and `agentteams-system` is the Namespace. You can change them, but all later commands must use the same names.

### 6.2 Install the current chart from source

To test an unpublished chart from this repository, run these commands from the project root:

```bash
helm dependency build helm/agentteams
helm upgrade --install agentteams ./helm/agentteams \
  --namespace agentteams-system \
  --create-namespace \
  --values agentteams-values.yaml \
  --set global.imageTag=latest \
  --render-subchart-notes \
  --wait \
  --timeout 15m
```

By default, the chart converts `appVersion` into an image tag with a `v` prefix; the repository's current `appVersion: 1.1.1` resolves to `v1.1.1`. Because that value may differ from the latest AgentTeams release, use an explicit `global.imageTag=latest` as shown above when validating the current source tree. In production, set a verified fixed tag that matches the intended deployment version instead of relying on the chart's implicit default or a mutable tag.

Inspect the resolved images before installing:

```bash
helm template agentteams ./helm/agentteams \
  --namespace agentteams-system \
  --values agentteams-values.yaml \
  --set global.imageTag=latest \
  | grep 'image:' | sort -u
```

## 7. Verify the installation

Do not treat a successful `helm install` return value as the complete acceptance test. Verify the model preflight, infrastructure, Manager, Worker, and Matrix message path in order.

### 7.1 Verify Helm, Pods, and storage

```bash
helm status agentteams -n agentteams-system
kubectl get pods -n agentteams-system
kubectl get pvc -n agentteams-system
kubectl wait --for=condition=Ready pod --all \
  -n agentteams-system \
  --timeout=15m
```

Expect the Helm status to be `deployed`, all infrastructure Pods to be `Running` and Ready, and the Tuwunel and MinIO PVCs to be `Bound`. Workers are created on demand, so having no Worker Pod immediately after installation is expected.

### 7.2 Verify the model preflight

The default LLM preflight is a Helm pre-install/pre-upgrade hook. In strict mode, Helm proceeds with the main installation only when the API key, Base URL, provider, and model name are usable.

A successful hook Job is deleted automatically, so it may no longer appear in `kubectl get job` after installation. Add `--debug` to the Helm command when you need diagnostic output, and observe the hook while it runs:

```bash
kubectl get job,pod -n agentteams-system \
  -l app.kubernetes.io/component=llm-preflight
```

If installation remains at the preflight stage, use another terminal to find the actual Job or Pod name and read its logs:

```bash
kubectl get job,pod -n agentteams-system
kubectl logs -n agentteams-system job/agentteams-llm-preflight
```

The Job name also changes when the release name is not `agentteams`.

### 7.3 Verify the Manager

The default Manager is created by the Controller, not by a static Helm Deployment. Inspect the CR and its Pod:

```bash
kubectl get managers.agentteams.io -n agentteams-system
kubectl describe manager default -n agentteams-system
kubectl get pods -n agentteams-system -l agentteams.io/role=manager
```

If initialization is slow, inspect the Controller logs:

```bash
kubectl logs -n agentteams-system deployment/agentteams-controller --tail=200
```

The Controller Deployment name usually changes when the release name is not `agentteams`. Run `kubectl get deployment -n agentteams-system` first to find the actual name.

The Manager CR `PHASE` should be `Running`. Once the Manager Pod is Ready, also log in to Element Web and send the Manager a short message. Confirm that it can call the model and return content; Pod readiness alone does not prove that inference works.

### 7.4 Create a Worker and close the message loop

Log in to Element Web and ask the Manager to create an acceptance-test Worker:

```text
Create a Worker named e2e-worker with the default model and the openclaw runtime. Report its state briefly when it is ready.
```

Confirm that the CR and Pod are ready:

```bash
kubectl get workers.agentteams.io -n agentteams-system
kubectl get pods -n agentteams-system -l agentteams.io/role=standalone
```

Then ask the Manager to assign a minimal task:

```text
In e2e-worker's Worker Room, @mention it and ask it to reply only with K8S_E2E_OK. Report back after you receive the result.
```

Worker group rooms enable `requireMention` by default. The Matrix event must contain `m.mentions` for the Worker's complete Matrix ID. A manually typed `@e2e-worker` without the full Matrix domain, or a client that does not generate mention metadata, is ignored. Assigning the task through the Manager, or selecting the Worker from the member list when mentioning it in Element, avoids sending an ordinary text message by mistake.

To confirm from logs whether the message entered the model path, run:

```bash
kubectl logs -n agentteams-system \
  -l agentteams.io/role=standalone \
  --tail=200 \
  | grep -E 'resolveAgentRoute|embedded run start|model='
```

`embedded run start` together with the expected model name confirms that the Matrix → Worker → model path started. A complete acceptance test still requires the Worker to post its result in the room and the Manager to receive and report it.

## 8. Access Element Web

### 8.1 Temporary port-forward

When `gateway.publicURL` is `http://localhost:18080`, run:

```bash
kubectl port-forward -n agentteams-system svc/higress-gateway 18080:80
```

Open `http://localhost:18080`. The default username comes from `credentials.adminUser` and is `admin`.

If you did not set an administrator password during installation, read the generated value:

```bash
kubectl get secret agentteams-runtime-env \
  -n agentteams-system \
  -o go-template='{{index .data "AGENTTEAMS_ADMIN_PASSWORD" | base64decode}}{{"\n"}}'
```

The Secret name depends on the release name. If it is not found, list the Secrets first:

```bash
kubectl get secret -n agentteams-system
```

### 8.2 Shared access through an Ingress

Use HTTPS in production. This example assumes an NGINX Ingress and an existing `agentteams-tls` TLS Secret:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: agentteams
  namespace: agentteams-system
spec:
  ingressClassName: nginx
  tls:
    - hosts:
        - agentteams.example.com
      secretName: agentteams-tls
  rules:
    - host: agentteams.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: higress-gateway
                port:
                  number: 80
```

Set the same origin in the values file:

```yaml
gateway:
  publicURL: https://agentteams.example.com
```

After updating the release, verify the web entry point and Matrix routing:

```bash
curl -fsSI https://agentteams.example.com/
curl -fsS https://agentteams.example.com/_matrix/client/versions
```

Expose only the Higress Gateway. Keep the Controller API, Tuwunel, MinIO, and Higress Console private by default. If you must expose them, add separate authentication, TLS, and network policies.

## 9. Create the first Worker

The recommended path is to log in to Element Web and ask the Manager directly:

```text
Create a development Worker named alice with the default model and runtime.
```

The Manager asks the Controller to create the `Worker` CR, Matrix identity, permissions, and Pod. Check its state:

```bash
kubectl get workers.agentteams.io -n agentteams-system
kubectl get pods -n agentteams-system -l agentteams.io/role=standalone
```

After learning the CRDs, you can also apply YAML directly:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
  namespace: agentteams-system
spec:
  model: gpt-5.4
  runtime: openclaw
  identity: Software engineer responsible for implementation, tests, and code review
```

```bash
kubectl apply -f worker-alice.yaml
```

See [Declarative Resource Management](../resource-management.md) and the [Worker Guide](../worker-guide.md) for more fields.

## 10. Common configuration options

### Images and versions

| Value | Purpose |
|---|---|
| `global.imageTag` | Shared version for AgentTeams components without an explicit tag; defaults to the chart `appVersion` |
| `controller.image.*` | Controller image repository, tag, and pull policy |
| `manager.image.*` | Manager image repository and tag |
| `worker.defaultImage.<runtime>.*` | Default image for each Worker runtime |
| `imagePullSecrets` | Pull credentials for private image registries |

`global.imageRegistry` is passed to subcharts that consume it, but the current Controller, Manager, and Worker values use complete `repository` paths. When switching regions or using a private registry, inspect and override every relevant `*.image.repository`; do not assume that one global value rewrites all image addresses.

When `global.imageTag` is empty, it resolves to `v<Chart.appVersion>`. That tag must exist in the Controller, Manager, and default Worker repositories. The LLM preflight also reuses the Controller image, so a missing Controller tag appears as a preflight `ImagePullBackOff` before the main workloads are created. Source-tree validation can temporarily use `latest`; production environments should pin a published and verified version.

### Persistence

| Value | Default | Meaning |
|---|---|---|
| `matrix.tuwunel.persistence.enabled` | `true` | Persist Matrix data |
| `matrix.tuwunel.persistence.size` | `10Gi` | Tuwunel PVC capacity |
| `matrix.tuwunel.persistence.storageClassName` | Empty | Use the default StorageClass when empty |
| `storage.minio.persistence.enabled` | `true` | Persist MinIO data |
| `storage.minio.persistence.size` | `10Gi` | MinIO PVC capacity |
| `storage.minio.persistence.storageClassName` | Empty | Use the default StorageClass when empty |

Do not disable persistence in production. Before upgrading, confirm StorageClass expansion support and a backup plan. Deleting a Namespace commonly deletes its PVCs as well.

### Resources

Resources can be set independently through:

- `controller.resources`
- `manager.resources`
- `worker.resources`
- `matrix.tuwunel.resources`
- `storage.minio.resources`
- `elementWeb.resources`
- `credentialProvider.resources`
- `preflight.llm.resources`

`worker.resources` is the default when a `Worker.spec.resources` value is not set. Increase CPU and memory for Workers that run builds, large repositories, or browser workloads.

### Controller and monitoring

- `controller.replicaCount`: Controller replicas; multiple replicas rely on leader election for consistent reconciliation.
- `controller.metrics.enabled`: expose the metrics Service.
- `controller.metrics.serviceMonitor.enabled`: create a ServiceMonitor when Prometheus Operator is installed.
- `cms.enabled`: enable Alibaba Cloud CMS/ARMS observability; endpoint, license key, project, and workspace are also required.
- `elementWeb.enabled`: deploy the default web client. If disabled, provide another Matrix client entry point.

## 11. External AI Gateway and OSS

This profile is intended for environments that already have Alibaba Cloud AI Gateway, OSS, and an enterprise credential-issuing service. Its minimum structure is:

```yaml
gateway:
  provider: ai-gateway
  mode: existing
  publicURL: https://agentteams.example.com
  higress:
    enabled: false
  aiGateway:
    region: cn-hangzhou
    gatewayId: "<gateway-id>"
    modelApiId: "<model-api-id>"
    envId: "<environment-id>"

storage:
  provider: oss
  mode: existing
  bucket: agentteams-storage
  oss:
    region: cn-hangzhou
    endpoint: ""

credentialProvider:
  enabled: true
  image:
    repository: registry.example.com/agentteams/credential-provider
    tag: latest
  envFrom:
    - secretRef:
        name: credential-provider-config
```

There is no generic default `credentialProvider` image. It must implement the temporary credential API expected by AgentTeams and be configured with your organization's RAM roles, permission boundaries, and identity source. Continue using managed Higress and MinIO if this service is unavailable.

## 12. Upgrade

Review the current values and continue using the same values file:

```bash
helm repo update
helm get values agentteams -n agentteams-system
helm upgrade agentteams higress.io/agentteams \
  --namespace agentteams-system \
  --values agentteams-values.yaml \
  --render-subchart-notes \
  --wait \
  --timeout 15m
```

Do not rely only on temporary `--set` history. Back up Matrix and object storage data before upgrading, and review compatibility for new chart values, CRDs, and images.

Changing `manager.runtime`, `manager.image`, or other critical Manager configuration can recreate the Manager Pod. Make these changes only when no tasks are running.

## 13. Uninstall

Uninstalling stops AgentTeams and may clean up Managers, Workers, Matrix users, and Agent data in object storage. Back up data and confirm your retention plan for PVCs, object storage, and CRs first.

Normal uninstall:

```bash
helm uninstall agentteams -n agentteams-system
```

By default, a pre-delete hook removes Manager, Worker, Team, and Human CRs and waits for the Controller to process their finalizers before removing the Controller. Do not casually use `--no-hooks`.

Delete the Namespace only after confirming that its Secrets, PVCs, and other resources are no longer needed:

```bash
kubectl delete namespace agentteams-system
```

Helm does not delete the CRDs automatically. Run the following only after confirming that no other AgentTeams release or custom resource in the cluster needs them:

```bash
kubectl delete crd \
  managers.agentteams.io \
  workers.agentteams.io \
  teams.agentteams.io \
  humans.agentteams.io
```

## 14. Troubleshooting

### Installation is stuck at LLM preflight

Check the API key, provider, model name, and Base URL, and confirm that cluster Pods can reach the model service. Helm may quickly clean up a failed hook Job. Use `helm install --debug` to inspect the output, or temporarily set `preflight.llm.strict=false` to collect more runtime logs.

### A Pod remains `Pending`

```bash
kubectl describe pod <pod-name> -n agentteams-system
kubectl get events -n agentteams-system --sort-by=.lastTimestamp
kubectl get pvc -n agentteams-system
```

Check node resources, the default StorageClass, PVC binding, taints, and scheduling constraints.

### A Pod is in `ImagePullBackOff`

Confirm that nodes can reach the registry, the image tag exists, the CPU architecture matches, and private registries have `imagePullSecrets`. When switching to the CoPaw Manager, verify that both the runtime and Manager image match.

If a source installation reports that an image such as `agentteams-controller:v<Chart.appVersion>` does not exist, inspect the rendered images and temporarily override the tag with one that exists:

```bash
helm template agentteams ./helm/agentteams \
  -n agentteams-system \
  -f agentteams-values.yaml \
  | grep 'image:' | sort -u

helm upgrade --install agentteams ./helm/agentteams \
  -n agentteams-system \
  --create-namespace \
  -f agentteams-values.yaml \
  --set global.imageTag=latest \
  --wait \
  --timeout 15m
```

Replace `latest` with a fixed version after validation succeeds.

### Helm prints a Higress values type warning

The current dependency combination can print a warning similar to `destination for higress-core.controller.image is a table`. If the Helm release reaches `deployed` and the Higress Controller and Gateway Pods are Ready, this warning does not block the deployment by itself. Record it and recheck values compatibility when upgrading the Higress subchart.

### Element Web opens but cannot log in or connect to Matrix

Confirm that `gateway.publicURL` matches the browser origin and that `/_matrix/client/versions` works. For an Ingress, also check WebSocket support, long-lived connections, request-body limits, and timeouts.

### The Manager CR exists but its Pod is not ready

```bash
kubectl describe manager default -n agentteams-system
kubectl get events -n agentteams-system --sort-by=.lastTimestamp
kubectl logs -n agentteams-system deployment/agentteams-controller --tail=200
```

If the Manager Pod exists, inspect its describe output and logs. Common causes include a model service that remains inaccessible after a non-strict preflight, a runtime/image mismatch, insufficient resources, or initialization dependencies that are not ready.

### A Running Worker does not reply in a group room

First confirm that the message actually mentions the Worker instead of containing ordinary text that only looks like a mention. A full Matrix ID looks like `@e2e-worker:<matrix-domain>`, and the event must contain the same user in `m.mentions.user_ids`. Select the Worker from the member list in Element, or ask the Manager to assign the task with its built-in Matrix message tool.

Then inspect the Worker logs:

```bash
kubectl logs -n agentteams-system \
  -l agentteams.io/role=standalone \
  --tail=200 \
  | grep -E 'matrix-auto-reply|resolveAgentRoute|embedded run start|error'
```

- `matrix-auto-reply skipping room message` usually means that the event has no valid mention or that the sender is not allowed.
- `resolveAgentRoute` without `embedded run start` means you should inspect the full Matrix ID, `m.mentions`, and `groupAllowFrom`.
- `embedded run start` means the message reached the model path; continue with model errors, tool calls, and Matrix send logs.

See the [FAQ](../troubleshooting/faq.md) for more help.
