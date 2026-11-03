// Package openclaw is the OpenClaw runtime's native config surface (P8a):
// the extracted surface description (embedded, version-pinned to the
// OpenClaw release it was extracted from), plus the controller-side
// read/write/reconcile/rollback operations on a worker's openclaw.json.
package openclaw

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

//go:embed surface.json
var surfaceJSON []byte

//go:embed surface.allowlist.json
var allowlistJSON []byte

//go:embed zod-keys.json
var zodKeysJSON []byte

// AllowlistEntry is one zod key the surface intentionally does not declare
// (passthrough / catchall / dynamically-keyed sections), with the reason.
type AllowlistEntry struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type allowlistFile struct {
	Allowlist []AllowlistEntry `json:"allowlist"`
}

type zodKeysFile struct {
	Source string   `json:"source"`
	Note   string   `json:"note"`
	Keys   []string `json:"keys"`
}

// LoadSurface parses and validates the embedded surface description. The
// embedded files are build inputs: a load failure is a compile defect, not
// a runtime condition callers need to recover from.
func LoadSurface() (runtimeconfig.Surface, error) {
	var s runtimeconfig.Surface
	if err := json.Unmarshal(surfaceJSON, &s); err != nil {
		return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: %w", err)
	}
	if s.Runtime != "openclaw" {
		return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: runtime %q", s.Runtime)
	}
	if s.SurfaceVersion == "" {
		return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: missing surfaceVersion")
	}
	seen := map[string]struct{}{}
	for _, face := range s.Faces {
		if face.ID == "" {
			return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: face with empty id")
		}
		for _, f := range face.Fields {
			if f.Key == "" || f.Type == "" || f.Applies == "" {
				return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: incomplete field %q", f.Key)
			}
			switch f.Applies {
			case runtimeconfig.AppliesHot, runtimeconfig.AppliesNextTurn, runtimeconfig.AppliesRestart:
			default:
				return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: field %q has invalid applies %q", f.Key, f.Applies)
			}
			if !validFieldType(f.Type) {
				return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: field %q has invalid type %q", f.Key, f.Type)
			}
			if !f.Class.Valid() {
				return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: field %q has invalid class %q", f.Key, f.Class)
			}
			if _, dup := seen[f.Key]; dup {
				return runtimeconfig.Surface{}, fmt.Errorf("openclaw surface: duplicate field key %q", f.Key)
			}
			seen[f.Key] = struct{}{}
		}
	}
	return s, nil
}

// LoadAllowlist parses the embedded coverage allowlist.
func LoadAllowlist() ([]AllowlistEntry, error) {
	var f allowlistFile
	if err := json.Unmarshal(allowlistJSON, &f); err != nil {
		return nil, fmt.Errorf("openclaw allowlist: %w", err)
	}
	return f.Allowlist, nil
}

// LoadZodKeys parses the embedded snapshot of the keys extracted from the
// OpenClaw zod schema (the coverage denominator).
func LoadZodKeys() ([]string, error) {
	var f zodKeysFile
	if err := json.Unmarshal(zodKeysJSON, &f); err != nil {
		return nil, fmt.Errorf("openclaw zod-keys: %w", err)
	}
	return f.Keys, nil
}

// validFieldType is the closed vocabulary surface.json may use.
func validFieldType(t string) bool {
	switch t {
	case "boolean", "number", "string", "secret", "array", "record", "object":
		return true
	}
	return false
}
