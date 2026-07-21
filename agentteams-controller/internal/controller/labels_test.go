package controller

import (
	"reflect"
	"testing"
)

// TestMergeLabels_PriorityLowToHigh verifies later layers overwrite
// earlier ones on key collision. This is the load-bearing invariant for
// the four-layer Pod-label precedence: reverse the order and reserved
// keys like agentteams.io/controller would be forgeable by user CR labels.
func TestMergeLabels_PriorityLowToHigh(t *testing.T) {
	got := mergeLabels(
		map[string]string{"team": "a", "env": "dev"}, // metadata
		map[string]string{"team": "b"},               // spec (overrides metadata)
		map[string]string{"team": "c", "app": "x"},   // system (overrides spec)
	)
	want := map[string]string{"team": "c", "env": "dev", "app": "x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeLabels priority: got %v want %v", got, want)
	}
}

// TestMergeLabels_NilAndEmptyLayersSkipped verifies nil and empty maps
// are no-ops (important: call sites pass CR fields that are often nil,
// and treating a nil layer as "clear all" would wipe earlier layers).
func TestMergeLabels_NilAndEmptyLayersSkipped(t *testing.T) {
	got := mergeLabels(
		nil,
		map[string]string{},
		map[string]string{"k": "v"},
		nil,
	)
	want := map[string]string{"k": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeLabels skip: got %v want %v", got, want)
	}
}

// TestMergeLabels_AllNilReturnsNil ensures the helper does not allocate
// an empty map when every layer is nil/empty — this keeps
// CreateRequest.Labels nil (rather than an empty map) in the degenerate
// case so downstream serializers do not emit `labels: {}`.
func TestMergeLabels_AllNilReturnsNil(t *testing.T) {
	if got := mergeLabels(nil, nil, map[string]string{}); got != nil {
		t.Fatalf("mergeLabels all-nil: got %v want nil", got)
	}
}

// TestMergeLabels_DoesNotMutateInputs is the safety net for every call
// site: reconcilers hand in CR-owned maps (w.ObjectMeta.Labels etc.)
// and any in-place mutation would corrupt the shared informer cache.
func TestMergeLabels_DoesNotMutateInputs(t *testing.T) {
	a := map[string]string{"k": "a"}
	b := map[string]string{"k": "b"}
	_ = mergeLabels(a, b)
	if a["k"] != "a" || b["k"] != "b" {
		t.Fatalf("mergeLabels mutated inputs: a=%v b=%v", a, b)
	}
}
