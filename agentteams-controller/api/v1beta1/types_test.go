package v1beta1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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

// TestLeaderSpec_DeepCopyLabels verifies LeaderSpec.Labels survives
// DeepCopy. The Leader path is exercised separately from TeamWorkerSpec
// because TeamSpec.DeepCopyInto calls LeaderSpec.DeepCopyInto directly
// and any regression there would silently drop leader labels.
func TestLeaderSpec_DeepCopyLabels(t *testing.T) {
	src := LeaderSpec{Name: "ld", Labels: map[string]string{"role-hint": "planner"}}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp.Labels, src.Labels) {
		t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
	}
	src.Labels["role-hint"] = "mutated"
	if cp.Labels["role-hint"] != "planner" {
		t.Fatalf("DeepCopy aliased LeaderSpec.Labels: %v", cp.Labels)
	}
}

// TestTeamWorkerSpec_DeepCopyLabels verifies TeamWorkerSpec.Labels
// survives DeepCopy through the TeamSpec.Workers[] slice path.
func TestTeamWorkerSpec_DeepCopyLabels(t *testing.T) {
	src := TeamWorkerSpec{Name: "w1", Labels: map[string]string{"skill": "rust"}}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp.Labels, src.Labels) {
		t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
	}
	src.Labels["skill"] = "mutated"
	if cp.Labels["skill"] != "rust" {
		t.Fatalf("DeepCopy aliased TeamWorkerSpec.Labels: %v", cp.Labels)
	}
}

// TestTeamSpec_DeepCopyCascadesToMemberLabels verifies that DeepCopy on
// the top-level TeamSpec correctly cascades into both LeaderSpec.Labels
// and every TeamWorkerSpec.Labels — catching the case where a future
// refactor regenerates zz_generated.deepcopy.go and accidentally drops a
// nested call.
func TestTeamSpec_DeepCopyCascadesToMemberLabels(t *testing.T) {
	src := TeamSpec{
		Leader: LeaderSpec{
			Name:   "ld",
			Labels: map[string]string{"role-hint": "planner"},
		},
		Workers: []TeamWorkerSpec{
			{Name: "w1", Labels: map[string]string{"skill": "rust"}},
			{Name: "w2"}, // nil labels branch
		},
	}
	cp := *src.DeepCopy()

	src.Leader.Labels["role-hint"] = "mutated"
	src.Workers[0].Labels["skill"] = "mutated"

	if cp.Leader.Labels["role-hint"] != "planner" {
		t.Fatalf("LeaderSpec.Labels not isolated: %v", cp.Leader.Labels)
	}
	if cp.Workers[0].Labels["skill"] != "rust" {
		t.Fatalf("Workers[0].Labels not isolated: %v", cp.Workers[0].Labels)
	}
	if cp.Workers[1].Labels != nil {
		t.Fatalf("Workers[1].Labels should remain nil after DeepCopy, got %v", cp.Workers[1].Labels)
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

func TestTeamSpec_DeepCopyCascadesToMemberResources(t *testing.T) {
	src := TeamSpec{
		Leader: LeaderSpec{
			Name: "lead",
			Resources: &AgentResourceRequirements{
				Requests: AgentResourceValues{CPU: "300m", Memory: "768Mi"},
				Limits:   AgentResourceValues{CPU: "2", Memory: "3Gi"},
			},
		},
		Workers: []TeamWorkerSpec{
			{
				Name: "w1",
				Resources: &AgentResourceRequirements{
					Requests: AgentResourceValues{CPU: "200m", Memory: "512Mi"},
					Limits:   AgentResourceValues{CPU: "1", Memory: "2Gi"},
				},
			},
			{Name: "w2"},
		},
	}
	cp := *src.DeepCopy()

	src.Leader.Resources.Requests.CPU = "999m"
	src.Workers[0].Resources.Limits.Memory = "9Gi"

	if cp.Leader.Resources.Requests.CPU != "300m" {
		t.Fatalf("LeaderSpec.Resources not isolated: %v", cp.Leader.Resources)
	}
	if cp.Workers[0].Resources.Limits.Memory != "2Gi" {
		t.Fatalf("TeamWorkerSpec.Resources not isolated: %v", cp.Workers[0].Resources)
	}
	if cp.Workers[1].Resources != nil {
		t.Fatalf("Workers[1].Resources should remain nil after DeepCopy, got %v", cp.Workers[1].Resources)
	}
}
