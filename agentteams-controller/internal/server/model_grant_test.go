package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- PR-B1: pure grant-set functions ---

func entry(provider, model string) v1beta1.ModelAccessEntry {
	return v1beta1.ModelAccessEntry{Provider: provider, Model: model}
}

func TestGrantAllows_MatchingRule(t *testing.T) {
	// Entry provider empty: matches any requested provider (model only).
	if !grantAllows([]v1beta1.ModelAccessEntry{entry("", "m1")}, "any-provider", "m1") {
		t.Errorf("empty entry provider should match on model only")
	}
	// Requested provider empty: the entry's provider is not consulted.
	if !grantAllows([]v1beta1.ModelAccessEntry{entry("gw", "m1")}, "", "m1") {
		t.Errorf("empty requested provider should match on model only")
	}
	// Both providers present and equal: match.
	if !grantAllows([]v1beta1.ModelAccessEntry{entry("gw", "m1")}, "gw", "m1") {
		t.Errorf("equal providers should match")
	}
	// Both providers present and different: no match even on equal model.
	if grantAllows([]v1beta1.ModelAccessEntry{entry("gw", "m1")}, "other", "m1") {
		t.Errorf("different non-empty providers must not match")
	}
	// Model mismatch always loses, regardless of providers.
	if grantAllows([]v1beta1.ModelAccessEntry{entry("gw", "m1")}, "gw", "m2") {
		t.Errorf("model mismatch must not match")
	}
	if grantAllows(nil, "gw", "m1") {
		t.Errorf("empty entry list must not match")
	}
}

func TestMergeGrantEntries_OrderDedupe(t *testing.T) {
	explicit := []v1beta1.ModelAccessEntry{entry("gw", "m1"), entry("gw", "m2")}
	implicit := []v1beta1.ModelAccessEntry{entry("gw", "m2"), entry("", "m3")}
	current := []v1beta1.ModelAccessEntry{entry("gw", "m1"), entry("gw", "m4")}

	merged := mergeGrantEntries(explicit, implicit, current)
	want := []v1beta1.ModelAccessEntry{
		entry("gw", "m1"), // explicit (first)
		entry("gw", "m2"), // explicit (implicit duplicate skipped)
		entry("", "m3"),   // implicit
		entry("gw", "m4"), // current (last)
	}
	if len(merged) != len(want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	for i := range want {
		if merged[i] != want[i] {
			t.Fatalf("merged[%d] = %v, want %v (full: %v)", i, merged[i], want[i], merged)
		}
	}
	// No explicit/implicit rows at all: only the current values survive.
	only := mergeGrantEntries(nil, nil, current)
	if len(only) != 2 || only[0] != entry("gw", "m1") || only[1] != entry("gw", "m4") {
		t.Errorf("current-only merge = %v, want both current pairs", only)
	}
	// Everything empty: a (non-nil) empty result, never nil.
	empty := mergeGrantEntries(nil, nil, nil)
	if len(empty) != 0 {
		t.Errorf("empty merge = %v, want []", empty)
	}
}

// TestMergeGrantEntries_ExplicitRowsNeverTruncated is the maintainer's
// regression (#1514 P2, 11-05): the union of explicit rows, implicit
// grants and the current value must never be truncated. modelGrantMax (32)
// bounds the implicit annotation only — the old union-wide cap dropped the
// 33rd explicit row and turned it into a model_not_allowed denial.
func TestMergeGrantEntries_ExplicitRowsNeverTruncated(t *testing.T) {
	// 33 explicit rows {provider:p, model:m0..m32}: all survive, and the
	// last row (m32) stays admissible through grantAllows.
	regression := make([]v1beta1.ModelAccessEntry, 0, 33)
	for i := 0; i <= 32; i++ {
		regression = append(regression, entry("p", fmt.Sprintf("m%d", i)))
	}
	merged := mergeGrantEntries(regression)
	if len(merged) != 33 {
		t.Fatalf("33 explicit rows merged to %d entries, want all 33", len(merged))
	}
	if !grantAllows(merged, "p", "m32") {
		t.Fatalf("m32 must stay allowed after the merge; merged = %v", merged)
	}

	// 200 explicit rows (the validateModelAccessSurfaceEntries ceiling).
	ceiling := make([]v1beta1.ModelAccessEntry, 0, 200)
	for i := 0; i < 200; i++ {
		ceiling = append(ceiling, entry("p", fmt.Sprintf("c%d", i)))
	}
	if got := mergeGrantEntries(ceiling); len(got) != 200 {
		t.Fatalf("200 explicit rows merged to %d entries, want all 200", len(got))
	}

	// Stacked: 32 implicit + 200 explicit + the current value. Explicit
	// and current rows must survive in full; a colliding pair dedupes.
	implicit := make([]v1beta1.ModelAccessEntry, 0, modelGrantMax)
	for i := 0; i < modelGrantMax; i++ {
		implicit = append(implicit, entry("imp", fmt.Sprintf("i%d", i)))
	}
	current := []v1beta1.ModelAccessEntry{entry("agentteams-gateway", "now")}
	merged = mergeGrantEntries(ceiling, implicit, current)
	if len(merged) != 200+modelGrantMax+1 {
		t.Fatalf("stacked merge = %d entries, want %d (200 explicit + 32 implicit + 1 current)",
			len(merged), 200+modelGrantMax+1)
	}
	if merged[0] != entry("p", "c0") || merged[199] != entry("p", "c199") {
		t.Fatalf("explicit rows not preserved in order: first=%v last=%v", merged[0], merged[199])
	}
	if merged[200] != entry("imp", "i0") || merged[200+modelGrantMax-1] != entry("imp", "i31") {
		t.Fatalf("implicit rows not preserved: first=%v last=%v", merged[200], merged[200+modelGrantMax-1])
	}
	if merged[len(merged)-1] != entry("agentteams-gateway", "now") {
		t.Fatalf("current value must survive the merge, last=%v", merged[len(merged)-1])
	}
}

// implicitAnnotationJSON renders n distinct implicit grant rows as the
// annotation value (provider "imp", models i0..i(n-1)).
func implicitAnnotationJSON(n int) string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, fmt.Sprintf(`{"provider":"imp","model":"i%d"}`, i))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

// workerWithImplicitAnnotation builds a worker whose implicit grant
// annotation carries exactly n rows.
func workerWithImplicitAnnotation(t *testing.T, n int) *v1beta1.Worker {
	t.Helper()
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "w1",
			Annotations: map[string]string{modelGrantAnnotation: implicitAnnotationJSON(n)}},
	}
}

// TestCheckImplicitGrantCap_Boundary: 32 implicit grants pass, 33 are
// rejected with the count in the message (fail closed — never a silent
// truncation of the effective set).
func TestCheckImplicitGrantCap_Boundary(t *testing.T) {
	for n, wantErr := range map[int]bool{
		modelGrantMax:     false,
		modelGrantMax + 1: true,
	} {
		w := workerWithImplicitAnnotation(t, n)
		err := checkImplicitGrantCap(w)
		if !wantErr {
			if err != nil {
				t.Fatalf("n=%d: unexpected error: %v", n, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("n=%d: expected an over-cap error, got nil", n)
		}
		if !errors.Is(err, errModelGrantOverflow) {
			t.Fatalf("n=%d: error must wrap errModelGrantOverflow: %v", n, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, fmt.Sprintf("carries %d implicit", n)) ||
			!strings.Contains(msg, fmt.Sprintf("cap is %d", modelGrantMax)) {
			t.Errorf("n=%d: error must carry the actual count and the cap: %s", n, msg)
		}
	}
}

// TestBuildModelGrantSet_ImplicitCapFailClosed: an over-cap annotation
// rejects the whole set build (preview and write paths share it); at the
// cap the build succeeds and keeps every explicit row.
func TestBuildModelGrantSet_ImplicitCapFailClosed(t *testing.T) {
	if _, err := buildModelGrantSet(nil, nil, workerWithImplicitAnnotation(t, modelGrantMax+1)); err == nil ||
		!errors.Is(err, errModelGrantOverflow) {
		t.Fatalf("33 implicit: want errModelGrantOverflow, got %v", err)
	}

	entries := make([]v1beta1.ModelAccessEntry, 0, 33)
	for i := 0; i <= 32; i++ {
		entries = append(entries, entry("p", fmt.Sprintf("m%d", i)))
	}
	grant := &v1beta1.ModelAccessScope{Main: &v1beta1.ModelAccessSurface{Entries: entries}}
	s, err := buildModelGrantSet(grant, nil, workerWithImplicitAnnotation(t, modelGrantMax))
	if err != nil {
		t.Fatalf("32 implicit + 33 explicit: unexpected error: %v", err)
	}
	// 33 explicit + 32 implicit (no current value: the worker has no model).
	if len(s.main) != 33+modelGrantMax {
		t.Fatalf("main = %d entries, want %d", len(s.main), 33+modelGrantMax)
	}
	if !grantAllows(s.main, "p", "m32") {
		t.Fatalf("m32 lost in the effective set: %v", s.main)
	}
}

// readModelGrants + appendModelGrant: annotation round-trip, dedupe and
// the 32-entry cap (overflow appends are skipped, the list never grows).
func TestModelGrantAnnotation_DedupeAndCap(t *testing.T) {
	h := &ResourceHandler{}
	w := &v1beta1.Worker{}

	h.appendModelGrant(context.Background(), w, "gw", "m1")
	h.appendModelGrant(context.Background(), w, "gw", "m1") // duplicate
	h.appendModelGrant(context.Background(), w, "", "m1")   // different pair (provider "")
	if got := readModelGrants(w); len(got) != 2 || got[0] != entry("gw", "m1") || got[1] != entry("", "m1") {
		t.Fatalf("annotation after appends = %v, want the 2 distinct pairs", got)
	}

	for i := 2; i < modelGrantMax; i++ {
		h.appendModelGrant(context.Background(), w, "gw", fmt.Sprintf("fill%d", i))
	}
	if got := readModelGrants(w); len(got) != modelGrantMax {
		t.Fatalf("annotation at cap = %d entries, want %d", len(got), modelGrantMax)
	}
	h.appendModelGrant(context.Background(), w, "gw", "overflow") // skipped
	if got := readModelGrants(w); len(got) != modelGrantMax || got[len(got)-1] != entry("gw", fmt.Sprintf("fill%d", modelGrantMax-1)) {
		t.Fatalf("overflow append must be skipped: %d entries, last=%v", len(got), got[len(got)-1])
	}
	// Malformed JSON degrades to an empty list (fail-closed).
	w.Annotations[modelGrantAnnotation] = `{"not":"an array"}`
	if got := readModelGrants(w); len(got) != 0 {
		t.Errorf("malformed annotation = %v, want empty", got)
	}
}

// TestRevocationContract_AnnotationAndCurrentSurvive pins the revocation
// contract: revoking the Human's explicit scope removes explicit rows
// immediately, but the L1-issued historical annotation and the worker's
// current value stay selectable. "Keep running the current value" and
// "re-select a value L1 previously granted" are the same grant source
// (the annotation), distinct from the revoked explicit scope; a value that
// was never granted is denied.
func TestRevocationContract_AnnotationAndCurrentSurvive(t *testing.T) {
	w := &v1beta1.Worker{}
	w.Spec.Model = "m-current"
	w.Spec.ModelProvider = "gw"
	w.Annotations = map[string]string{
		modelGrantAnnotation: `[{"provider":"gw","model":"m-old"}]`,
	}

	s, err := buildModelGrantSet(nil, nil, w)
	if err != nil {
		t.Fatalf("buildModelGrantSet: %v", err)
	}
	if !grantAllows(s.main, "gw", "m-current") {
		t.Errorf("current value must stay selectable after revocation")
	}
	if !grantAllows(s.main, "gw", "m-old") {
		t.Errorf("L1-issued annotation must stay selectable after revocation")
	}
	if grantAllows(s.main, "gw", "m-never-granted") {
		t.Errorf("a value that was never granted must be denied")
	}
}
