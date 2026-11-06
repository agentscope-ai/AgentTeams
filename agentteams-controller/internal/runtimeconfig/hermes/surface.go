// Package hermes declares the native configuration surface of the Hermes
// runtime (hermes_cli config.yaml, _config_version 51 at the time of
// writing). It never translates Hermes fields into another runtime's
// vocabulary; the controller passes the surface through unchanged.
package hermes

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// Wildcard is the path segment used by keys that address one member of a
// dynamic map, e.g. "providers.*.api_key" for providers.<name>.api_key.
const Wildcard = "*"

//go:embed surface.json
var surfaceJSON []byte

var (
	once      sync.Once
	cached    runtimeconfig.Surface
	cachedErr error
)

// validApplies mirrors the Applies vocabulary in the frozen
// internal/runtimeconfig types; a drifted word here would mean the surface
// file and the platform types no longer agree.
var validApplies = map[string]bool{
	"hot":       true,
	"next-turn": true,
	"restart":   true,
}

// validTypes mirrors the Field.Type vocabulary ("string|int|float|bool|
// enum|string-list|object") documented in the frozen types.
var validTypes = map[string]bool{
	"string":      true,
	"int":         true,
	"float":       true,
	"bool":        true,
	"enum":        true,
	"string-list": true,
	"object":      true,
}

// Surface returns the parsed and validated Hermes surface. The error is
// non-nil only when the embedded surface.json is malformed, which is a build
// defect, not a runtime condition.
func Surface() (runtimeconfig.Surface, error) {
	once.Do(func() {
		cached, cachedErr = parseSurface(surfaceJSON)
	})
	return cached, cachedErr
}

func parseSurface(data []byte) (runtimeconfig.Surface, error) {
	var s runtimeconfig.Surface
	if err := json.Unmarshal(data, &s); err != nil {
		return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: %w", err)
	}
	if s.Runtime != "hermes" {
		return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: runtime = %q, want hermes", s.Runtime)
	}
	if s.SurfaceVersion == "" {
		return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: empty surfaceVersion")
	}
	seenFace := map[string]bool{}
	seenKey := map[string]string{} // key -> owning face id
	for _, face := range s.Faces {
		if face.ID == "" {
			return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: face with empty id")
		}
		if seenFace[face.ID] {
			return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: duplicate face %q", face.ID)
		}
		seenFace[face.ID] = true
		for _, fld := range face.Fields {
			if fld.Key == "" {
				return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: field with empty key in face %q", face.ID)
			}
			// A field either names its face exactly (flat top-level keys such
			// as "model") or hangs off the face id with a dot.
			if fld.Key != face.ID && !strings.HasPrefix(fld.Key, face.ID+".") {
				return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: field %q does not belong to face %q", fld.Key, face.ID)
			}
			if !validTypes[fld.Type] {
				return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: field %q has unknown type %q", fld.Key, fld.Type)
			}
			if !validApplies[string(fld.Applies)] {
				return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: field %q has unknown applies %q", fld.Key, fld.Applies)
			}
			if !fld.Class.Valid() {
				return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: field %q has invalid class %q", fld.Key, fld.Class)
			}
			if faceID, dup := seenKey[fld.Key]; dup {
				return runtimeconfig.Surface{}, fmt.Errorf("hermes surface: key %q declared by both faces %q and %q", fld.Key, faceID, face.ID)
			}
			seenKey[fld.Key] = face.ID
		}
	}
	return s, nil
}

// MatchKey reports whether a concrete config key matches a declared field
// key pattern. Segments compare position by position; the pattern segment
// "*" stands for exactly one non-empty key segment, and a pattern may carry
// at most one such segment. A pattern without wildcards matches only
// itself.
func MatchKey(pattern, key string) bool {
	p := strings.Split(pattern, ".")
	k := strings.Split(key, ".")
	if len(p) != len(k) {
		return false
	}
	wildcards := 0
	for i, seg := range p {
		switch {
		case seg == Wildcard:
			wildcards++
			if k[i] == "" {
				return false
			}
		case seg != k[i]:
			return false
		}
	}
	return wildcards <= 1
}

// FieldFor resolves a concrete config key to its declaring field, following
// the platform's wildcard convention for dynamic-map faces (providers,
// mcp_servers, moa.presets). Exact declared keys win over patterns; the
// frozen Surface.Field remains exact-match only, so pattern lookup lives
// here.
func FieldFor(s runtimeconfig.Surface, key string) (runtimeconfig.Field, bool) {
	if fld, ok := s.Field(key); ok {
		return fld, true
	}
	for _, face := range s.Faces {
		for _, fld := range face.Fields {
			if strings.Contains(fld.Key, Wildcard) && MatchKey(fld.Key, key) {
				return fld, true
			}
		}
	}
	return runtimeconfig.Field{}, false
}
