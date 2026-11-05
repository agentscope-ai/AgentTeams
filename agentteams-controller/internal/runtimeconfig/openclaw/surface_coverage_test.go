package openclaw

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// TestOpenClawSurfaceCoverage is the P8a coverage gate: the extracted
// surface (surface.json) is checked against the zod-schema key snapshot
// (zod-keys.json), with the allowlist (surface.allowlist.json) as the only
// admitted escape hatch. Five assertions:
//
//	(a) every snapshot key is either covered by a declared field or by an
//	    allowlist entry whose reason is non-empty — the coverage delta must
//	    be explained, never silent;
//	(b) every declared field key exists in the snapshot (exact match);
//	(c) every allowlist key exists in the snapshot (exact match);
//	(d) every field's applies value is in the closed vocabulary and field
//	    keys are unique across all faces;
//	(e) every field's class is in the closed vocabulary, and the model
//	    selection plane is classified exactly once.
//
// Coverage is literal string matching: `*` is a snapshot placeholder for a
// dynamically-keyed section, not a glob. Field `storage.locations.*.provider`
// covers the identically spelled snapshot key; allowlist root `channels`
// covers every `channels.*` snapshot key by prefix.
func TestOpenClawSurfaceCoverage(t *testing.T) {
	// Parse surface.json directly (not via LoadSurface) so assertion (d)
	// is tested here rather than subsumed by the loader's validation.
	var s runtimeconfig.Surface
	if err := json.Unmarshal(surfaceJSON, &s); err != nil {
		t.Fatalf("unmarshal surface.json: %v", err)
	}
	allowlist, err := LoadAllowlist()
	if err != nil {
		t.Fatalf("load allowlist: %v", err)
	}
	zodKeys, err := LoadZodKeys()
	if err != nil {
		t.Fatalf("load zod keys: %v", err)
	}

	zodSet := make(map[string]struct{}, len(zodKeys))
	for _, k := range zodKeys {
		zodSet[k] = struct{}{}
	}

	var fields []runtimeconfig.Field
	for _, face := range s.Faces {
		fields = append(fields, face.Fields...)
	}
	t.Logf("openclaw surface: %d faces, %d fields, %d zod-snapshot keys, %d allowlist entries",
		len(s.Faces), len(fields), len(zodKeys), len(allowlist))

	// (a) every snapshot key is field-covered or allowlist-covered.
	for _, k := range zodKeys {
		if fieldCovers(fields, k) {
			continue
		}
		if a, ok := allowlistCover(allowlist, k); !ok || a.Reason == "" {
			t.Errorf("(a) zod key %q is covered by no field and by no allowlist entry with a reason", k)
		}
	}

	// (b) every declared field key must exist in the snapshot.
	for _, f := range fields {
		if _, ok := zodSet[f.Key]; !ok {
			t.Errorf("(b) declared field key %q is not present in the zod-key snapshot", f.Key)
		}
	}

	// (c) every allowlist key must exist in the snapshot.
	for _, a := range allowlist {
		if _, ok := zodSet[a.Key]; !ok {
			t.Errorf("(c) allowlist key %q is not present in the zod-key snapshot", a.Key)
		}
	}

	// (d) applies vocabulary + key uniqueness across all faces.
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		switch f.Applies {
		case runtimeconfig.AppliesHot, runtimeconfig.AppliesNextTurn, runtimeconfig.AppliesRestart:
		default:
			t.Errorf("(d) field %q has applies %q (want hot|next-turn|restart)", f.Key, f.Applies)
		}
		if _, dup := seen[f.Key]; dup {
			t.Errorf("(d) duplicate field key %q", f.Key)
		}
		seen[f.Key] = struct{}{}
	}

	// (e) every field's class is in the closed vocabulary, and the model
	// selection plane is classified exactly once: 2 selection keys,
	// 39 definition keys, 1 policy key.
	classes := map[runtimeconfig.Class]int{}
	for _, f := range fields {
		if !f.Class.Valid() {
			t.Errorf("field %q has invalid class %q", f.Key, f.Class)
		}
		classes[f.Class]++
	}
	if got, want := classes[runtimeconfig.ClassSelection], 2; got != want {
		t.Errorf("selection-classed keys = %d, want %d", got, want)
	}
	if got, want := classes[runtimeconfig.ClassDefinition], 39; got != want {
		t.Errorf("definition-classed keys = %d, want %d", got, want)
	}
	if got, want := classes[runtimeconfig.ClassPolicy], 1; got != want {
		t.Errorf("policy-classed keys = %d, want %d", got, want)
	}
}

// fieldCovers reports whether K is exactly a declared field key or lies
// below one (dot-separated prefix, literal `*` segments included).
func fieldCovers(fields []runtimeconfig.Field, k string) bool {
	for _, f := range fields {
		if k == f.Key || strings.HasPrefix(k, f.Key+".") {
			return true
		}
	}
	return false
}

// allowlistCover returns the first allowlist entry covering K (exact or
// dot-separated prefix), mirroring fieldCovers.
func allowlistCover(allowlist []AllowlistEntry, k string) (AllowlistEntry, bool) {
	for _, a := range allowlist {
		if k == a.Key || strings.HasPrefix(k, a.Key+".") {
			return a, true
		}
	}
	return AllowlistEntry{}, false
}
