# Agent Pod Template

The agentteams-controller creates one Kubernetes Pod per Manager and Worker
("agent Pods"). By default, these Pods have a minimal shape: one container
named `worker`, a projected `agentteams-token` volume, the controller-managed
`ServiceAccount`, and not much else.

To inject cluster-specific concerns — sysctls, nodeSelectors, tolerations,
imagePullSecrets, annotations consumed by CNI/sidecar injectors, etc. —
you provide a `corev1.PodTemplateSpec` overlay via a ConfigMap.

## How it works

On **every** `Create()`, the controller reads a ConfigMap from its own
namespace with the name equal to its `AGENTTEAMS_CONTROLLER_NAME` environment
variable (for a Helm release `prod`, that is `prod-agentteams-controller`).
If the ConfigMap is present and has a key `pod-template.yaml`, its value is
parsed as a `PodTemplateSpec` and merged with the controller-owned fields
to produce the final Pod.

> `AGENTTEAMS_CONTROLLER_NAME` is also the leader-election lease name and the
> value the controller stamps on every Worker/Manager/Team/Human CR it
> creates as the `agentteams.io/controller` label. The controller's informer
> cache filters CRs by this label, so multiple AgentTeams releases in the same
> namespace never reconcile each other's resources. The Helm chart sets
> this automatically from the release name; if you deploy by hand, set it
> explicitly — starting the controller without it in incluster mode fails
> fast.

No caching. Edit the ConfigMap → the next Pod created by the controller
uses the new template. Existing Pods are untouched (delete them to pick up
the change).

If the ConfigMap is missing, malformed, or the API call fails for any reason,
the controller falls back to the default Pod shape. Pod creation is never
blocked by a broken template.

## ConfigMap schema

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: <controller-name>    # == AGENTTEAMS_CONTROLLER_NAME env on controller
  namespace: <controller-ns> # same namespace as controller
data:
  pod-template.yaml: |
    metadata:
      annotations: {...}
      labels: {...}
    spec:
      nodeSelector: {...}
      tolerations: [...]
      imagePullSecrets: [...]
      securityContext: {...}
      # ...any corev1.PodSpec field
```

> The value under `pod-template.yaml` is a `PodTemplateSpec` with only two
> top-level fields: `metadata:` and `spec:`. Do **not** wrap it in
> `apiVersion: v1` / `kind: PodTemplate`.

Find a ready-to-apply example at [`docs/examples/agent-pod-template-cm.yaml`](examples/agent-pod-template-cm.yaml).

## Merge semantics

Fields the **template wins**:

- `spec.nodeSelector`
- `spec.tolerations`
- `spec.affinity`
- `spec.imagePullSecrets`
- `spec.securityContext` (including `sysctls`)
- `spec.topologySpreadConstraints`
- `spec.runtimeClassName`, `spec.schedulerName`, `spec.priorityClassName`
- `spec.dnsPolicy`, `spec.dnsConfig`
- `spec.hostAliases` (then controller `CreateRequest.ExtraHosts` is appended)
- `spec.containers[]` with names other than `worker` — preserved as sidecars
- Any other `spec.*` field not listed below

Fields the **controller wins** (template-provided values are discarded):

- `metadata.ownerReferences` — always inherited from the controller Pod
- `spec.serviceAccountName`
- `spec.automountServiceAccountToken` — forced to `false`
- Agent container's `image`, `env`, `workingDir`, `imagePullPolicy`

Hybrid merges:

| Field | Rule |
|---|---|
| `metadata.labels` | template first, controller labels overwrite on key collision |
| `metadata.annotations` | template first, controller annotations overwrite on key collision |
| Agent container's `resources` | `CreateRequest.Resources` (per-request) > template's resources > backend default |
| Agent container's `volumeMounts` | template first, `agentteams-token` volumeMount always appended |
| `spec.volumes` | template first, `agentteams-token` projected volume always appended |
| `spec.restartPolicy` | template if set, else `Always` |

## Troubleshooting

**Does my template apply?** Create a new Worker and inspect its Pod:

```bash
kubectl get pod <worker-pod> -n <ns> -o yaml | grep -A5 nodeSelector
```

**Is the controller seeing my ConfigMap?** Controller logs at `V(1)` or
default level will show one of:

- `agent pod template ConfigMap not found; using empty overlay` — create/rename the CM.
- `agent pod template YAML parse failed` — your YAML is invalid.
- `agent pod template ConfigMap fetch failed` — API / RBAC issue.

**RBAC**: The default Helm-installed controller `ClusterRole` already grants
`get` on `configmaps`. For hand-rolled Deployments, ensure that verb is
present.
