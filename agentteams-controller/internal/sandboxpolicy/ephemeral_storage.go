// Package sandboxpolicy resolves execution sandbox resource requirements
// against the controller's ephemeral-storage policy and Kubernetes compute
// resource constraints.
package sandboxpolicy

import (
	"fmt"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	DefaultRequest = "256Mi"
	DefaultLimit   = "2Gi"
	MaxLimit       = "8Gi"
)

// Policy holds validated execution-sandbox ephemeral-storage bounds.
type Policy struct {
	defaultRequest resource.Quantity
	defaultLimit   resource.Quantity
	maxLimit       resource.Quantity
}

// New creates a policy after validating its positive, ordered quantities.
func New(defaultRequest, defaultLimit, maxLimit string) (Policy, error) {
	request, err := parsePositive("default ephemeral-storage request", defaultRequest)
	if err != nil {
		return Policy{}, err
	}
	limit, err := parsePositive("default ephemeral-storage limit", defaultLimit)
	if err != nil {
		return Policy{}, err
	}
	maximum, err := parsePositive("maximum ephemeral-storage limit", maxLimit)
	if err != nil {
		return Policy{}, err
	}
	if request.Cmp(limit) > 0 {
		return Policy{}, fmt.Errorf("default ephemeral-storage request %s exceeds default limit %s", request.String(), limit.String())
	}
	if limit.Cmp(maximum) > 0 {
		return Policy{}, fmt.Errorf("default ephemeral-storage limit %s exceeds maximum limit %s", limit.String(), maximum.String())
	}
	return Policy{defaultRequest: request, defaultLimit: limit, maxLimit: maximum}, nil
}

// Default returns the built-in policy. A panic means its compile-time
// constants have been changed to an invalid policy.
func Default() Policy {
	policy, err := New(DefaultRequest, DefaultLimit, MaxLimit)
	if err != nil {
		panic(err)
	}
	return policy
}

// Resolve copies in, fills omitted ephemeral-storage values from the policy,
// and returns canonical API values, container requirements, and the volume
// size limit. It never mutates in.
func (p Policy) Resolve(in *v1beta1.ExecutionSandboxResourceRequirements) (
	*v1beta1.ExecutionSandboxResourceRequirements,
	corev1.ResourceRequirements,
	resource.Quantity,
	error,
) {
	if p.isZero() {
		p = Default()
	}

	resolved := &v1beta1.ExecutionSandboxResourceRequirements{}
	if in != nil {
		*resolved = *in
	}
	if resolved.Requests.EphemeralStorage == "" {
		resolved.Requests.EphemeralStorage = p.defaultRequest.String()
	}
	if resolved.Limits.EphemeralStorage == "" {
		resolved.Limits.EphemeralStorage = p.defaultLimit.String()
	}

	requests, err := resourceList(&resolved.Requests)
	if err != nil {
		return nil, corev1.ResourceRequirements{}, resource.Quantity{}, fmt.Errorf("sandbox resource requests: %w", err)
	}
	limits, err := resourceList(&resolved.Limits)
	if err != nil {
		return nil, corev1.ResourceRequirements{}, resource.Quantity{}, fmt.Errorf("sandbox resource limits: %w", err)
	}

	request := requests[corev1.ResourceEphemeralStorage]
	limit := limits[corev1.ResourceEphemeralStorage]
	if request.Cmp(limit) > 0 {
		return nil, corev1.ResourceRequirements{}, resource.Quantity{}, fmt.Errorf("ephemeral-storage request %s exceeds limit %s", request.String(), limit.String())
	}
	if limit.Cmp(p.maxLimit) > 0 {
		return nil, corev1.ResourceRequirements{}, resource.Quantity{}, fmt.Errorf("ephemeral-storage limit %s exceeds maximum limit %s", limit.String(), p.maxLimit.String())
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, hasRequest := requests[name]
		limit, hasLimit := limits[name]
		if hasRequest && hasLimit && request.Cmp(limit) > 0 {
			return nil, corev1.ResourceRequirements{}, resource.Quantity{}, fmt.Errorf("%s request %s exceeds limit %s", name, request.String(), limit.String())
		}
	}

	return resolved, corev1.ResourceRequirements{Requests: requests, Limits: limits}, limit, nil
}

func (p Policy) isZero() bool {
	return p.defaultRequest.IsZero() && p.defaultLimit.IsZero() && p.maxLimit.IsZero()
}

func resourceList(values *v1beta1.ExecutionSandboxResourceValues) (corev1.ResourceList, error) {
	result := corev1.ResourceList{}
	for _, item := range []struct {
		name  corev1.ResourceName
		raw   string
		apply func(string)
	}{
		{name: corev1.ResourceCPU, raw: values.CPU, apply: func(value string) { values.CPU = value }},
		{name: corev1.ResourceMemory, raw: values.Memory, apply: func(value string) { values.Memory = value }},
		{name: corev1.ResourceEphemeralStorage, raw: values.EphemeralStorage, apply: func(value string) { values.EphemeralStorage = value }},
	} {
		if item.raw == "" || (item.name != corev1.ResourceEphemeralStorage && strings.TrimSpace(item.raw) == "") {
			continue
		}
		quantity, err := resource.ParseQuantity(item.raw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s quantity %q: %w", item.name, item.raw, err)
		}
		if item.name == corev1.ResourceEphemeralStorage && quantity.Sign() <= 0 {
			return nil, fmt.Errorf("%s quantity %q must be positive", item.name, item.raw)
		}
		if item.name != corev1.ResourceEphemeralStorage && quantity.Sign() < 0 {
			return nil, fmt.Errorf("%s quantity %q must be non-negative", item.name, item.raw)
		}
		item.apply(quantity.String())
		result[item.name] = quantity
	}
	return result, nil
}

func parsePositive(name, raw string) (resource.Quantity, error) {
	quantity, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parse %s %q: %w", name, raw, err)
	}
	if quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s %q must be positive", name, raw)
	}
	return quantity, nil
}
