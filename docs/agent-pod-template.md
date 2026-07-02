# Agent Pod Template

The hiclaw-controller creates one Kubernetes Pod per Manager and Worker
("agent Pods"). By default, these Pods have a minimal shape: one container
named `worker`, a projected `hiclaw-token` volume, the controller-managed
`ServiceAccount`, and not much else.

To inject cluster-specific concerns — sysctls, nodeSelectors, tolerations,
imagePullSecrets, annotations consumed by CNI/sidecar injectors, etc. —
you provide a `corev1.PodTemplateSpec` overlay via a ConfigMap.

## How it works

On **every** `Create()`, the controller reads a ConfigMap from its own
namespace with the name equal to its `HICLAW_CONTROLLER_NAME` environment
variable (for a Helm release `prod`, that is `prod-hiclaw-controller`).
If the ConfigMap is present and has a key `pod-template.yaml`, its value is
parsed as a `PodTemplateSpec` and merged with the controller-owned fields
to produce the final Pod.

> `HICLAW_CONTROLLER_NAME` is also the leader-election lease name and the
> value the controller stamps on every Worker/Manager/Team/Human CR it
> creates as the `hiclaw.io/controller` label. The controller's informer
> cache filters CRs by this label, so multiple hiclaw releases in the same
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
  name: <controller-name>    # == HICLAW_CONTROLLER_NAME env on controller
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

## Per-deploy-mode override (Remote)

When a Worker is created with `spec.deployMode: Remote`, the controller
schedules its Pod into a remote cluster fronted by a virtual-kubelet style
provider, where node taints, image-pull secrets, and tolerations almost
always differ from the in-cluster defaults. To avoid forcing those
remote-only fields onto every local Pod, the same ConfigMap may carry an
additional key:

| Key | Consumed when |
|---|---|
| `pod-template.yaml` | Default and `deployMode: Local` only. NOT used as fallback for Remote mode. |
| `pod-template-remote.yaml` | Only when `deployMode: Remote` and the key is present and non-empty. |

Resolution rules:

- `deployMode: Remote` → if `pod-template-remote.yaml` is present and
  non-empty, it is used; otherwise the controller applies the default Pod
  spec with no overlay (it does **not** fall back to `pod-template.yaml`).
- Any other deploy mode (default / `Local`) → the remote key is ignored
  even when present, and only `pod-template.yaml` is consulted.
- Non-K8s backends never consume the remote key.

This lets you keep one ConfigMap per controller while shipping different
`tolerations`, `nodeSelector`, `imagePullSecrets`, annotations or sidecars
to remote-cluster Pods, e.g.:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: <controller-name>
  namespace: <controller-ns>
data:
  pod-template.yaml: |
    spec:
      nodeSelector:
        kubernetes.io/os: linux
  pod-template-remote.yaml: |
    spec:
      nodeSelector:
        type: virtual-kubelet
      tolerations:
        - key: virtual-kubelet.io/provider
          operator: Exists
          effect: NoSchedule
      imagePullSecrets:
        - name: remote-registry-pull-secret
```

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
| Agent container's `volumeMounts` | template first, `hiclaw-token` volumeMount always appended |
| `spec.volumes` | template first, `hiclaw-token` projected volume always appended |
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
