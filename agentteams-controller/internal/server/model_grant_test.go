package server

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
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

func TestMergeGrantEntries_OrderDedupeCap(t *testing.T) {
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
	// No explicit/implicit rows at all: only the current value survives.
	only := mergeGrantEntries(nil, nil, current)
	if len(only) != 1 || only[0] != entry("gw", "m1") {
		t.Errorf("current-only merge = %v, want the current pair", only)
	}
	// Everything empty: a (non-nil) empty result, never nil.
	empty := mergeGrantEntries(nil, nil, nil)
	if len(empty) != 0 {
		t.Errorf("empty merge = %v, want []", empty)
	}
}

func TestMergeGrantEntries_Cap32(t *testing.T) {
	group := make([]v1beta1.ModelAccessEntry, 0, 40)
	for i := 0; i < 40; i++ {
		group = append(group, entry("gw", "m"+string(rune('A'+i/26))+string(rune('a'+i%26))))
	}
	merged := mergeGrantEntries(group)
	if len(merged) != modelGrantMax {
		t.Fatalf("capped merge = %d entries, want %d", len(merged), modelGrantMax)
	}
	// First-32 win: order preserved from the first group.
	for i := 0; i < modelGrantMax; i++ {
		if merged[i] != group[i] {
			t.Fatalf("capped merge[%d] = %v, want %v", i, merged[i], group[i])
		}
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
		h.appendModelGrant(context.Background(), w, "gw", "fill"+string(rune('a'+i)))
	}
	if got := readModelGrants(w); len(got) != modelGrantMax {
		t.Fatalf("annotation at cap = %d entries, want %d", len(got), modelGrantMax)
	}
	h.appendModelGrant(context.Background(), w, "gw", "overflow") // skipped
	if got := readModelGrants(w); len(got) != modelGrantMax || got[len(got)-1] != entry("gw", "filly") {
		t.Fatalf("overflow append must be skipped: %d entries, last=%v", len(got), got[len(got)-1])
	}
	// Malformed JSON degrades to an empty list (fail-closed).
	w.Annotations[modelGrantAnnotation] = `{"not":"an array"}`
	if got := readModelGrants(w); len(got) != 0 {
		t.Errorf("malformed annotation = %v, want empty", got)
	}
}
