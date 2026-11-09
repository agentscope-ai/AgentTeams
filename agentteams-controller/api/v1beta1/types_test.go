package v1beta1

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/yaml"
)

// strPtr / boolPtr are tiny helpers used by the cross-cluster deployment
// serialization tests below. Kept package-private to avoid leaking generic
// helpers from the API package.
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// int32Ptr is a tiny helper for pointer-int test fixtures, same
// package-private convention as strPtr/boolPtr above.
func int32Ptr(i int32) *int32 { return &i }

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

// TestCRDStatusSchemasCoverStatusStructs guards the structural-schema
// pruning contract: every status field the controllers persist must exist
// as a property in the CRD schema under config/crd/, otherwise a real
// apiserver silently prunes it (the feature goes no-op while fake-client
// unit tests stay green).
func TestCRDStatusSchemasCoverStatusStructs(t *testing.T) {
	workers := loadCRDDoc(t, "../../config/crd/workers.agentteams.io.yaml")
	assertCRDPropertiesCoverStruct(t, "WorkerStatus", workers, reflect.TypeOf(WorkerStatus{}),
		"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "status", "properties")
	teams := loadCRDDoc(t, "../../config/crd/teams.agentteams.io.yaml")
	assertCRDPropertiesCoverStruct(t, "TeamMemberStatus", teams, reflect.TypeOf(TeamMemberStatus{}),
		"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "status", "properties", "members", "items", "properties")
	loops := loadCRDDoc(t, "../../config/crd/loops.agentteams.io.yaml")
	assertCRDPropertiesCoverStruct(t, "LoopStatus", loops, reflect.TypeOf(LoopStatus{}),
		"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "status", "properties")
}

// TestCRDSpecSchemasCoverSpecStructs extends the structural-schema pruning
// contract to the spec: every JSON-tagged WorkerSpec/TeamSpec field must
// exist as a property in the deployed CRD schema under config/crd/,
// otherwise a real apiserver silently prunes it (the field goes no-op
// while fake-client unit tests stay green). Regression pin for #1292:
// spec.subagentModel was added to the Go types but missing from the CRD,
// so real clusters dropped it before the controller ever saw it.
func TestCRDSpecSchemasCoverSpecStructs(t *testing.T) {
	workers := loadCRDDoc(t, "../../config/crd/workers.agentteams.io.yaml")
	assertCRDPropertiesCoverStruct(t, "WorkerSpec", workers, reflect.TypeOf(WorkerSpec{}),
		"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "spec", "properties")
	teams := loadCRDDoc(t, "../../config/crd/teams.agentteams.io.yaml")
	assertCRDPropertiesCoverStruct(t, "TeamSpec", teams, reflect.TypeOf(TeamSpec{}),
		"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "spec", "properties")
	loops := loadCRDDoc(t, "../../config/crd/loops.agentteams.io.yaml")
	assertCRDPropertiesCoverStruct(t, "LoopSpec", loops, reflect.TypeOf(LoopSpec{}),
		"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "spec", "properties")
}

func loadCRDDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// assertCRDPropertiesCoverStruct walks path (with "*" selecting the first
// list element) and asserts every JSON-tagged field of rt has a property in
// the resolved schema node.
func assertCRDPropertiesCoverStruct(t *testing.T, label string, doc map[string]any, rt reflect.Type, path ...string) {
	t.Helper()
	var cur any = doc
	for _, key := range path {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[key]
		case []any:
			if key != "*" || len(node) == 0 {
				t.Fatalf("%s: cannot walk %q in a list", label, key)
			}
			cur = node[0]
		default:
			t.Fatalf("%s: cannot walk %q at %T", label, key, cur)
		}
	}
	props, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("%s: resolved node is %T, want a properties map", label, cur)
	}
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if _, ok := props[name]; !ok {
			t.Errorf("%s: CRD schema is missing property %q (a real apiserver would prune it)", label, name)
		}
	}
}

// TestLoopSpec_JSONTagsAndDefaults verifies LoopSpec marshals with stable
// lowerCamelCase JSON keys, that a minimal spec (required fields only)
// omits every optional field cleanly, and that a fully populated spec
// round-trips without value drift.
func TestLoopSpec_JSONTagsAndDefaults(t *testing.T) {
	min := LoopSpec{TaskID: "task-1", LoopType: LoopTypeCron}
	data, err := json.Marshal(min)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, sub := range []string{`"taskID":"task-1"`, `"loopType":"cron"`} {
		if !strings.Contains(got, sub) {
			t.Errorf("JSON missing %q: %s", sub, got)
		}
	}
	for _, sub := range []string{`"teamName"`, `"schedule"`, `"timeoutSeconds"`, `"maxIterations"`, `"paused"`} {
		if strings.Contains(got, sub) {
			t.Errorf("JSON should omit %q: %s", sub, got)
		}
	}

	orig := LoopSpec{
		TaskID:         "task-1",
		TeamName:       "team-a",
		LoopType:       LoopTypeInterval,
		Schedule:       "300",
		TimeoutSeconds: int32Ptr(600),
		MaxIterations:  int32Ptr(10),
		Paused:         true,
	}
	data, err = json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var gotSpec LoopSpec
	if err := json.Unmarshal(data, &gotSpec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(gotSpec, orig) {
		t.Fatalf("round trip drift: got %+v want %+v", gotSpec, orig)
	}
}

// TestLoopCRD_EnumsMatchConstants verifies the enums enforced by the
// deployed CRD schema cover the Go constants, so a value the apiserver
// accepts is always one the reconciler knows.
func TestLoopCRD_EnumsMatchConstants(t *testing.T) {
	loops := loadCRDDoc(t, "../../config/crd/loops.agentteams.io.yaml")
	assertCRDEnumCoversConstants(t, "spec.loopType",
		crdNodeAt(t, "spec.loopType", loops,
			"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "spec", "properties", "loopType"),
		[]string{LoopTypeCron, LoopTypeInterval, LoopTypeEvent})
	assertCRDEnumCoversConstants(t, "status.state",
		crdNodeAt(t, "status.state", loops,
			"spec", "versions", "*", "schema", "openAPIV3Schema", "properties", "status", "properties", "state"),
		[]string{LoopStatePending, LoopStateActive, LoopStatePaused, LoopStateCompleted, LoopStateFailed})
}

func crdNodeAt(t *testing.T, label string, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, key := range path {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[key]
		case []any:
			if key != "*" || len(node) == 0 {
				t.Fatalf("%s: cannot walk %q in a list", label, key)
			}
			cur = node[0]
		default:
			t.Fatalf("%s: cannot walk %q at %T", label, key, cur)
		}
	}
	return cur
}

func assertCRDEnumCoversConstants(t *testing.T, label string, node any, consts []string) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s: resolved node is %T, want a property map", label, node)
	}
	enum, ok := m["enum"].([]any)
	if !ok {
		t.Fatalf("%s: missing enum list in CRD schema", label)
	}
	got := map[string]bool{}
	for _, e := range enum {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s: non-string enum entry %v", label, e)
		}
		got[s] = true
	}
	for _, c := range consts {
		if !got[c] {
			t.Errorf("%s: CRD enum %v does not include constant %q", label, enum, c)
		}
	}
}

// TestLoop_DeepCopy verifies Loop's pointer, slice, and map fields are
// deep-copied: mutating the source after DeepCopy must not mutate the
// copy, and a zero spec/status must not allocate pointers on copy.
func TestLoop_DeepCopy(t *testing.T) {
	now := metav1.Time{Time: time.Now()}
	src := Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "loop-1", Labels: map[string]string{"tier": "ctrl"}},
		Spec: LoopSpec{
			TaskID:         "task-1",
			TeamName:       "team-a",
			LoopType:       LoopTypeInterval,
			Schedule:       "300",
			TimeoutSeconds: int32Ptr(600),
			MaxIterations:  int32Ptr(10),
			Paused:         true,
		},
		Status: LoopStatus{
			State:              LoopStateActive,
			CurrentIteration:   3,
			NextRunAt:          &now,
			LastRunAt:          &now,
			LastResult:         "ok",
			Conditions:         []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Scheduled", Message: "m", LastTransitionTime: now}},
			ObservedGeneration: 7,
		},
	}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp, src) {
		t.Fatalf("copy drift: got %+v want %+v", cp, src)
	}

	src.Spec.TimeoutSeconds = int32Ptr(999)
	if cp.Spec.TimeoutSeconds == nil || *cp.Spec.TimeoutSeconds != 600 {
		t.Fatalf("DeepCopy aliased LoopSpec.TimeoutSeconds: %v", cp.Spec.TimeoutSeconds)
	}
	src.Spec.MaxIterations = int32Ptr(99)
	if cp.Spec.MaxIterations == nil || *cp.Spec.MaxIterations != 10 {
		t.Fatalf("DeepCopy aliased LoopSpec.MaxIterations: %v", cp.Spec.MaxIterations)
	}
	src.Status.NextRunAt = &metav1.Time{Time: now.Add(time.Hour)}
	if !cp.Status.NextRunAt.Equal(&now) {
		t.Fatalf("DeepCopy aliased LoopStatus.NextRunAt: %v", cp.Status.NextRunAt)
	}
	src.Status.LastRunAt = &metav1.Time{Time: now.Add(time.Hour)}
	if !cp.Status.LastRunAt.Equal(&now) {
		t.Fatalf("DeepCopy aliased LoopStatus.LastRunAt: %v", cp.Status.LastRunAt)
	}
	src.Status.Conditions[0].Message = "mutated"
	if cp.Status.Conditions[0].Message != "m" {
		t.Fatalf("DeepCopy aliased LoopStatus.Conditions: %v", cp.Status.Conditions)
	}
	src.ObjectMeta.Labels["mutated"] = "x"
	if cp.ObjectMeta.Labels["mutated"] != "" {
		t.Fatalf("DeepCopy aliased Loop.ObjectMeta.Labels: %v", cp.ObjectMeta.Labels)
	}

	srcNil := Loop{Spec: LoopSpec{TaskID: "task-1", LoopType: LoopTypeCron}}
	cpNil := *srcNil.DeepCopy()
	if cpNil.Spec.TimeoutSeconds != nil || cpNil.Spec.MaxIterations != nil {
		t.Fatalf("DeepCopy allocated nil spec pointers: %+v", cpNil.Spec)
	}
	if cpNil.Status.NextRunAt != nil || cpNil.Status.LastRunAt != nil || cpNil.Status.Conditions != nil {
		t.Fatalf("DeepCopy allocated nil status pointers: %+v", cpNil.Status)
	}
}
