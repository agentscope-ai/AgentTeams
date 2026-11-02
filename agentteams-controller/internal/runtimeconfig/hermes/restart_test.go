package hermes

import (
	"reflect"
	"testing"
)

func TestRestartKeys(t *testing.T) {
	s := mustSurface(t)

	// Pure next-turn keys: nothing to restart for.
	if got := RestartKeys(s, []string{"delegation.model", "streaming.transport", "model"}); len(got) != 0 {
		t.Errorf("next-turn-only keys: %v, want none", got)
	}
	// Each restart-class key is listed when written.
	if got := RestartKeys(s, []string{"platforms.matrix.token"}); !reflect.DeepEqual(got, []string{"platforms.matrix.token"}) {
		t.Errorf("matrix token: %v, want [platforms.matrix.token]", got)
	}
	if got := RestartKeys(s, []string{"streaming.enabled"}); !reflect.DeepEqual(got, []string{"streaming.enabled"}) {
		t.Errorf("streaming enabled: %v, want [streaming.enabled]", got)
	}
	// A concrete key matching a wildcard pattern counts through the
	// declared entry key - and only on an exact segment-count match
	// (MatchKey: "*" stands for exactly one segment).
	if got := RestartKeys(s, []string{"providers.openrouter.api_key"}); len(got) != 0 {
		t.Errorf("wildcard entry key (next-turn): %v, want none", got)
	}
	if got := RestartKeys(s, []string{"providers.api_key"}); len(got) != 0 {
		t.Errorf("wrong segment count: %v, want none", got)
	}
	// Mixed input keeps input order and skips undeclared keys.
	got := RestartKeys(s, []string{"model", "platforms.matrix.homeserver", "unknown.key", "streaming.enabled"})
	if want := []string{"platforms.matrix.homeserver", "streaming.enabled"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mixed: %v, want %v", got, want)
	}
}
