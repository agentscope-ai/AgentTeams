package hermes

import (
	"reflect"
	"testing"
)

// The allowed key set is exactly the three delegation keys; backend and
// backends are the session's concern and must never be projected.
var allowedDelegationKeys = map[string]bool{
	"delegation.model":              true,
	"delegation.provider":           true,
	"delegation.fallback_providers": true,
}

func TestProjectDelegation(t *testing.T) {
	cases := []struct {
		name string
		in   SubagentModel
		want map[string]any
	}{
		{
			name: "all unset",
			in:   SubagentModel{},
			want: map[string]any{},
		},
		{name: "explicit fallback disabled", in: SubagentModel{Fallback: []map[string]any{}}, want: map[string]any{"delegation.fallback_providers": []map[string]any{}}},
		{
			name: "model only",
			in:   SubagentModel{Model: "claude-sonnet-4"},
			want: map[string]any{"delegation.model": "claude-sonnet-4"},
		},
		{
			name: "provider only",
			in:   SubagentModel{Provider: "openrouter"},
			want: map[string]any{"delegation.provider": "openrouter"},
		},
		{
			name: "model and fallback",
			in:   SubagentModel{Model: "claude-sonnet-4", Fallback: []map[string]any{{"provider": "openai", "model": "gpt-4.1"}, {"provider": "anthropic", "model": "claude-sonnet-4"}}},
			want: map[string]any{
				"delegation.model":              "claude-sonnet-4",
				"delegation.fallback_providers": []map[string]any{{"provider": "openai", "model": "gpt-4.1"}, {"provider": "anthropic", "model": "claude-sonnet-4"}},
			},
		},
		{
			name: "all set",
			in:   SubagentModel{Model: "claude-sonnet-4", Provider: "anthropic", Fallback: []map[string]any{{"provider": "openai", "model": "gpt-4.1"}}},
			want: map[string]any{
				"delegation.model":              "claude-sonnet-4",
				"delegation.provider":           "anthropic",
				"delegation.fallback_providers": []map[string]any{{"provider": "openai", "model": "gpt-4.1"}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProjectDelegation(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ProjectDelegation(%+v) = %v, want %v", c.in, got, c.want)
			}
			// Exact key set: nothing outside the three allowed keys, and
			// fallback never leaks into delegation.model.
			for k, v := range got {
				if !allowedDelegationKeys[k] {
					t.Errorf("projection wrote key outside the allowed set: %q", k)
				}
				if k == "delegation.model" && v != c.in.Model {
					t.Errorf("delegation.model = %v, want %q (fallback must not mix in)", v, c.in.Model)
				}
			}
		})
	}
}
