package v1beta1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// strPtr / boolPtr are tiny helpers used by the cross-cluster deployment
// serialization tests below. Kept package-private to avoid leaking generic
// helpers from the API package.
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// TestWorkerSpec_DeployFieldsJSONTags verifies the deployment fields
// (DeployMode, ServiceEnabled) marshal
// with stable, lowerCamelCase JSON keys and omit cleanly when nil.
func TestWorkerSpec_DeployFieldsJSONTags(t *testing.T) {
	cases := []struct {
		name    string
		spec    WorkerSpec
		wantSub []string // substrings expected in JSON
		absent  []string // substrings that must NOT appear in JSON
	}{
		{
			name: "local_with_service",
			spec: WorkerSpec{
				Model:          "m",
				DeployMode:     strPtr("Local"),
				ServiceEnabled: boolPtr(true),
			},
			wantSub: []string{`"deployMode":"Local"`, `"serviceEnabled":true`},
		},
		{
			name: "edge_without_service",
			spec: WorkerSpec{
				Model:      "m",
				DeployMode: strPtr("Edge"),
			},
			wantSub: []string{`"deployMode":"Edge"`},
			absent:  []string{`"serviceEnabled"`},
		},
		{
			name:   "all_nil_omitted",
			spec:   WorkerSpec{Model: "m"},
			absent: []string{`"deployMode"`, `"targetCluster"`, `"serviceEnabled"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(data)
			for _, sub := range tc.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("JSON missing %q: %s", sub, got)
				}
			}
			for _, sub := range tc.absent {
				if strings.Contains(got, sub) {
					t.Errorf("JSON should omit %q: %s", sub, got)
				}
			}
		})
	}
}

// TestWorkerSpec_DeployFieldsRoundTrip verifies the new fields survive a
// JSON marshal/unmarshal cycle without value drift.
func TestWorkerSpec_DeployFieldsRoundTrip(t *testing.T) {
	orig := WorkerSpec{
		Model:          "m",
		DeployMode:     strPtr("Edge"),
		ServiceEnabled: boolPtr(false),
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got WorkerSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.DeployMode == nil || *got.DeployMode != "Edge" {
		t.Fatalf("DeployMode = %v, want *Edge", got.DeployMode)
	}
	if got.ServiceEnabled == nil || *got.ServiceEnabled != false {
		t.Fatalf("ServiceEnabled = %v, want *false", got.ServiceEnabled)
	}
}

// TestWorkerSpec_BackwardCompatOldJSON verifies that JSON payloads written
// before the cross-cluster fields existed deserialize cleanly with all
// new pointer fields left nil.
func TestWorkerSpec_BackwardCompatOldJSON(t *testing.T) {
	old := []byte(`{"model":"m","runtime":"openclaw"}`)
	var got WorkerSpec
	if err := json.Unmarshal(old, &got); err != nil {
		t.Fatalf("Unmarshal old payload: %v", err)
	}
	if got.DeployMode != nil {
		t.Errorf("DeployMode should default to nil, got %v", *got.DeployMode)
	}
	if got.ServiceEnabled != nil {
		t.Errorf("ServiceEnabled should default to nil, got %v", *got.ServiceEnabled)
	}
}

// TestWorkerSpec_DeepCopyLabels verifies WorkerSpec.Labels is deep-copied:
// mutating the source map after DeepCopy must not mutate the copy. Covers
// nil, empty-but-non-nil, and populated variants because our hand-edited
// zz_generated.deepcopy.go has no code-gen safety net.
func TestWorkerSpec_DeepCopyLabels(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
	}{
		{name: "nil", in: nil},
		{name: "empty", in: map[string]string{}},
		{name: "populated", in: map[string]string{"owner": "alice", "env": "prod"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := WorkerSpec{Model: "m", Labels: tc.in}
			cp := *src.DeepCopy()

			if !reflect.DeepEqual(cp.Labels, src.Labels) {
				t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
			}
			if tc.in != nil {
				src.Labels["mutated"] = "x"
				if _, ok := cp.Labels["mutated"]; ok {
					t.Fatalf("DeepCopy did not isolate Labels: %v", cp.Labels)
				}
			}
		})
	}
}

// TestManagerSpec_DeepCopyLabels mirrors the WorkerSpec assertion for
// ManagerSpec.Labels.
func TestManagerSpec_DeepCopyLabels(t *testing.T) {
	src := ManagerSpec{Model: "m", Labels: map[string]string{"tier": "ctrl"}}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp.Labels, src.Labels) {
		t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
	}
	src.Labels["mutated"] = "x"
	if _, ok := cp.Labels["mutated"]; ok {
		t.Fatalf("DeepCopy did not isolate ManagerSpec.Labels: %v", cp.Labels)
	}
	// Nil branch — ensure DeepCopy does not allocate an empty map for nil
	// input (preserves JSON omitempty round-trip stability).
	srcNil := ManagerSpec{Model: "m"}
	cpNil := *srcNil.DeepCopy()
	if cpNil.Labels != nil {
		t.Fatalf("expected nil Labels on deep-copy of nil source, got %v", cpNil.Labels)
	}
}

func TestWorkerSpec_DeepCopyResources(t *testing.T) {
	src := WorkerSpec{
		Model: "m",
		Resources: &AgentResourceRequirements{
			Requests: AgentResourceValues{CPU: "250m", Memory: "512Mi"},
			Limits:   AgentResourceValues{CPU: "2", Memory: "4Gi"},
		},
	}
	cp := *src.DeepCopy()

	if !reflect.DeepEqual(cp.Resources, src.Resources) {
		t.Fatalf("copy resources=%v want %v", cp.Resources, src.Resources)
	}
	src.Resources.Requests.CPU = "500m"
	if cp.Resources.Requests.CPU != "250m" {
		t.Fatalf("DeepCopy aliased WorkerSpec.Resources: %v", cp.Resources)
	}

	srcNil := WorkerSpec{Model: "m"}
	cpNil := *srcNil.DeepCopy()
	if cpNil.Resources != nil {
		t.Fatalf("expected nil Resources on deep-copy of nil source, got %v", cpNil.Resources)
	}
}

func TestManagerSpec_DeepCopyResources(t *testing.T) {
	src := ManagerSpec{
		Model: "m",
		Resources: &AgentResourceRequirements{
			Requests: AgentResourceValues{CPU: "500m", Memory: "1Gi"},
			Limits:   AgentResourceValues{CPU: "3", Memory: "5Gi"},
		},
	}
	cp := *src.DeepCopy()

	src.Resources.Limits.Memory = "6Gi"
	if cp.Resources.Limits.Memory != "5Gi" {
		t.Fatalf("DeepCopy aliased ManagerSpec.Resources: %v", cp.Resources)
	}
}

func TestWorkerSpec_DeepAgentsRuntimeConfigRoundTripAndDeepCopy(t *testing.T) {
	src := WorkerSpec{
		Model:   "qwen-max",
		Runtime: "deepagents",
		RuntimeConfig: &WorkerRuntimeConfig{
			DeepAgents: &DeepAgentsRuntimeConfig{
				Approvals: DeepAgentsApprovalConfig{
					FileWrites:   "required",
					MCPDefault:   "required",
					Coordinators: []string{"@lead:example.org"},
					MCPRules: []DeepAgentsMCPApprovalRule{{
						Server: "github",
						Tool:   "get_issue",
						Mode:   "notRequired",
					}},
				},
				Execution: DeepAgentsExecutionConfig{
					Mode:        "sandbox",
					IdleTimeout: "30m",
					MaxLifetime: "8h",
					Resources: &AgentResourceRequirements{
						Requests: AgentResourceValues{CPU: "100m", Memory: "128Mi"},
						Limits:   AgentResourceValues{CPU: "1", Memory: "1Gi"},
					},
					Egress: []DeepAgentsEgressRule{{
						CIDR:  "10.96.0.10/32",
						Ports: []int32{53, 443},
					}},
				},
			},
		},
	}

	payload, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonText := string(payload)
	for _, want := range []string{
		`"runtimeConfig"`, `"deepagents"`, `"fileWrites":"required"`,
		`"mcpDefault":"required"`, `"idleTimeout":"30m"`,
		`"maxLifetime":"8h"`, `"cidr":"10.96.0.10/32"`,
	} {
		if !strings.Contains(jsonText, want) {
			t.Errorf("JSON missing %q: %s", want, jsonText)
		}
	}

	var roundTripped WorkerSpec
	if err := json.Unmarshal(payload, &roundTripped); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, src) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", roundTripped, src)
	}

	cloned := *src.DeepCopy()
	src.RuntimeConfig.DeepAgents.Approvals.Coordinators[0] = "@mutated:example.org"
	src.RuntimeConfig.DeepAgents.Approvals.MCPRules[0].Tool = "mutated"
	src.RuntimeConfig.DeepAgents.Execution.Resources.Requests.CPU = "900m"
	src.RuntimeConfig.DeepAgents.Execution.Egress[0].Ports[0] = 1
	got := cloned.RuntimeConfig.DeepAgents
	if got.Approvals.Coordinators[0] != "@lead:example.org" ||
		got.Approvals.MCPRules[0].Tool != "get_issue" ||
		got.Execution.Resources.Requests.CPU != "100m" ||
		got.Execution.Egress[0].Ports[0] != 53 {
		t.Fatalf("DeepCopy aliased DeepAgents runtime config: %#v", got)
	}
}

func TestWorkerStatus_DeepCopyConditions(t *testing.T) {
	src := WorkerStatus{Conditions: []metav1.Condition{{
		Type:    "RuntimeConfigReady",
		Status:  metav1.ConditionTrue,
		Reason:  "Projected",
		Message: "runtime config is available",
	}}}
	cloned := *src.DeepCopy()
	src.Conditions[0].Message = "mutated"
	if got := cloned.Conditions[0].Message; got != "runtime config is available" {
		t.Fatalf("DeepCopy aliased WorkerStatus.Conditions: %q", got)
	}
}

func TestExecutionSandboxRegisteredAndDeepCopied(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	obj, err := scheme.New(SchemeGroupVersion.WithKind("ExecutionSandbox"))
	if err != nil {
		t.Fatalf("ExecutionSandbox not registered: %v", err)
	}
	if _, ok := obj.(*ExecutionSandbox); !ok {
		t.Fatalf("registered object type=%T, want *ExecutionSandbox", obj)
	}

	now := metav1.Now()
	src := &ExecutionSandbox{
		Spec: ExecutionSandboxSpec{
			WorkerRef: ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID: "thread-hash",
			Image:     "runner:v1",
			Resources: &AgentResourceRequirements{
				Requests: AgentResourceValues{CPU: "100m", Memory: "128Mi"},
			},
			Egress: []DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{53}}},
		},
		Status: ExecutionSandboxStatus{
			Phase:         "Ready",
			LastHeartbeat: &now,
			ExpiresAt:     &now,
			Conditions:    []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	cloned := src.DeepCopy()
	src.Spec.Resources.Requests.CPU = "900m"
	src.Spec.Egress[0].Ports[0] = 443
	src.Status.LastHeartbeat.Time = src.Status.LastHeartbeat.Add(time.Hour)
	src.Status.Conditions[0].Type = "Mutated"
	if cloned.Spec.Resources.Requests.CPU != "100m" || cloned.Spec.Egress[0].Ports[0] != 53 ||
		cloned.Status.Conditions[0].Type != "Ready" || cloned.Status.LastHeartbeat.Equal(src.Status.LastHeartbeat) {
		t.Fatalf("ExecutionSandbox DeepCopy aliased source: %#v", cloned)
	}
}
