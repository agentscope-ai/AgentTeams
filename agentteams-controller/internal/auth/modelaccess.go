package auth

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// --- Per-human model-access evaluation (PR-A1b) ---
//
// Pure functions: given a human's grant spec (one selected scope) and the
// resolved ModelAccessTable spec, compute what each model surface (main,
// spawn, coding CLIs) allows. No catalog validation, no Higress lookup,
// no K8s access — the caller loads the table and passes it in.

// ModelRef is one concrete model coordinate (provider is required).
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SurfaceAccess describes one surface's writability.
type SurfaceAccess struct {
	CanEdit bool                       `json:"canEdit"`
	Inherit bool                       `json:"inherit"`
	Entries []v1beta1.ModelAccessEntry `json:"entries,omitempty"`
	Reason  string                     `json:"reason,omitempty"` // not_granted | cli_not_granted | byok_unsupported
}

// CLIAccess is a SurfaceAccess plus the inheritance source.
type CLIAccess struct {
	SurfaceAccess
	InheritedFrom *ModelRef `json:"inheritedFrom,omitempty"`
}

// EffectiveModelAccess is one evaluation result.
type EffectiveModelAccess struct {
	Locked bool                 `json:"locked"`
	Main   SurfaceAccess        `json:"main"`
	Spawn  SurfaceAccess        `json:"spawn"`
	CLI    map[string]CLIAccess `json:"codingCli"` // always carries the qwen-code / opencode / qoder keys
}

// Denial reasons surfaced in SurfaceAccess.Reason.
const (
	ReasonNotGranted      = "not_granted"
	ReasonCLINotGranted   = "cli_not_granted"
	ReasonBYOKUnsupported = "byok_unsupported"
)

// modelAccessCLIs is the closed coding-CLI set. The response map always
// carries all three keys; qoder is BYOK-only and never editable.
var modelAccessCLIs = []string{"qwen-code", "opencode", "qoder"}

// EffectiveScope selects the scope that applies to a worker role: an
// explicit role block wins over the blanket ("*" or "") scope, and "" and
// "*" are the same scope. Returns nil when no scope matches the role.
func EffectiveScope(spec *v1beta1.ModelAccessSpec, role string) *v1beta1.ModelAccessScope {
	if spec == nil {
		return nil
	}
	var blanket *v1beta1.ModelAccessScope
	for i := range spec.Scopes {
		sc := &spec.Scopes[i]
		if sc.Role == role {
			return sc
		}
		if (sc.Role == "" || sc.Role == "*") && blanket == nil {
			blanket = sc
		}
	}
	return blanket
}

// Resolve evaluates one selected scope (nil = no grant) against the loaded
// table spec (nil = table not loaded or not referenced) into the three
// model surfaces. Scope selection (EffectiveScope) happens before this.
func Resolve(scope *v1beta1.ModelAccessScope, table *v1beta1.ModelAccessTableSpec) EffectiveModelAccess {
	var (
		mainSurf  *v1beta1.ModelAccessSurface
		spawnSurf *v1beta1.ModelAccessSurface
		clis      []v1beta1.ModelAccessCli
	)
	if scope != nil {
		if scope.Table != "" {
			// The table reference wins; inline surfaces are ignored
			// defensively (A1a validation already rejects the mix).
			if table != nil {
				mainSurf = table.Main
				spawnSurf = table.Spawn
				clis = table.CodingCli
			}
		} else {
			mainSurf = scope.Main
			spawnSurf = scope.Spawn
			clis = scope.CodingCli
		}
	}

	var mainEntries []v1beta1.ModelAccessEntry
	if mainSurf != nil {
		mainEntries = mainSurf.Entries
	}
	var spawnEntries []v1beta1.ModelAccessEntry
	spawnInherit := false
	if spawnSurf != nil {
		spawnEntries = spawnSurf.Entries
		spawnInherit = spawnSurf.Inherit != nil && *spawnSurf.Inherit
	}

	// Inheritance source for inherit-only coding CLIs: the spawn surface's
	// first entry, falling back to the primary model's first entry when
	// spawn inherits without entries of its own.
	var inheritedFrom *ModelRef
	if len(spawnEntries) > 0 {
		inheritedFrom = &ModelRef{Provider: spawnEntries[0].Provider, Model: spawnEntries[0].Model}
	} else if spawnInherit && len(mainEntries) > 0 {
		inheritedFrom = &ModelRef{Provider: mainEntries[0].Provider, Model: mainEntries[0].Model}
	}

	res := EffectiveModelAccess{
		Main:  SurfaceAccess{CanEdit: len(mainEntries) > 0, Entries: mainEntries},
		Spawn: SurfaceAccess{CanEdit: len(spawnEntries) > 0 || spawnInherit, Inherit: spawnInherit, Entries: spawnEntries},
		CLI:   make(map[string]CLIAccess, len(modelAccessCLIs)),
	}
	if !res.Main.CanEdit {
		res.Main.Reason = ReasonNotGranted
	}
	if !res.Spawn.CanEdit {
		res.Spawn.Reason = ReasonNotGranted
	}

	scoped := make(map[string]v1beta1.ModelAccessCli, len(clis))
	for _, c := range clis {
		scoped[c.CLI] = c
	}
	for _, cli := range modelAccessCLIs {
		if cli == "qoder" {
			// BYOK-only: never granted by this API, even when the scope
			// names it explicitly.
			res.CLI[cli] = CLIAccess{SurfaceAccess: SurfaceAccess{Reason: ReasonBYOKUnsupported}}
			continue
		}
		c, ok := scoped[cli]
		if !ok {
			res.CLI[cli] = CLIAccess{SurfaceAccess: SurfaceAccess{Reason: ReasonCLINotGranted}}
			continue
		}
		acc := CLIAccess{SurfaceAccess: SurfaceAccess{Entries: c.Entries}}
		inherit := c.Inherit != nil && *c.Inherit
		switch {
		case len(c.Entries) > 0:
			// inherit stays reportable next to explicit rows: the caller
			// may leave the value empty and follow the spawn model.
			acc.CanEdit = true
			acc.Inherit = inherit
		case inherit && inheritedFrom != nil:
			acc.CanEdit = true
			acc.Inherit = true
			acc.InheritedFrom = inheritedFrom
		default:
			// inherit=true but nothing to inherit from (spawn and main
			// both empty), or the CLI was granted with neither entries
			// nor inherit.
			acc.Reason = ReasonNotGranted
		}
		res.CLI[cli] = acc
	}

	res.Locked = !res.Main.CanEdit && !res.Spawn.CanEdit
	for _, acc := range res.CLI {
		if acc.CanEdit {
			res.Locked = false
		}
	}
	return res
}
