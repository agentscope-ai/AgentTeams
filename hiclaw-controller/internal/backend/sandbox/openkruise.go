package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.kruise.io",
	Version:  "v1alpha1",
	Resource: "sandboxes",
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
	}
}

func (p *OpenKruisePlugin) Capabilities(config ProviderConfig) ProviderCapabilities {
	max := p.MaxCapabilities()
	cfg := config.Capabilities
	return ProviderCapabilities{
		Hibernate: max.Hibernate && cfg.Hibernate,
	}
}

func (p *OpenKruisePlugin) CreateSandbox(ctx context.Context, spec SandboxSpec, config ProviderConfig) (SandboxHandle, error) {
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
			"kind":       "Sandbox",
			"metadata":   metadata,
			"spec":       p.buildSandboxSpec(spec),
		},
	}

	if spec.OwnerRef != nil {
		obj.SetOwnerReferences([]metav1.OwnerReference{*spec.OwnerRef})
	}

	created, err := p.dynamicClient.Resource(sandboxGVR).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		// AlreadyExists covers two cases we want the reconciler to retry:
		//   1. A live Sandbox CR with the same name already exists.
		//   2. A previous Sandbox CR with the same name is still in its
		//      terminating/finalizer phase (error message contains
		//      "object is being deleted"). The API server surfaces this as
		//      reason=AlreadyExists.
		if apierrors.IsAlreadyExists(err) {
			return SandboxHandle{}, fmt.Errorf("%w: %s/%s: %v", ErrAlreadyExists, ns, spec.Name, err)
		}
		return SandboxHandle{}, fmt.Errorf("openkruise create sandbox %s/%s: %w", ns, spec.Name, err)
	}

	return SandboxHandle{
		SandboxID: created.GetName(),
	}, nil
}

func (p *OpenKruisePlugin) DeleteSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error {
	ns := config.Namespace
	err := p.dynamicClient.Resource(sandboxGVR).Namespace(ns).Delete(ctx, sandboxID, metav1.DeleteOptions{})
	if err != nil {
		// Treat NotFound as success.
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

	_, err = p.dynamicClient.Resource(sandboxGVR).Namespace(ns).Patch(
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

	_, err = p.dynamicClient.Resource(sandboxGVR).Namespace(ns).Patch(
		ctx, sandboxID, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("openkruise resume sandbox %s/%s: %w", ns, sandboxID, err)
	}
	return nil
}

func (p *OpenKruisePlugin) GetStatus(ctx context.Context, sandboxID string, config ProviderConfig) (SandboxStatus, error) {
	ns := config.Namespace
	obj, err := p.dynamicClient.Resource(sandboxGVR).Namespace(ns).Get(ctx, sandboxID, metav1.GetOptions{})
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

	return SandboxStatus{
		Phase:           phase,
		Message:         message,
		Raw:             raw,
		AppliedSpecHash: obj.GetAnnotations()[AnnotationLastAppliedSpecHash],
	}, nil
}

func (p *OpenKruisePlugin) Validate(config ProviderConfig) error {
	if p.dynamicClient == nil {
		return fmt.Errorf("%w: dynamic client is nil", ErrInvalidConfig)
	}
	// Verify the CRD is available by attempting a List with limit=0.
	_, err := p.dynamicClient.Resource(sandboxGVR).Namespace(config.Namespace).List(
		context.Background(), metav1.ListOptions{Limit: 1})
	if err != nil {
		// If it's a 404 on the resource type, the CRD isn't installed.
		return fmt.Errorf("%w: agents.kruise.io/v1alpha1 sandboxes not available: %v", ErrProviderUnavailable, err)
	}
	return nil
}

func (p *OpenKruisePlugin) HealthCheck(ctx context.Context, config ProviderConfig) error {
	if p.dynamicClient == nil {
		return fmt.Errorf("%w: dynamic client is nil", ErrProviderUnavailable)
	}
	_, err := p.dynamicClient.Resource(sandboxGVR).Namespace(config.Namespace).List(
		ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	return nil
}

// buildSandboxSpec constructs the unstructured spec map for the Sandbox CR.
// The Sandbox CRD (agents.kruise.io/v1alpha1) embeds a standard v1.PodTemplateSpec
// under spec.template. We serialize the full PodTemplateSpec via JSON round-trip
// to preserve all fields without manual field-by-field mapping.
func (p *OpenKruisePlugin) buildSandboxSpec(spec SandboxSpec) map[string]interface{} {
	runtimes := []interface{}{
		map[string]interface{}{"name": "agent-runtime"},
	}

	templateBytes, err := json.Marshal(spec.Template)
	if err != nil {
		return map[string]interface{}{"template": map[string]interface{}{}, "runtimes": runtimes}
	}
	var templateMap map[string]interface{}
	if err := json.Unmarshal(templateBytes, &templateMap); err != nil {
		return map[string]interface{}{"template": map[string]interface{}{}, "runtimes": runtimes}
	}
	return map[string]interface{}{
		"template": templateMap,
		"runtimes": runtimes,
	}
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
