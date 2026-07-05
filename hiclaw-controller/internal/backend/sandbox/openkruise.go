package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.kruise.io",
	Version:  "v1alpha1",
	Resource: "sandboxes",
}

var sandboxClaimGVR = schema.GroupVersionResource{
	Group:    "agents.kruise.io",
	Version:  "v1alpha1",
	Resource: "sandboxclaims",
}

// OpenKruisePlugin implements SandboxPlugin for the OpenKruise Agent Sandbox CRD.
// It uses the Kubernetes dynamic client to operate on agents.kruise.io/v1alpha1
// Sandbox resources, which is compatible with both open-source openkruise/agents
// and Alibaba Cloud ACS.
type OpenKruisePlugin struct {
	dynamicClient dynamic.Interface
}

// NewOpenKruisePlugin creates a new OpenKruise sandbox plugin.
func NewOpenKruisePlugin(dynamicClient dynamic.Interface) *OpenKruisePlugin {
	return &OpenKruisePlugin{dynamicClient: dynamicClient}
}

func (p *OpenKruisePlugin) Type() string { return "openkruise" }

// MaxCapabilities returns the theoretical maximum capabilities of the OpenKruise
// plugin. Actual capabilities are min(Max, config.Capabilities).
func (p *OpenKruisePlugin) MaxCapabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Hibernate: true,
		Pool:      true,
	}
}

func (p *OpenKruisePlugin) Capabilities(config ProviderConfig) ProviderCapabilities {
	max := p.MaxCapabilities()
	cfg := config.Capabilities
	return ProviderCapabilities{
		Hibernate: max.Hibernate && cfg.Hibernate,
		Pool:      max.Pool && cfg.Pool,
	}
}

// resolveClient returns the dynamic client to use for an operation.
// It prefers config.DynamicClient (set by SandboxBackend.resolveProviderConfig
// for remote-mode operations) and falls back to the plugin's own client
// (local cluster) when config.DynamicClient is nil.
func (p *OpenKruisePlugin) resolveClient(config ProviderConfig) dynamic.Interface {
	if config.DynamicClient != nil {
		return config.DynamicClient
	}
	return p.dynamicClient
}

func (p *OpenKruisePlugin) CreateSandboxClaim(ctx context.Context, spec SandboxClaimSpec, config ProviderConfig) (SandboxHandle, error) {
	ns := config.Namespace
	if ns == "" {
		ns = spec.Namespace
	}

	metadata := map[string]interface{}{
		"name":      spec.Name,
		"namespace": ns,
	}
	if len(spec.Labels) > 0 {
		metadata["labels"] = toStringInterfaceMap(spec.Labels)
	}
	if len(spec.Annotations) > 0 {
		metadata["annotations"] = toStringInterfaceMap(spec.Annotations)
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "agents.kruise.io/v1alpha1",
			"kind":       "SandboxClaim",
			"metadata":   metadata,
			"spec":       p.buildSandboxClaimSpec(spec),
		},
	}

	if spec.OwnerRef != nil {
		obj.SetOwnerReferences([]metav1.OwnerReference{*spec.OwnerRef})
	}

	created, err := p.resolveClient(config).Resource(sandboxClaimGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return SandboxHandle{}, fmt.Errorf("%w: %s/%s: %v", ErrAlreadyExists, ns, spec.Name, err)
		}
		return SandboxHandle{}, fmt.Errorf("openkruise create SandboxClaim %s/%s: %w", ns, spec.Name, err)
	}

	sandboxID, _, _ := unstructured.NestedString(created.Object, "status", "sandboxName")
	if sandboxID == "" {
		sandboxID, _, _ = unstructured.NestedString(created.Object, "status", "sandboxRef", "name")
	}
	if sandboxID == "" {
		sandboxID = created.GetName()
	}
	return SandboxHandle{
		SandboxID: sandboxID,
	}, nil
}

func (p *OpenKruisePlugin) DeleteSandboxClaim(ctx context.Context, claimID string, config ProviderConfig) error {
	ns := config.Namespace
	err := p.resolveClient(config).Resource(sandboxClaimGVR).Namespace(ns).Delete(ctx, claimID, metav1.DeleteOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("openkruise delete SandboxClaim %s/%s: %w", ns, claimID, err)
	}
	return nil
}

func (p *OpenKruisePlugin) DeleteSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error {
	ns := config.Namespace
	err := p.resolveClient(config).Resource(sandboxGVR).Namespace(ns).Delete(ctx, sandboxID, metav1.DeleteOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("openkruise delete sandbox %s/%s: %w", ns, sandboxID, err)
	}
	return nil
}

func (p *OpenKruisePlugin) HibernateSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error {
	caps := p.Capabilities(config)
	if !caps.Hibernate {
		return ErrCapabilityNotSupported
	}

	ns := config.Namespace
	// Patch spec.paused=true and stamp last-paused-time in the same MergePatch.
	// Co-locating the two writes guarantees the bookkeeping annotation is
	// updated iff the hibernate intent is recorded server-side, so retries
	// after a partial failure cannot drift the timestamp from reality.
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				AnnotationLastPausedTime: time.Now().UTC().Format(time.RFC3339),
			},
		},
		"spec": map[string]interface{}{
			"paused": true,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("openkruise hibernate: marshal patch: %w", err)
	}

	_, err = p.resolveClient(config).Resource(sandboxGVR).Namespace(ns).Patch(
		ctx, sandboxID, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("openkruise hibernate sandbox %s/%s: %w", ns, sandboxID, err)
	}
	return nil
}

func (p *OpenKruisePlugin) ResumeSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error {
	// Resume is an idempotent MergePatch of spec.paused=false. It is a
	// no-op against an already-running CR and has no destructive side
	// effects, so no capability gate is needed. Only Hibernate (which
	// actively stops the workload) keeps the opt-in gate.
	ns := config.Namespace
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"paused": false,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("openkruise resume: marshal patch: %w", err)
	}

	_, err = p.resolveClient(config).Resource(sandboxGVR).Namespace(ns).Patch(
		ctx, sandboxID, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("openkruise resume sandbox %s/%s: %w", ns, sandboxID, err)
	}
	return nil
}

func (p *OpenKruisePlugin) GetSandboxStatus(ctx context.Context, sandboxID string, config ProviderConfig) (SandboxStatus, error) {
	ns := config.Namespace
	obj, err := p.resolveClient(config).Resource(sandboxGVR).Namespace(ns).Get(ctx, sandboxID, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			// CR really does not exist. Surface a typed sentinel rather
			// than a synthesized Phase so the backend layer cannot
			// accidentally conflate "gone" with "Terminated".
			return SandboxStatus{}, ErrNotFound
		}
		return SandboxStatus{}, fmt.Errorf("openkruise get sandbox %s/%s: %w", ns, sandboxID, err)
	}

	// CR still exists but is already being deleted (finalizer in progress).
	// Report a synthetic Terminating phase so the reconciler waits instead
	// of trying to Create on top of a terminating object and triggering
	// "object is being deleted" AlreadyExists errors.
	if ts := obj.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
		return SandboxStatus{Phase: PhaseTerminating}, nil
	}

	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(obj.Object, "status", "message")

	// .spec.paused is the operator's authoritative intent and is set
	// synchronously by HibernateSandbox, while .status.phase is reconciled
	// by the provider asynchronously. When paused=true, override the phase
	// so the upper layer sees Hibernated immediately and does not race the
	// provider into a Delete+Create cycle. Single-direction only:
	// paused=false does NOT override, because resume has legitimate
	// intermediate phases (Starting/Resuming) the provider reports more
	// accurately.
	if paused, ok, _ := unstructured.NestedBool(obj.Object, "spec", "paused"); ok && paused {
		phase = PhaseHibernated
	}

	var raw map[string]any
	if statusMap, ok, _ := unstructured.NestedMap(obj.Object, "status"); ok {
		raw = statusMap
	}

	// Check the "Ready" condition specifically — only this condition type
	// determines container health. Other conditions (e.g. InplaceUpdate) are
	// informational and do not affect the running/not-running determination.
	readyStatus := true
	var readyMessage string
	if conditions, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions"); ok {
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			condType, _, _ := unstructured.NestedString(cond, "type")
			if condType != "Ready" {
				continue
			}
			// Found the Ready condition.
			s, _, _ := unstructured.NestedString(cond, "status")
			if s != "True" {
				readyStatus = false
				readyMessage, _, _ = unstructured.NestedString(cond, "message")
			}
			break
		}
	}

	return SandboxStatus{
		SandboxID:             sandboxID,
		Phase:                 phase,
		Message:               message,
		Raw:                   raw,
		AppliedSpecHash:       obj.GetAnnotations()[AnnotationLastAppliedSpecHash],
		ReadyConditionStatus:  readyStatus,
		ReadyConditionMessage: readyMessage,
	}, nil
}

func (p *OpenKruisePlugin) ListSandboxes(ctx context.Context, matchLabels map[string]string, config ProviderConfig) ([]SandboxStatus, error) {
	ns := config.Namespace
	opts := metav1.ListOptions{}
	if len(matchLabels) > 0 {
		opts.LabelSelector = labels.SelectorFromSet(labels.Set(matchLabels)).String()
	}
	list, err := p.resolveClient(config).Resource(sandboxGVR).Namespace(ns).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("openkruise list sandboxes %s selector %q: %w", ns, opts.LabelSelector, err)
	}
	out := make([]SandboxStatus, 0, len(list.Items))
	for i := range list.Items {
		status, err := p.GetSandboxStatus(ctx, list.Items[i].GetName(), config)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}

func (p *OpenKruisePlugin) GetSandboxClaimStatus(ctx context.Context, claimID string, config ProviderConfig) (SandboxStatus, error) {
	ns := config.Namespace
	obj, err := p.resolveClient(config).Resource(sandboxClaimGVR).Namespace(ns).Get(ctx, claimID, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return SandboxStatus{}, ErrNotFound
		}
		return SandboxStatus{}, fmt.Errorf("openkruise get SandboxClaim %s/%s: %w", ns, claimID, err)
	}
	if ts := obj.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
		return SandboxStatus{Phase: PhaseTerminating}, nil
	}

	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	message, _, _ := unstructured.NestedString(obj.Object, "status", "message")
	desiredReplicas, desiredOK, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	claimedReplicas, claimedOK, _ := unstructured.NestedInt64(obj.Object, "status", "claimedReplicas")
	var raw map[string]any
	if statusMap, ok, _ := unstructured.NestedMap(obj.Object, "status"); ok {
		raw = statusMap
	}
	var desiredReplicasPtr *int64
	if desiredOK {
		desiredReplicasPtr = &desiredReplicas
	}
	var claimedReplicasPtr *int64
	if claimedOK {
		claimedReplicasPtr = &claimedReplicas
	}
	readyStatus, readyMessage := readyConditionFromObject(obj)
	return SandboxStatus{
		Phase:                 phase,
		Message:               message,
		Raw:                   raw,
		AppliedSpecHash:       obj.GetAnnotations()[AnnotationLastAppliedSpecHash],
		ReadyConditionStatus:  readyStatus,
		ReadyConditionMessage: readyMessage,
		DesiredReplicas:       desiredReplicasPtr,
		ClaimedReplicas:       claimedReplicasPtr,
	}, nil
}

func (p *OpenKruisePlugin) Validate(config ProviderConfig) error {
	if p.dynamicClient == nil {
		return fmt.Errorf("%w: dynamic client is nil", ErrInvalidConfig)
	}
	// CRD existence is validated at operation time
	// to avoid requiring list permissions on the CRD during startup.
	return nil
}

func (p *OpenKruisePlugin) HealthCheck(ctx context.Context, config ProviderConfig) error {
	if p.dynamicClient == nil {
		return fmt.Errorf("%w: dynamic client is nil", ErrProviderUnavailable)
	}
	_, err := p.resolveClient(config).Resource(sandboxGVR).Namespace(config.Namespace).List(
		ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	return nil
}

func (p *OpenKruisePlugin) buildSandboxClaimSpec(spec SandboxClaimSpec) map[string]interface{} {
	out := map[string]interface{}{
		"templateName":      spec.SandboxSetName,
		"replicas":          int64(1),
		"claimTimeout":      "5m",
		"waitReadyTimeout":  "2m",
		"ttlAfterCompleted": "15m",
	}
	if len(spec.Labels) > 0 {
		out["labels"] = toStringInterfaceMap(spec.Labels)
	}
	if len(spec.Annotations) > 0 {
		out["annotations"] = toStringInterfaceMap(spec.Annotations)
	}
	if spec.InplaceUpdate != nil && spec.InplaceUpdate.Image != "" {
		out["inplaceUpdate"] = map[string]interface{}{
			"image": spec.InplaceUpdate.Image,
		}
	}
	if len(spec.DynamicVolumesMount) > 0 {
		mounts := make([]interface{}, 0, len(spec.DynamicVolumesMount))
		for _, mount := range spec.DynamicVolumesMount {
			item := map[string]interface{}{
				"pvName":    mount.PVName,
				"mountPath": mount.MountPath,
				"subPath":   mount.SubPath,
				"readOnly":  mount.ReadOnly,
			}
			if len(mount.Attributes) > 0 {
				item["attributes"] = toStringInterfaceMap(mount.Attributes)
			}
			mounts = append(mounts, item)
		}
		out["dynamicVolumesMount"] = mounts
	}
	return out
}

func readyConditionFromObject(obj *unstructured.Unstructured) (bool, string) {
	readyStatus := true
	var readyMessage string
	if conditions, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions"); ok {
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			condType, _, _ := unstructured.NestedString(cond, "type")
			if condType != "Ready" {
				continue
			}
			s, _, _ := unstructured.NestedString(cond, "status")
			if s != "True" {
				readyStatus = false
				readyMessage, _, _ = unstructured.NestedString(cond, "message")
			}
			break
		}
	}
	return readyStatus, readyMessage
}

// toStringInterfaceMap converts map[string]string to map[string]interface{} for unstructured.
func toStringInterfaceMap(m map[string]string) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// isNotFound checks if the error is a Kubernetes NotFound error.
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
