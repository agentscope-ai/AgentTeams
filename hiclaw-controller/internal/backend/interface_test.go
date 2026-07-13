package backend

import "testing"

func TestResolveRuntime(t *testing.T) {
	cases := []struct {
		name       string
		reqRuntime string
		fallback   string
		want       string
	}{
		{"explicit_request_wins_over_fallback", RuntimeCopaw, RuntimeHermes, RuntimeCopaw},
		{"explicit_over_empty_fallback", RuntimeOpenClaw, "", RuntimeOpenClaw},
		{"empty_uses_fallback_hermes", "", RuntimeHermes, RuntimeHermes},
		{"empty_uses_fallback_copaw", "", RuntimeCopaw, RuntimeCopaw},
		{"empty_uses_fallback_qwenpaw", "", RuntimeQwenPaw, RuntimeQwenPaw},
		{"empty_and_no_fallback_uses_openclaw", "", "", RuntimeOpenClaw},
		{"explicit_openclaw_preserved", RuntimeOpenClaw, RuntimeHermes, RuntimeOpenClaw},
		{"explicit_hermes_preserved", RuntimeHermes, RuntimeCopaw, RuntimeHermes},
		{"explicit_qwenpaw_preserved", RuntimeQwenPaw, RuntimeCopaw, RuntimeQwenPaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveRuntime(tc.reqRuntime, tc.fallback)
			if got != tc.want {
				t.Fatalf("ResolveRuntime(%q, %q) = %q, want %q", tc.reqRuntime, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestValidRuntime(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{RuntimeOpenClaw, true},
		{RuntimeCopaw, true},
		{RuntimeHermes, true},
		{RuntimeQwenPaw, true},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := ValidRuntime(tc.in); got != tc.want {
			t.Fatalf("ValidRuntime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
