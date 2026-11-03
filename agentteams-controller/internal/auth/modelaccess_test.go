package auth

import (
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// --- PR-A1b: model access evaluator tests ---

func maBool(b bool) *bool { return &b }

func maEntry(provider, model string) v1beta1.ModelAccessEntry {
	return v1beta1.ModelAccessEntry{Provider: provider, Model: model}
}

func TestModelAccessEffectiveScope(t *testing.T) {
	mainA := &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("p", "a")}}
	mainB := &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("p", "b")}}

	cases := []struct {
		name   string
		scopes []v1beta1.ModelAccessScope
		role   string
		// wantModel is the first main entry's model; "" means no scope applies.
		wantModel string
	}{
		{"no spec", nil, "worker", ""},
		{"empty spec", []v1beta1.ModelAccessScope{}, "worker", ""},
		{"blanket star applies to worker", []v1beta1.ModelAccessScope{{Role: "*", Main: mainA}}, "worker", "a"},
		{"empty role equals star", []v1beta1.ModelAccessScope{{Role: "", Main: mainA}}, "team_leader", "a"},
		{"explicit worker wins over star", []v1beta1.ModelAccessScope{{Role: "*", Main: mainA}, {Role: "worker", Main: mainB}}, "worker", "b"},
		{"star covers team_leader when only worker is explicit", []v1beta1.ModelAccessScope{{Role: "*", Main: mainA}, {Role: "worker", Main: mainB}}, "team_leader", "a"},
		{"worker-only scope does not cover team_leader", []v1beta1.ModelAccessScope{{Role: "worker", Main: mainB}}, "team_leader", ""},
		{"explicit team_leader block", []v1beta1.ModelAccessScope{{Role: "team_leader", Main: mainB}}, "team_leader", "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var spec *v1beta1.ModelAccessSpec
			if tc.scopes != nil {
				spec = &v1beta1.ModelAccessSpec{Scopes: tc.scopes}
			}
			sc := EffectiveScope(spec, tc.role)
			if tc.wantModel == "" {
				if sc != nil {
					t.Fatalf("scope = %+v, want nil", sc)
				}
				return
			}
			if sc == nil || sc.Main == nil || len(sc.Main.Entries) != 1 || sc.Main.Entries[0].Model != tc.wantModel {
				t.Fatalf("scope = %+v, want main entry %q", sc, tc.wantModel)
			}
		})
	}
}

// maCLIWant is the expected evaluation of one coding CLI key.
type maCLIWant struct {
	canEdit    bool
	inherit    bool
	reason     string
	from       *ModelRef
	entryCount int
}

func maDeniedCLIs() map[string]maCLIWant {
	return map[string]maCLIWant{
		"qwen-code": {reason: ReasonCLINotGranted},
		"opencode":  {reason: ReasonCLINotGranted},
		"qoder":     {reason: ReasonBYOKUnsupported},
	}
}

func TestModelAccessResolve(t *testing.T) {
	sharedTable := &v1beta1.ModelAccessTableSpec{
		Main:  &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("tp", "tm")}},
		Spawn: &v1beta1.ModelAccessSurface{Inherit: maBool(true)},
	}

	cases := []struct {
		name         string
		scope        *v1beta1.ModelAccessScope
		table        *v1beta1.ModelAccessTableSpec
		locked       bool
		mainEdit     bool
		mainModel    string
		spawnEdit    bool
		spawnInherit bool
		cli          map[string]maCLIWant
	}{
		{
			name:   "nil scope is fully locked",
			locked: true,
			cli:    maDeniedCLIs(),
		},
		{
			name:      "inline main grant",
			scope:     &v1beta1.ModelAccessScope{Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("mp", "mm")}}},
			locked:    false,
			mainEdit:  true,
			mainModel: "mm",
			cli:       maDeniedCLIs(),
		},
		{
			name: "table wins over inline",
			scope: &v1beta1.ModelAccessScope{
				Table: "shared",
				// Inline surfaces must be ignored when the table is loaded.
				Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("inline-p", "inline-m")}},
			},
			table:        sharedTable,
			locked:       false,
			mainEdit:     true,
			mainModel:    "tm",
			spawnEdit:    true,
			spawnInherit: true,
			cli:          maDeniedCLIs(),
		},
		{
			name:   "table reference with missing table fails closed",
			scope:  &v1beta1.ModelAccessScope{Table: "shared"},
			locked: true,
			cli:    maDeniedCLIs(),
		},
		{
			name: "spawn inherit without entries is editable",
			scope: &v1beta1.ModelAccessScope{
				Main:  &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("mp", "mm")}},
				Spawn: &v1beta1.ModelAccessSurface{Inherit: maBool(true)},
			},
			locked:       false,
			mainEdit:     true,
			mainModel:    "mm",
			spawnEdit:    true,
			spawnInherit: true,
			cli:          maDeniedCLIs(),
		},
		{
			name: "cli inherit takes the spawn first entry",
			scope: &v1beta1.ModelAccessScope{
				Main:  &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("mp", "mm")}},
				Spawn: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("sp", "s1"), maEntry("sp", "s2")}},
				CodingCli: []v1beta1.ModelAccessCli{
					{CLI: "qwen-code", Inherit: maBool(true)},
				},
			},
			locked:    false,
			mainEdit:  true,
			spawnEdit: true,
			cli: map[string]maCLIWant{
				"qwen-code": {canEdit: true, inherit: true, from: &ModelRef{Provider: "sp", Model: "s1"}},
				"opencode":  {reason: ReasonCLINotGranted},
				"qoder":     {reason: ReasonBYOKUnsupported},
			},
		},
		{
			name: "cli inherit falls back to the main first entry",
			scope: &v1beta1.ModelAccessScope{
				Main:  &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{maEntry("mp", "mm")}},
				Spawn: &v1beta1.ModelAccessSurface{Inherit: maBool(true)},
				CodingCli: []v1beta1.ModelAccessCli{
					{CLI: "qwen-code", Inherit: maBool(true)},
				},
			},
			locked:       false,
			mainEdit:     true,
			spawnEdit:    true,
			spawnInherit: true,
			cli: map[string]maCLIWant{
				"qwen-code": {canEdit: true, inherit: true, from: &ModelRef{Provider: "mp", Model: "mm"}},
				"opencode":  {reason: ReasonCLINotGranted},
				"qoder":     {reason: ReasonBYOKUnsupported},
			},
		},
		{
			name: "cli inherit with nothing to inherit is not granted",
			scope: &v1beta1.ModelAccessScope{
				CodingCli: []v1beta1.ModelAccessCli{
					{CLI: "qwen-code", Inherit: maBool(true)},
				},
			},
			locked: true,
			cli: map[string]maCLIWant{
				"qwen-code": {reason: ReasonNotGranted},
				"opencode":  {reason: ReasonCLINotGranted},
				"qoder":     {reason: ReasonBYOKUnsupported},
			},
		},
		{
			name: "qoder is never editable even when granted",
			scope: &v1beta1.ModelAccessScope{
				CodingCli: []v1beta1.ModelAccessCli{
					{CLI: "qoder", Entries: []v1beta1.ModelAccessEntry{maEntry("qp", "qm")}},
				},
			},
			locked: true,
			cli: map[string]maCLIWant{
				"qwen-code": {reason: ReasonCLINotGranted},
				"opencode":  {reason: ReasonCLINotGranted},
				"qoder":     {reason: ReasonBYOKUnsupported},
			},
		},
		{
			name: "cli explicit entries are editable",
			scope: &v1beta1.ModelAccessScope{
				CodingCli: []v1beta1.ModelAccessCli{
					{CLI: "opencode", Entries: []v1beta1.ModelAccessEntry{maEntry("op", "om")}},
				},
			},
			locked: false,
			cli: map[string]maCLIWant{
				"qwen-code": {reason: ReasonCLINotGranted},
				"opencode":  {canEdit: true, entryCount: 1},
				"qoder":     {reason: ReasonBYOKUnsupported},
			},
		},
		{
			name: "all empty surfaces stay locked",
			scope: &v1beta1.ModelAccessScope{
				Role:      "worker",
				Main:      &v1beta1.ModelAccessSurface{},
				Spawn:     &v1beta1.ModelAccessSurface{Inherit: maBool(false)},
				CodingCli: []v1beta1.ModelAccessCli{{CLI: "qwen-code"}},
			},
			locked: true,
			cli: map[string]maCLIWant{
				"qwen-code": {reason: ReasonNotGranted},
				"opencode":  {reason: ReasonCLINotGranted},
				"qoder":     {reason: ReasonBYOKUnsupported},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Resolve(tc.scope, tc.table)
			if res.Locked != tc.locked {
				t.Errorf("locked = %v, want %v", res.Locked, tc.locked)
			}
			if res.Main.CanEdit != tc.mainEdit {
				t.Errorf("main.canEdit = %v, want %v", res.Main.CanEdit, tc.mainEdit)
			}
			if tc.mainModel != "" {
				if len(res.Main.Entries) == 0 || res.Main.Entries[0].Model != tc.mainModel {
					t.Errorf("main.entries = %+v, want first model %q", res.Main.Entries, tc.mainModel)
				}
			}
			if res.Main.Inherit {
				t.Errorf("main.inherit = true, want false (the primary model never inherits)")
			}
			if res.Spawn.CanEdit != tc.spawnEdit {
				t.Errorf("spawn.canEdit = %v, want %v", res.Spawn.CanEdit, tc.spawnEdit)
			}
			if res.Spawn.Inherit != tc.spawnInherit {
				t.Errorf("spawn.inherit = %v, want %v", res.Spawn.Inherit, tc.spawnInherit)
			}
			if len(res.CLI) != len(modelAccessCLIs) {
				t.Fatalf("codingCli keys = %d, want %d: %+v", len(res.CLI), len(modelAccessCLIs), res.CLI)
			}
			for cli, want := range tc.cli {
				got, ok := res.CLI[cli]
				if !ok {
					t.Fatalf("codingCli missing key %q", cli)
				}
				if got.CanEdit != want.canEdit {
					t.Errorf("%s.canEdit = %v, want %v", cli, got.CanEdit, want.canEdit)
				}
				if got.Inherit != want.inherit {
					t.Errorf("%s.inherit = %v, want %v", cli, got.Inherit, want.inherit)
				}
				if got.Reason != want.reason {
					t.Errorf("%s.reason = %q, want %q", cli, got.Reason, want.reason)
				}
				if got.InheritedFrom != nil && want.from == nil {
					t.Errorf("%s.inheritedFrom = %+v, want nil", cli, got.InheritedFrom)
				}
				if want.from != nil && (got.InheritedFrom == nil || *got.InheritedFrom != *want.from) {
					t.Errorf("%s.inheritedFrom = %+v, want %+v", cli, got.InheritedFrom, want.from)
				}
				if len(got.Entries) != want.entryCount {
					t.Errorf("%s.entries = %d rows, want %d", cli, len(got.Entries), want.entryCount)
				}
			}
		})
	}
}
