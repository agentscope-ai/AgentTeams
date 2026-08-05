package sandboxpolicy

import (
	"reflect"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

func TestPolicyResolve(t *testing.T) {
	tests := []struct {
		name        string
		in          *v1beta1.ExecutionSandboxResourceRequirements
		wantRequest string
		wantLimit   string
		wantCPU     string
		wantMemory  string
	}{
		{
			name:        "nil input uses defaults",
			wantRequest: "256Mi",
			wantLimit:   "2Gi",
		},
		{
			name: "request only uses default limit",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "512Mi"},
			},
			wantRequest: "512Mi",
			wantLimit:   "2Gi",
		},
		{
			name: "limit only uses default request",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "4Gi"},
			},
			wantRequest: "256Mi",
			wantLimit:   "4Gi",
		},
		{
			name: "request and limit are retained",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "512Mi"},
				Limits:   v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "4Gi"},
			},
			wantRequest: "512Mi",
			wantLimit:   "4Gi",
		},
		{
			name: "CPU and memory are preserved",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{
					CPU:              "250m",
					Memory:           "256Mi",
					EphemeralStorage: "512Mi",
				},
				Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "4Gi"},
			},
			wantRequest: "512Mi",
			wantLimit:   "4Gi",
			wantCPU:     "250m",
			wantMemory:  "256Mi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var before *v1beta1.ExecutionSandboxResourceRequirements
			if tt.in != nil {
				copy := *tt.in
				before = &copy
			}

			resolved, resources, sizeLimit, err := Default().Resolve(tt.in)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !reflect.DeepEqual(tt.in, before) {
				t.Fatalf("Resolve() mutated input: got %#v, want %#v", tt.in, before)
			}
			if got := resolved.Requests.EphemeralStorage; got != tt.wantRequest {
				t.Errorf("resolved request = %q, want %q", got, tt.wantRequest)
			}
			if got := resolved.Limits.EphemeralStorage; got != tt.wantLimit {
				t.Errorf("resolved limit = %q, want %q", got, tt.wantLimit)
			}
			if got := quantityString(resources.Requests, corev1.ResourceEphemeralStorage); got != tt.wantRequest {
				t.Errorf("ephemeral-storage request = %q, want %q", got, tt.wantRequest)
			}
			if got := quantityString(resources.Limits, corev1.ResourceEphemeralStorage); got != tt.wantLimit {
				t.Errorf("ephemeral-storage limit = %q, want %q", got, tt.wantLimit)
			}
			if got := sizeLimit.String(); got != tt.wantLimit {
				t.Errorf("sizeLimit = %q, want %q", got, tt.wantLimit)
			}
			if tt.wantCPU != "" {
				if got := quantityString(resources.Requests, corev1.ResourceCPU); got != tt.wantCPU {
					t.Errorf("CPU request = %q, want %q", got, tt.wantCPU)
				}
			}
			if tt.wantMemory != "" {
				if got := quantityString(resources.Requests, corev1.ResourceMemory); got != tt.wantMemory {
					t.Errorf("memory request = %q, want %q", got, tt.wantMemory)
				}
			}
		})
	}
}

func quantityString(resources corev1.ResourceList, name corev1.ResourceName) string {
	quantity := resources[name]
	return quantity.String()
}

func TestPolicyResolveRejectsInvalidEphemeralStorage(t *testing.T) {
	tests := []struct {
		name string
		in   *v1beta1.ExecutionSandboxResourceRequirements
	}{
		{
			name: "garbage quantity",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "garbage"},
			},
		},
		{
			name: "zero quantity",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "0"},
			},
		},
		{
			name: "negative quantity",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "-1Gi"},
			},
		},
		{
			name: "request exceeds limit",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "3Gi"},
				Limits:   v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "2Gi"},
			},
		},
		{
			name: "limit exceeds maximum",
			in: &v1beta1.ExecutionSandboxResourceRequirements{
				Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "9Gi"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := *tt.in
			_, _, _, err := Default().Resolve(tt.in)
			if err == nil {
				t.Fatal("Resolve() error = nil, want error")
			}
			if !reflect.DeepEqual(tt.in, &before) {
				t.Fatalf("Resolve() mutated input: got %#v, want %#v", tt.in, &before)
			}
		})
	}
}

func TestZeroPolicyUsesDefaults(t *testing.T) {
	resolved, resources, sizeLimit, err := (Policy{}).Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.Requests.EphemeralStorage; got != DefaultRequest {
		t.Errorf("resolved request = %q, want %q", got, DefaultRequest)
	}
	if got := resolved.Limits.EphemeralStorage; got != DefaultLimit {
		t.Errorf("resolved limit = %q, want %q", got, DefaultLimit)
	}
	if got := quantityString(resources.Limits, corev1.ResourceEphemeralStorage); got != DefaultLimit {
		t.Errorf("ephemeral-storage limit = %q, want %q", got, DefaultLimit)
	}
	if got := sizeLimit.String(); got != DefaultLimit {
		t.Errorf("sizeLimit = %q, want %q", got, DefaultLimit)
	}
}

func TestNewRejectsInconsistentDefaults(t *testing.T) {
	tests := []struct {
		name           string
		defaultRequest string
		defaultLimit   string
		maxLimit       string
	}{
		{
			name:           "default request exceeds default limit",
			defaultRequest: "3Gi",
			defaultLimit:   "2Gi",
			maxLimit:       "8Gi",
		},
		{
			name:           "default limit exceeds maximum",
			defaultRequest: "256Mi",
			defaultLimit:   "9Gi",
			maxLimit:       "8Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.defaultRequest, tt.defaultLimit, tt.maxLimit); err == nil {
				t.Fatal("New() error = nil, want error")
			}
		})
	}
}
